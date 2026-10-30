package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dns"
	"ly-route/backend/internal/runtime/proxy"
)

// Real-Xray validation of the proxy DNS endpoint.
//
// The tests in this file need the shipped binary:
//
//	LY_ROUTE_XRAY_BINARY=/root/work/rootfs-prod/usr/bin/xray \
//	  go test ./internal/runtime/service -run 'RealXray' -count=1 -v
//
// They are skipped when the variable is unset, so a checkout without the
// appliance rootfs still builds and tests normally.
//
// What is proven here: the generated JSON is accepted by the real binary, the
// loopback endpoint forwards a query through the *proxy outbound* (a local
// SOCKS5 server stands in for the proxy node), the plain/TCP/DoH/DoT transports
// work, a listening-port conflict prevents Xray from starting, and an
// unreachable upstream produces no answer (fail closed) instead of a direct
// resolution.
//
// What is NOT proven here (see docs/DNS-PROXY-SEMANTICS.md §5): the egress
// fwmark path into the VPP service chain, carrier blocking of plaintext :53,
// reachability of real public DoH/DoT servers, and SmartDNS itself.
const (
	realXrayEnv = "LY_ROUTE_XRAY_BINARY"

	// Non-routable destinations. The fake SOCKS server maps them onto local
	// test servers, so a successful query can only have gone through the proxy
	// outbound.
	realXrayUDPTarget = "203.0.113.53"
	realXrayDoHTarget = "203.0.113.54"
	realXrayDoTTarget = "203.0.113.55"
	realXraySocksTag  = "proxy-egress"
)

func realXrayBinary(t *testing.T) string {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv(realXrayEnv))
	if binary == "" {
		t.Skip(realXrayEnv + " is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("%s does not exist: %v", binary, err)
	}
	return binary
}

