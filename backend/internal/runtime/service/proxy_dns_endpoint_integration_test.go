package service

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dns"
	"ly-route/backend/internal/runtime/proxy"
)

// End-to-end proxy DNS endpoint test without a real Xray binary.
//
// The test binary re-executes itself as a fake Xray process: it reads the
// rendered Xray config and opens exactly the loopback inbounds the product
// rendered, forwarding each inbound to its configured target. That proves the
// generated config is self-consistent (listener, network, fixed target) and
// that SmartDNS really reaches an upstream through the endpoint. It is *not* a
// substitute for a real Xray run on the appliance (see the "device
// verification" section of docs/DNS-PROXY-SEMANTICS.md).
const fakeXrayHelperEnv = "LY_ROUTE_TEST_FAKE_XRAY_CONFIG"

func TestFakeXrayHelperProcess(t *testing.T) {
	configPath := strings.TrimSpace(os.Getenv(fakeXrayHelperEnv))
	if configPath == "" {
		t.Skip("helper process only")
	}
	runFakeXrayProcess(configPath)
	os.Exit(0)
}

func runFakeXrayProcess(configPath string) {
	payload, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake xray:", err)
		return
	}
	var config proxy.XrayConfigPayload
	if err := json.Unmarshal(payload, &config); err != nil {
		fmt.Fprintln(os.Stderr, "fake xray:", err)
		return
	}
	var wait sync.WaitGroup
	for _, inbound := range config.Inbounds {
		if !strings.HasPrefix(inbound.Tag, proxy.ProxyDNSEndpointTagPrefix) {
			continue
		}
		listen := net.JoinHostPort(inbound.Listen, fmt.Sprintf("%d", inbound.Port))
		target := net.JoinHostPort(inbound.Settings.Address, fmt.Sprintf("%d", inbound.Settings.Port))
		if strings.Contains(inbound.Settings.Network, "udp") {
			connection, listenErr := net.ListenPacket("udp", listen)
			if listenErr != nil {
				fmt.Fprintln(os.Stderr, "fake xray udp:", listenErr)
				continue
			}
			wait.Add(1)
			go func(connection net.PacketConn, target string) {
				defer wait.Done()
				defer connection.Close()
				buffer := make([]byte, 4096)
				for {
					read, source, readErr := connection.ReadFrom(buffer)
					if readErr != nil {
						return
					}
					query := append([]byte(nil), buffer[:read]...)
					go func(query []byte, source net.Addr) {
						upstream, dialErr := net.Dial("udp", target)
						if dialErr != nil {
							return
						}
						defer upstream.Close()
						_ = upstream.SetDeadline(time.Now().Add(2 * time.Second))
						if _, writeErr := upstream.Write(query); writeErr != nil {
							return
						}
						response := make([]byte, 4096)
						count, readErr := upstream.Read(response)
						if readErr != nil {
							return
						}
						_, _ = connection.WriteTo(response[:count], source)
					}(query, source)
				}
			}(connection, target)
		}
		if strings.Contains(inbound.Settings.Network, "tcp") {
			listener, listenErr := net.Listen("tcp", listen)
			if listenErr != nil {
				fmt.Fprintln(os.Stderr, "fake xray tcp:", listenErr)
				continue
			}
			wait.Add(1)
			go func(listener net.Listener, target string) {
				defer wait.Done()
				defer listener.Close()
				for {
					connection, acceptErr := listener.Accept()
					if acceptErr != nil {
						return
					}
					go func(connection net.Conn) {
						defer connection.Close()
						upstream, dialErr := net.Dial("tcp", target)
						if dialErr != nil {
							return
						}
						defer upstream.Close()
						_, _ = io.Copy(upstream, connection)
					}(connection)
				}
			}(listener, target)
		}
	}
	wait.Wait()
}

// startFakeXray writes the rendered Xray artifacts to disk and starts the fake
// process. It returns a stop function.
func startFakeXray(t *testing.T, artifacts []RenderedArtifact) func() {
	t.Helper()
	if len(artifacts) != 1 {
		t.Fatalf("xray artifacts = %#v", artifacts)
	}
	path := t.TempDir() + "/xray.json"
	if err := os.WriteFile(path, []byte(artifacts[0].Content), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestFakeXrayHelperProcess")
	command.Env = append(os.Environ(), fakeXrayHelperEnv+"="+path)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
		}
	}()
	stopped := false
	var stopMu sync.Mutex
	stop := func() {
		stopMu.Lock()
		defer stopMu.Unlock()
		if stopped {
			return
		}
		stopped = true
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	t.Cleanup(stop)
	return stop
}

func waitForProxyDNSEndpoint(t *testing.T, prober ProxyDNSEndpointProber, endpoint ProxyDNSEndpoint) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = prober.Probe(context.Background(), endpoint)
		if last == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy DNS endpoint %s never answered: %v", endpoint.Listen(), last)
}

// localRoutableIPv4 returns a non-loopback IPv4 of the test host. The endpoint
// validator deliberately refuses loopback targets (a loopback resolver would
// recurse into the local SmartDNS), so the fake upstream must be reachable on a
// real host address.
func localRoutableIPv4(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("interface inventory unavailable: %v", err)
	}
	for _, candidate := range interfaces {
		if candidate.Flags&net.FlagUp == 0 || candidate.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, addressErr := candidate.Addrs()
		if addressErr != nil {
			continue
		}
		for _, raw := range addresses {
			address, parseErr := netip.ParseAddr(strings.SplitN(raw.String(), "/", 2)[0])
			if parseErr == nil && address.Is4() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
				return address.String()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address to bind the fake upstream")
	return ""
}

// startFakeUpstreamDNS runs the resolver the endpoint must reach.
func startFakeUpstreamDNS(t *testing.T, host string, handler func([]byte) []byte) (string, func()) {
	t.Helper()
	connection, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
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
			response := handler(append([]byte(nil), buffer[:read]...))
			if len(response) == 0 {
				continue
			}
			_, _ = connection.WriteTo(response, source)
		}
	}()
	stop := func() {
		connection.Close()
		<-done
	}
	t.Cleanup(stop)
	return connection.LocalAddr().String(), stop
}

