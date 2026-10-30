package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/proxy"
)

func fakeDNSResponse(query []byte) []byte {
	response := append([]byte(nil), query...)
	if len(response) < 12 {
		return response
	}
	response[2] |= 0x80 // QR
	response[3] = 0     // NOERROR
	return response
}

func fakeDNSPort(t *testing.T, address string) int {
	t.Helper()
	_, portValue, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	for _, character := range portValue {
		port = port*10 + int(character-'0')
	}
	return port
}

func startFakeUDPDNSServer(t *testing.T, transform func([]byte) []byte) (ProxyDNSEndpoint, func()) {
	t.Helper()
	connection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			read, source, readErr := connection.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			query := append([]byte(nil), buffer[:read]...)
			response := transform(query)
			if len(response) == 0 {
				continue
			}
			_, _ = connection.WriteTo(response, source)
		}
	}()
	endpoint := ProxyDNSEndpoint{
		ID:            "px-udp",
		ProxyEgressID: "proxy-media",
		UpstreamID:    "dns-proxy",
		Protocol:      proxy.ProxyDNSEndpointUDP,
		ListenAddress: "127.0.0.1",
		Port:          fakeDNSPort(t, connection.LocalAddr().String()),
	}
	return endpoint, func() {
		connection.Close()
		<-done
	}
}

func startFakeTCPDNSServer(t *testing.T, transform func([]byte) []byte) (ProxyDNSEndpoint, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				header := make([]byte, 2)
				if _, readErr := io.ReadFull(connection, header); readErr != nil {
					return
				}
				length := int(binary.BigEndian.Uint16(header))
				query := make([]byte, length)
				if _, readErr := io.ReadFull(connection, query); readErr != nil {
					return
				}
				response := transform(query)
				if len(response) == 0 {
					return
				}
				framed := make([]byte, 2+len(response))
				binary.BigEndian.PutUint16(framed, uint16(len(response)))
				copy(framed[2:], response)
				_, _ = connection.Write(framed)
			}(connection)
		}
	}()
	endpoint := ProxyDNSEndpoint{
		ID:            "px-tcp",
		ProxyEgressID: "proxy-media",
		UpstreamID:    "dns-proxy",
		Protocol:      proxy.ProxyDNSEndpointTCP,
		ListenAddress: "127.0.0.1",
		Port:          fakeDNSPort(t, listener.Addr().String()),
	}
	return endpoint, func() {
		listener.Close()
		<-done
	}
}

func TestProxyDNSEndpointProbeUDP(t *testing.T) {
	endpoint, stop := startFakeUDPDNSServer(t, fakeDNSResponse)
	defer stop()
	prober := ProxyDNSEndpointProber{Timeout: time.Second}
	if err := prober.Probe(context.Background(), endpoint); err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	badID, stopBad := startFakeUDPDNSServer(t, func(query []byte) []byte {
		response := fakeDNSResponse(query)
		response[0] ^= 0xff
		return response
	})
	defer stopBad()
	if err := prober.Probe(context.Background(), badID); err == nil || !strings.Contains(err.Error(), "id does not match") {
		t.Fatalf("mismatched response error = %v", err)
	}

	silent, stopSilent := startFakeUDPDNSServer(t, func([]byte) []byte { return nil })
	defer stopSilent()
	short := ProxyDNSEndpointProber{Timeout: 300 * time.Millisecond}
	if err := short.Probe(context.Background(), silent); err == nil || !strings.Contains(err.Error(), "received no answer") {
		t.Fatalf("silent endpoint error = %v", err)
	}
}