// realXrayConfigForTest renders the product config and swaps the compiled
// proxy outbound for a SOCKS5 outbound tagged exactly like the product's
// egress. The DNS inbounds and their routing rules are untouched, so the
// generated endpoint is what the real binary executes.
func realXrayConfigForTest(t *testing.T, plan ProxyDNSEndpointPlan, socksAddress string, keepCaptureInbound bool) string {
	t.Helper()
	compiledProxy, err := proxy.CompileEgress(proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := RenderXrayWithProxyDNS(compiledProxy, plan)
	if err != nil {
		t.Fatal(err)
	}
	var payload proxy.XrayConfigPayload
	if err := json.Unmarshal([]byte(artifacts[0].Content), &payload); err != nil {
		t.Fatal(err)
	}
	host, portValue, err := net.SplitHostPort(socksAddress)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	for _, character := range portValue {
		port = port*10 + int(character-'0')
	}
	payload.Outbounds = []proxy.XrayOutbound{{
		Tag:      realXraySocksTag,
		Protocol: "socks",
		Settings: map[string]any{"servers": []any{map[string]any{"address": host, "port": port}}},
	}}
	if !keepCaptureInbound {
		kept := make([]proxy.XrayInbound, 0, len(payload.Inbounds))
		for _, inbound := range payload.Inbounds {
			if strings.HasPrefix(inbound.Tag, proxy.ProxyDNSEndpointTagPrefix) {
				kept = append(kept, inbound)
			}
		}
		payload.Inbounds = kept
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func writeRealXrayConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "xray-proxy-dns.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRealXrayAcceptsProxyDNSConfigs runs the binary's own config validator on
// every endpoint shape the product can render.
func TestRealXrayAcceptsProxyDNSConfigs(t *testing.T) {
	binary := realXrayBinary(t)
	output, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("xray version failed: %v: %s", err, output)
	}
	t.Logf("xray: %s", strings.TrimSpace(string(output)))

	upstreams := []SmartDNSUpstream{
		{ID: "dns-udp", Servers: []string{realXrayUDPTarget}},
		{ID: "dns-doh", Servers: []string{"https://doh.test/dns-query"}, ResolvedHostIPs: map[string][]string{"doh.test": {realXrayDoHTarget}}},
		{ID: "dns-dot", Servers: []string{"tls://dot.test:853"}, ResolvedHostIPs: map[string][]string{"dot.test": {realXrayDoTTarget}}},
		{ID: "dns-fallback", Servers: []string{"198.51.100.99"}},
	}
	bindings := []dns.ProxyDNSBinding{
		{RuleID: "plain", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp", FailureMode: dns.FailureModeFailClosed},
		{RuleID: "doh", ProxyEgressID: "proxy-media", UpstreamID: "dns-doh", FailureMode: dns.FailureModeFailClosed},
		{RuleID: "dot", ProxyEgressID: "proxy-media", UpstreamID: "dns-dot", FailureMode: dns.FailureModeReject, FallbackUpstreamID: "dns-fallback"},
		{RuleID: "miss-proxy", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"},
	}
	plan := PlanProxyDNSEndpoints(bindings, upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 3 {
		t.Fatalf("plan = %#v", plan)
	}

	for name, keepCapture := range map[string]bool{"full-product-shape": true, "dns-only": false} {
		content := realXrayConfigForTest(t, plan, "127.0.0.1:1080", keepCapture)
		path := writeRealXrayConfig(t, content)
		output, err := exec.Command(binary, "run", "-test", "-config", path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: real xray rejected the generated config: %v\n%s\n--- config ---\n%s", name, err, output, content)
		}
		if strings.TrimSpace(string(output)) != "" {
			t.Logf("%s: xray -test output: %s", name, strings.TrimSpace(string(output)))
		}
	}
}

func TestRealXrayRejectsDuplicateProxyDNSPort(t *testing.T) {
	binary := realXrayBinary(t)
	upstreams := []SmartDNSUpstream{{ID: "dns-udp", Servers: []string{realXrayUDPTarget}}}
	plan := PlanProxyDNSEndpoints([]dns.ProxyDNSBinding{{RuleID: "plain", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"}}, upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	// Occupy the planned port so Xray cannot bind it.
	blocker, err := net.Listen("tcp", plan.Endpoints[0].Listen())
	if err != nil {
		t.Fatalf("reserve endpoint port: %v", err)
	}
	defer blocker.Close()

	content := realXrayConfigForTest(t, plan, "127.0.0.1:1080", false)
	path := writeRealXrayConfig(t, content)
	command := exec.Command(binary, "run", "-config", path)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("xray started with a taken proxy DNS port; output:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "address already in use") &&
		!strings.Contains(strings.ToLower(string(output)), "bind") &&
		!strings.Contains(strings.ToLower(string(output)), "listen") {
		t.Fatalf("xray failed for an unexpected reason: %v\n%s", err, output)
	}
	t.Logf("xray port-conflict output: %s", strings.TrimSpace(string(output)))
}

// ---------------------------------------------------------------- fake SOCKS

// fakeSocksServer is a minimal SOCKS5 server with CONNECT and UDP ASSOCIATE.
// Destinations are mapped by port onto local test servers: this is what makes a
// successful DNS answer proof that the query reached the proxy outbound (the
// configured destination is a non-routable TEST-NET-3 address).
type fakeSocksServer struct {
	listener net.Listener
	udp      net.PacketConn
	portMap  map[int]string

	mu       sync.Mutex
	observed []string
}

func startFakeSocksServer(t *testing.T, portMap map[int]string) *fakeSocksServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &fakeSocksServer{listener: listener, udp: udp, portMap: portMap}
	go server.acceptLoop()
	go server.udpLoop()
	t.Cleanup(server.Close)
	return server
}

func (server *fakeSocksServer) Address() string {
	return server.listener.Addr().String()
}

func (server *fakeSocksServer) Observed() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.observed...)
}

func (server *fakeSocksServer) record(destination string) {
	server.mu.Lock()
	server.observed = append(server.observed, destination)
	server.mu.Unlock()
}

func (server *fakeSocksServer) Close() {
	server.listener.Close()
	server.udp.Close()
}

func (server *fakeSocksServer) local(port int) (string, bool) {
	address, exists := server.portMap[port]
	return address, exists
}

func (server *fakeSocksServer) acceptLoop() {
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		go server.handle(connection)
	}
}

func (server *fakeSocksServer) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(connection, greeting); err != nil || greeting[0] != 5 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(connection, methods); err != nil {
		return
	}
	if _, err := connection.Write([]byte{5, 0}); err != nil {
		return
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil || header[0] != 5 {
		return
	}
	if header[3] != 1 {
		// Only IPv4 destinations are used by the test configs.
		return
	}
	address := make([]byte, 4)
	if _, err := io.ReadFull(connection, address); err != nil {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(connection, portBytes); err != nil {
		return
	}
	destinationPort := int(binary.BigEndian.Uint16(portBytes))
	destination := fmt.Sprintf("%s:%d", net.IP(address), destinationPort)
	server.record(destination)

	switch header[1] {
	case 1: // CONNECT
		local, ok := server.local(destinationPort)
		if !ok {
			_, _ = connection.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		upstream, err := net.Dial("tcp", local)
		if err != nil {
			_, _ = connection.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer upstream.Close()
		_ = connection.SetDeadline(time.Time{})
		if _, err := connection.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(upstream, connection)
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(connection, upstream)
			done <- struct{}{}
		}()
		<-done
	case 3: // UDP ASSOCIATE
		udpAddress, ok := server.udp.LocalAddr().(*net.UDPAddr)
		if !ok {
			return
		}
		reply := []byte{5, 0, 0, 1}
		reply = append(reply, udpAddress.IP.To4()...)
		portReply := make([]byte, 2)
		binary.BigEndian.PutUint16(portReply, uint16(udpAddress.Port))
		reply = append(reply, portReply...)
		if _, err := connection.Write(reply); err != nil {
			return
		}
		_ = connection.SetDeadline(time.Time{})
		_, _ = io.Copy(io.Discard, connection)
	default:
		_, _ = connection.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
	}
}

func (server *fakeSocksServer) udpLoop() {
	buffer := make([]byte, 65535)
	for {
		read, client, err := server.udp.ReadFrom(buffer)
		if err != nil {
			return
		}
		if read < 10 {
			continue
		}
		packet := append([]byte(nil), buffer[:read]...)
		// RSV(2) FRAG(1) ATYP(1) ADDR(4) PORT(2) DATA
		if packet[3] != 1 {
			continue
		}
		destinationPort := int(binary.BigEndian.Uint16(packet[8:10]))
		destination := fmt.Sprintf("%s:%d", net.IP(packet[4:8]), destinationPort)
		server.record(destination)
		local, ok := server.local(destinationPort)
		if !ok {
			continue
		}
		payload := packet[10:]
		go func(payload []byte, client net.Addr, header []byte) {
			upstream, dialErr := net.Dial("udp", local)
			if dialErr != nil {
				return
			}
			defer upstream.Close()
			_ = upstream.SetDeadline(time.Now().Add(2 * time.Second))
			if _, writeErr := upstream.Write(payload); writeErr != nil {
				return
			}
			reply := make([]byte, 4096)
			count, readErr := upstream.Read(reply)
			if readErr != nil {
				return
			}
			framed := append(append([]byte(nil), header...), reply[:count]...)
			_, _ = server.udp.WriteTo(framed, client)
		}(append([]byte(nil), payload...), client, append([]byte(nil), packet[:10]...))
	}
}

// ------------------------------------------------------------- test servers

// realXrayTLSCertificate creates a self-signed certificate for the given DNS
// name. Test-only: the product never disables verification.
func realXrayTLSCertificate(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func startRealXrayDoHDNSServer(t *testing.T, name string) (string, *x509.CertPool) {
	t.Helper()
	certificate, roots := realXrayTLSCertificate(t, name)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dns-query" {
			http.NotFound(w, r)
			return
		}
		query, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 9)))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "https://"), roots
}

func startRealXrayDoTDNSServer(t *testing.T, name string) (string, *x509.CertPool) {
	t.Helper()
	certificate, roots := realXrayTLSCertificate(t, name)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				for {
					header := make([]byte, 2)
					if _, readErr := io.ReadFull(connection, header); readErr != nil {
						return
					}
					query := make([]byte, int(binary.BigEndian.Uint16(header)))
					if _, readErr := io.ReadFull(connection, query); readErr != nil {
						return
					}
					response := fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 8))
					framed := make([]byte, 2+len(response))
					binary.BigEndian.PutUint16(framed, uint16(len(response)))
					copy(framed[2:], response)
					if _, writeErr := connection.Write(framed); writeErr != nil {
						return
					}
				}
			}(connection)
		}
	}()
	return listener.Addr().String(), roots
}