// fakeDNSResponseWithA returns a NOERROR answer with one A record.
func fakeDNSResponseWithA(query []byte, address net.IP) []byte {
	if len(query) < 12 {
		return nil
	}
	questionEnd := decodeDNSQuestionEnd(query)
	if questionEnd < 0 {
		return nil
	}
	response := append([]byte(nil), query[:questionEnd]...)
	response[2] |= 0x80 // QR
	response[3] = 0x80  // NOERROR + RA
	binary.BigEndian.PutUint16(response[6:8], 1)
	// SmartDNS sends EDNS OPT in ARCOUNT. The response only carries the
	// question and one answer, so the authority/additional counts must be
	// cleared or a real resolver rejects the message as truncated.
	binary.BigEndian.PutUint16(response[8:10], 0)
	binary.BigEndian.PutUint16(response[10:12], 0)
	answer := make([]byte, 16)
	answer[0], answer[1] = 0xc0, 0x0c // name pointer
	binary.BigEndian.PutUint16(answer[2:4], 1)
	binary.BigEndian.PutUint16(answer[4:6], 1)
	binary.BigEndian.PutUint32(answer[6:10], 60)
	binary.BigEndian.PutUint16(answer[10:12], 4)
	copy(answer[12:16], address.To4())
	return append(response, answer...)
}

func decodeDNSQuestionEnd(query []byte) int {
	offset := 12
	for {
		if offset >= len(query) {
			return -1
		}
		length := int(query[offset])
		offset++
		if length == 0 {
			break
		}
		offset += length
	}
	if offset+4 > len(query) {
		return -1
	}
	return offset + 4
}

func decodeDNSQuestionName(query []byte) string {
	if len(query) < 12 {
		return ""
	}
	offset := 12
	labels := make([]string, 0, 4)
	for {
		if offset >= len(query) {
			return ""
		}
		length := int(query[offset])
		offset++
		if length == 0 {
			break
		}
		if offset+length > len(query) {
			return ""
		}
		labels = append(labels, string(query[offset:offset+length]))
		offset += length
	}
	return strings.Join(labels, ".")
}

func TestProxyDNSEndpointIntegrationThroughFakeXray(t *testing.T) {
	// A fake upstream that answers every A query with 203.0.113.9.
	var queries []string
	var queriesMu sync.Mutex
	upstreamAddress, stopUpstream := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		name := decodeDNSQuestionName(query)
		queriesMu.Lock()
		queries = append(queries, name)
		queriesMu.Unlock()
		return fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 9))
	})

	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{upstreamAddress}}}
	compiledDNS, err := dns.CompilePolicyWithOptions(dns.NewPolicy(dns.Reject(), []dns.Rule{{
		ID:      "proxy-media",
		Domains: []string{"media.example"},
		Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"},
	}}), dns.CompileOptions{
		ProxyEgresses:              []proxy.Egress{proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound")},
		UpstreamIDs:                []string{"dns-proxy"},
		ValidateUpstreamReferences: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := compiledDNS.RenderSmartDNS()
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, map[string]string{"proxy-media": "proxy-media"}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	endpoint := plan.Endpoints[0]

	compiledProxy, err := proxy.CompileEgress(proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := RenderXrayWithProxyDNS(compiledProxy, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifacts[0].Content, "\"listen\": \"127.0.0.1\"") {
		t.Fatalf("xray config does not bind the endpoint on loopback: %s", artifacts[0].Content)
	}
	if !strings.Contains(artifacts[0].Content, fmt.Sprintf("\"port\": %d", endpoint.Port)) {
		t.Fatalf("xray config does not contain the planned port %d: %s", endpoint.Port, artifacts[0].Content)
	}

	// The SmartDNS side must point at the same endpoint and never at the
	// upstream address directly.
	dnsConfig := renderProxyDNSTestConfig(t, render, upstreams, plan)
	if !strings.Contains(dnsConfig, "nameserver /-.media.example/"+endpoint.Group()) {
		t.Fatalf("smartdns does not route the domain to the endpoint:\n%s", dnsConfig)
	}
	if strings.Contains(dnsConfig, "nameserver /-.media.example/dns-proxy") {
		t.Fatalf("smartdns routed the proxied domain to the direct upstream group:\n%s", dnsConfig)
	}

	stopXray := startFakeXray(t, artifacts)
	prober := ProxyDNSEndpointProber{Timeout: time.Second}
	waitForProxyDNSEndpoint(t, prober, endpoint)

	queriesMu.Lock()
	observed := append([]string(nil), queries...)
	queriesMu.Unlock()
	if len(observed) == 0 || observed[len(observed)-1] != "example.com" {
		t.Fatalf("upstream queries = %#v, want the probe question through the endpoint", observed)
	}

	// Upstream unavailable: the endpoint stays listening (port open) but no
	// longer answers. fail_closed must not invent an answer.
	stopUpstream()
	short := ProxyDNSEndpointProber{Timeout: 300 * time.Millisecond}
	if err := short.Probe(context.Background(), endpoint); err == nil {
		t.Fatal("endpoint answered after the upstream became unreachable")
	}

	// Endpoint process gone (Xray restart/stop): the port check itself fails.
	stopXray()
	if err := short.Probe(context.Background(), endpoint); err == nil {
		t.Fatal("endpoint answered after the fake xray process stopped")
	}
}