func TestProxyDNSEndpointProbeTCP(t *testing.T) {
	endpoint, stop := startFakeTCPDNSServer(t, fakeDNSResponse)
	defer stop()
	prober := ProxyDNSEndpointProber{Timeout: time.Second}
	if err := prober.Probe(context.Background(), endpoint); err != nil {
		t.Fatalf("TCP probe failed: %v", err)
	}

	servfail, stopServfail := startFakeTCPDNSServer(t, func(query []byte) []byte {
		response := fakeDNSResponse(query)
		response[3] = 2 // SERVFAIL
		return response
	})
	defer stopServfail()
	if err := prober.Probe(context.Background(), servfail); err == nil || !strings.Contains(err.Error(), "rcode 2") {
		t.Fatalf("SERVFAIL probe error = %v", err)
	}
}

func TestProxyDNSEndpointProbeClosedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fakeDNSPort(t, listener.Addr().String())
	listener.Close()
	endpoint := ProxyDNSEndpoint{
		ID:            "px-closed",
		Protocol:      proxy.ProxyDNSEndpointTCP,
		ListenAddress: "127.0.0.1",
		Port:          port,
	}
	prober := ProxyDNSEndpointProber{Timeout: 300 * time.Millisecond}
	if err := prober.Probe(context.Background(), endpoint); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("closed port error = %v", err)
	}
}

func TestProxyDNSEndpointProbeDoH(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dns-query" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "wrong content type", http.StatusBadRequest)
			return
		}
		query, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(fakeDNSResponse(query))
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	endpoint := ProxyDNSEndpoint{
		ID:            "px-doh",
		Protocol:      proxy.ProxyDNSEndpointDoH,
		ListenAddress: "127.0.0.1",
		Port:          fakeDNSPort(t, strings.TrimPrefix(server.URL, "https://")),
		ServerName:    "example.com",
	}
	prober := ProxyDNSEndpointProber{Timeout: 2 * time.Second, TLSRootCAs: roots}
	if err := prober.Probe(context.Background(), endpoint); err != nil {
		t.Fatalf("DoH probe failed: %v", err)
	}

	// Wrong SNI must fail: the probe has to verify the certificate chain.
	mismatched := endpoint
	mismatched.ServerName = "wrong.example"
	mismatched.ID = "px-doh-wrong"
	if err := prober.Probe(context.Background(), mismatched); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("SNI mismatch error = %v", err)
	}
}

func TestProxyDNSEndpointProbeDoT(t *testing.T) {
	certificate, roots := selfSignedProxyDNSCertificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				header := make([]byte, 2)
				if _, readErr := io.ReadFull(connection, header); readErr != nil {
					return
				}
				query := make([]byte, int(binary.BigEndian.Uint16(header)))
				if _, readErr := io.ReadFull(connection, query); readErr != nil {
					return
				}
				response := fakeDNSResponse(query)
				framed := make([]byte, 2+len(response))
				binary.BigEndian.PutUint16(framed, uint16(len(response)))
				copy(framed[2:], response)
				_, _ = connection.Write(framed)
			}(connection)
		}
	}()
	endpoint := ProxyDNSEndpoint{
		ID:            "px-dot",
		Protocol:      proxy.ProxyDNSEndpointDoT,
		ListenAddress: "127.0.0.1",
		Port:          fakeDNSPort(t, listener.Addr().String()),
		ServerName:    "dot.example",
	}
	prober := ProxyDNSEndpointProber{Timeout: 2 * time.Second, TLSRootCAs: roots}
	if err := prober.Probe(context.Background(), endpoint); err != nil {
		t.Fatalf("DoT probe failed: %v", err)
	}
}

func selfSignedProxyDNSCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dot.example"},
		DNSNames:              []string{"dot.example"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	_ = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certificate, roots
}

func TestBuildProxyDNSProbeQueryRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"", "..", strings.Repeat("a", 64) + ".example"} {
		if _, _, err := buildProxyDNSProbeQuery(name); err == nil {
			t.Fatalf("query name %q was accepted", name)
		}
	}
	query, id, err := buildProxyDNSProbeQuery("media.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(query) < 12 || binary.BigEndian.Uint16(query[0:2]) != id {
		t.Fatalf("query header = %#v", query)
	}
}