// --------------------------------------------------------- running the binary

type realXrayProcess struct {
	command *exec.Cmd
	output  *lockedBuffer
	done    chan error
	stopped sync.Once
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func startRealXrayProcess(t *testing.T, binary, configPath string) *realXrayProcess {
	t.Helper()
	command := exec.Command(binary, "run", "-config", configPath)
	output := &lockedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start xray: %v", err)
	}
	process := &realXrayProcess{command: command, output: output, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(process.Stop)
	return process
}

func (process *realXrayProcess) Stop() {
	process.stopped.Do(func() {
		_ = process.command.Process.Kill()
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func (process *realXrayProcess) Wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-process.done:
		t.Fatalf("xray exited unexpectedly: %v\n%s", err, process.output.String())
	case <-time.After(50 * time.Millisecond):
	}
}

func (process *realXrayProcess) WaitReady(t *testing.T, prober ProxyDNSEndpointProber, endpoint ProxyDNSEndpoint) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			t.Fatalf("xray exited before the endpoint became ready: %v\n%s", err, process.output.String())
		default:
		}
		last = prober.Probe(context.Background(), endpoint)
		if last == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("endpoint %s never answered: %v\nxray output:\n%s", endpoint.Listen(), last, process.output.String())
}

func TestRealXrayForwardsProxyDNSQueries(t *testing.T) {
	binary := realXrayBinary(t)

	// Plain UDP DNS.
	var plainQueries []string
	var plainMu sync.Mutex
	upstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		plainMu.Lock()
		plainQueries = append(plainQueries, decodeDNSQuestionName(query))
		plainMu.Unlock()
		return fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 7))
	})

	// DoH and DoT with local self-signed TLS.
	dohAddress, dohRoots := startRealXrayDoHDNSServer(t, "doh.test")
	dotAddress, dotRoots := startRealXrayDoTDNSServer(t, "dot.test")

	upstreams := []SmartDNSUpstream{
		{ID: "dns-udp", Servers: []string{realXrayUDPTarget}},
		{ID: "dns-doh", Servers: []string{"https://doh.test/dns-query"}, ResolvedHostIPs: map[string][]string{"doh.test": {realXrayDoHTarget}}},
		{ID: "dns-dot", Servers: []string{"tls://dot.test:853"}, ResolvedHostIPs: map[string][]string{"dot.test": {realXrayDoTTarget}}},
	}
	bindings := []dns.ProxyDNSBinding{
		{RuleID: "plain", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"},
		{RuleID: "doh", ProxyEgressID: "proxy-media", UpstreamID: "dns-doh"},
		{RuleID: "dot", ProxyEgressID: "proxy-media", UpstreamID: "dns-dot"},
	}
	plan := PlanProxyDNSEndpoints(bindings, upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 3 {
		t.Fatalf("plan = %#v", plan)
	}

	socks := startFakeSocksServer(t, map[int]string{53: upstreamAddress, 443: dohAddress, 853: dotAddress})
	configPath := writeRealXrayConfig(t, realXrayConfigForTest(t, plan, socks.Address(), true))
	process := startRealXrayProcess(t, binary, configPath)

	prober := ProxyDNSEndpointProber{Timeout: 2 * time.Second}
	for _, endpoint := range plan.Endpoints {
		probe := prober
		switch endpoint.Protocol {
		case proxy.ProxyDNSEndpointDoH:
			probe.TLSRootCAs = dohRoots
		case proxy.ProxyDNSEndpointDoT:
			probe.TLSRootCAs = dotRoots
		}
		process.WaitReady(t, probe, endpoint)
	}

	plainMu.Lock()
	observed := append([]string(nil), plainQueries...)
	plainMu.Unlock()
	if len(observed) == 0 || observed[len(observed)-1] != "example.com" {
		t.Fatalf("plain upstream queries = %#v", observed)
	}
	destinations := strings.Join(socks.Observed(), ",")
	for _, want := range []string{
		realXrayUDPTarget + ":53",
		realXrayDoHTarget + ":443",
		realXrayDoTTarget + ":853",
	} {
		if !strings.Contains(destinations, want) {
			t.Fatalf("proxy outbound did not dial %q; observed %q", want, destinations)
		}
	}

	// SNI verification still happens end to end: the same endpoint must fail
	// with a certificate name that does not match the TLS server.
	for _, endpoint := range plan.Endpoints {
		if endpoint.Protocol != proxy.ProxyDNSEndpointDoH && endpoint.Protocol != proxy.ProxyDNSEndpointDoT {
			continue
		}
		mismatched := endpoint
		mismatched.ServerName = "wrong.test"
		mismatched.ID += "-sni"
		if err := prober.Probe(context.Background(), mismatched); err == nil {
			t.Fatalf("%s endpoint accepted a mismatched SNI", endpoint.Protocol)
		}
	}
	process.Wait(t)
}

func TestRealXrayProxyDNSEndpointFailsClosedWhenUpstreamIsUnreachable(t *testing.T) {
	binary := realXrayBinary(t)
	upstreams := []SmartDNSUpstream{{ID: "dns-udp", Servers: []string{realXrayUDPTarget}}}
	plan := PlanProxyDNSEndpoints([]dns.ProxyDNSBinding{{RuleID: "plain", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"}}, upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	// The SOCKS server has no mapping for port 53, so the proxy outbound fails
	// to reach the upstream. fail_closed must produce no answer.
	socks := startFakeSocksServer(t, map[int]string{})
	configPath := writeRealXrayConfig(t, realXrayConfigForTest(t, plan, socks.Address(), false))
	process := startRealXrayProcess(t, binary, configPath)

	// Wait for the listener itself (TCP port check) without requiring an answer.
	listenDeadline := time.Now().Add(5 * time.Second)
	for {
		if connection, err := net.Dial("tcp", plan.Endpoints[0].Listen()); err == nil {
			connection.Close()
			break
		}
		if time.Now().After(listenDeadline) {
			t.Fatalf("endpoint never listened:\n%s", process.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	prober := ProxyDNSEndpointProber{Timeout: 400 * time.Millisecond}
	started := time.Now()
	err := prober.Probe(context.Background(), plan.Endpoints[0])
	if err == nil {
		t.Fatalf("endpoint answered although the proxy outbound could not reach the upstream; socks observed %v", socks.Observed())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("probe took %s; the failure must surface as a timeout, not a hang", elapsed)
	}
	if !strings.Contains(err.Error(), "no answer") && !strings.Contains(err.Error(), "no answer:") {
		t.Logf("probe error (informational): %v", err)
	}
	if len(socks.Observed()) == 0 {
		t.Fatalf("xray never used the proxy outbound; it may have dialled directly")
	}
	process.Wait(t)
}
