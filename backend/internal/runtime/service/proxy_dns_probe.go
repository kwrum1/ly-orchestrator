package service

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/proxy"
)

// Proxy DNS endpoint health.
//
// Unit state is not enough for a local endpoint: Xray can be active while the
// inbound was rejected, the egress route is missing, or the upstream silently
// drops the query. The probe therefore checks both the port and one real DNS
// round trip through the endpoint. It never reports success for a listening
// socket that cannot answer.
const (
	// ProxyDNSEndpointProbeQuery is a stable, cache-friendly A query. The
	// answer value is irrelevant; the round trip and the response code are.
	ProxyDNSEndpointProbeQuery = "example.com"
	// ProxyDNSEndpointProbeTimeout stays below the SmartDNS client budget so a
	// dead endpoint is reported instead of looking like a slow resolver.
	ProxyDNSEndpointProbeTimeout = 2 * time.Second
)

// ProxyDNSEndpointProber issues one real query against a local endpoint.
type ProxyDNSEndpointProber struct {
	Timeout time.Duration
	// QueryName overrides the probe question (tests and diagnostics).
	QueryName string
	// TLSRootCAs replaces the system pool for DoH/DoT probes. Tests use it;
	// production keeps the system pool so a MITM cannot fake a healthy path.
	TLSRootCAs *x509.CertPool
}

// Probe checks the endpoint port and performs one DNS query. The returned
// error is operator-facing and names the transport that failed.
func (prober ProxyDNSEndpointProber) Probe(ctx context.Context, endpoint ProxyDNSEndpoint) error {
	timeout := prober.Timeout
	if timeout <= 0 {
		timeout = ProxyDNSEndpointProbeTimeout
	}
	queryName := strings.TrimSpace(prober.QueryName)
	if queryName == "" {
		queryName = ProxyDNSEndpointProbeQuery
	}
	query, id, err := buildProxyDNSProbeQuery(queryName)
	if err != nil {
		return err
	}
	address := endpoint.Listen()
	if strings.TrimSpace(address) == ":" || endpoint.Port <= 0 {
		return fmt.Errorf("proxy DNS endpoint %q has no listening address", endpoint.ID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch endpoint.Protocol {
	case proxy.ProxyDNSEndpointUDP:
		response, err := probeProxyDNSUDP(probeCtx, address, query)
		if err != nil {
			return err
		}
		return validateProxyDNSProbeResponse(response, id, "UDP")
	case proxy.ProxyDNSEndpointTCP:
		response, err := probeProxyDNSTCP(probeCtx, address, query)
		if err != nil {
			return err
		}
		return validateProxyDNSProbeResponse(response, id, "TCP")
	case proxy.ProxyDNSEndpointDoT:
		response, err := probeProxyDNSDoT(probeCtx, address, endpoint.ServerName, prober.TLSRootCAs, query)
		if err != nil {
			return err
		}
		return validateProxyDNSProbeResponse(response, id, "DoT")
	case proxy.ProxyDNSEndpointDoH:
		response, err := probeProxyDNSDoH(probeCtx, address, endpoint.ServerName, prober.TLSRootCAs, query)
		if err != nil {
			return err
		}
		return validateProxyDNSProbeResponse(response, id, "DoH")
	default:
		return fmt.Errorf("proxy DNS endpoint %q uses unsupported protocol %q", endpoint.ID, endpoint.Protocol)
	}
}

func probeProxyDNSUDP(ctx context.Context, address string, query []byte) ([]byte, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, fmt.Errorf("UDP port %s is not reachable: %w", address, err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if _, err := connection.Write(query); err != nil {
		return nil, fmt.Errorf("UDP probe write to %s failed: %w", address, err)
	}
	buffer := make([]byte, 4096)
	read, err := connection.Read(buffer)
	if err != nil {
		return nil, fmt.Errorf("UDP probe query to %s received no answer: %w", address, err)
	}
	return buffer[:read], nil
}

func probeProxyDNSTCP(ctx context.Context, address string, query []byte) ([]byte, error) {
	connection, err := dialProxyDNSEndpoint(ctx, address)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	return exchangeDNSOverStream(connection, query)
}

func probeProxyDNSDoT(ctx context.Context, address, serverName string, roots *x509.CertPool, query []byte) ([]byte, error) {
	raw, err := dialProxyDNSEndpoint(ctx, address)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	connection := tls.Client(raw, &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := connection.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("DoT TLS handshake with %s (SNI %q) failed: %w", address, serverName, err)
	}
	return exchangeDNSOverStream(connection, query)
}

func probeProxyDNSDoH(ctx context.Context, address, serverName string, roots *x509.CertPool, query []byte) ([]byte, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialProxyDNSEndpoint(ctx, address)
		},
		TLSClientConfig: &tls.Config{
			ServerName: serverName,
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+"/dns-query", strings.NewReader(string(query)))
	if err != nil {
		return nil, fmt.Errorf("DoH probe request for %s could not be built: %w", address, err)
	}
	request.Header.Set("Content-Type", "application/dns-message")
	request.Header.Set("Accept", "application/dns-message")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("DoH probe to %s (SNI %q) failed: %w", address, serverName, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH probe to %s returned HTTP %d", address, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("DoH probe to %s could not be read: %w", address, err)
	}
	return body, nil
}

func dialProxyDNSEndpoint(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("TCP port %s is not reachable: %w", address, err)
	}
	return connection, nil
}

func exchangeDNSOverStream(connection net.Conn, query []byte) ([]byte, error) {
	framed := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(framed, uint16(len(query)))
	copy(framed[2:], query)
	if _, err := connection.Write(framed); err != nil {
		return nil, fmt.Errorf("DNS stream write failed: %w", err)
	}
	header := make([]byte, 2)
	if _, err := io.ReadFull(connection, header); err != nil {
		return nil, fmt.Errorf("DNS stream query received no answer: %w", err)
	}
	length := int(binary.BigEndian.Uint16(header))
	if length < 12 || length > 4096 {
		return nil, fmt.Errorf("DNS stream response length %d is invalid", length)
	}
	response := make([]byte, length)
	if _, err := io.ReadFull(connection, response); err != nil {
		return nil, fmt.Errorf("DNS stream response was truncated: %w", err)
	}
	return response, nil
}

// buildProxyDNSProbeQuery encodes one A/IN query with a random message id.
func buildProxyDNSProbeQuery(name string) ([]byte, uint16, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return nil, 0, errors.New("proxy DNS probe query name is empty")
	}
	labels := strings.Split(name, ".")
	if len(name) > 253 {
		return nil, 0, fmt.Errorf("proxy DNS probe query name %q is too long", name)
	}
	message := make([]byte, 12, 12+len(name)+6)
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, 0, fmt.Errorf("proxy DNS probe id could not be generated: %w", err)
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	binary.BigEndian.PutUint16(message[0:2], id)
	message[2] = 0x01 // recursion desired
	binary.BigEndian.PutUint16(message[4:6], 1)
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return nil, 0, fmt.Errorf("proxy DNS probe query name %q has an invalid label", name)
		}
		message = append(message, byte(len(label)))
		message = append(message, label...)
	}
	message = append(message, 0)
	message = append(message, 0, 1, 0, 1) // QTYPE A, QCLASS IN
	return message, id, nil
}

func validateProxyDNSProbeResponse(response []byte, id uint16, transport string) error {
	if len(response) < 12 {
		return fmt.Errorf("%s probe response is too short (%d bytes)", transport, len(response))
	}
	if binary.BigEndian.Uint16(response[0:2]) != id {
		return fmt.Errorf("%s probe response id does not match the query", transport)
	}
	if response[2]&0x80 == 0 {
		return fmt.Errorf("%s probe response is not marked as a response", transport)
	}
	if rcode := response[3] & 0x0f; rcode != 0 {
		return fmt.Errorf("%s probe response returned rcode %d", transport, rcode)
	}
	return nil
}
