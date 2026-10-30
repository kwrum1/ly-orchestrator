package service

import (
	"encoding/binary"
	"fmt"
	"net"
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

// Real-binary validation of the DNS chain and the reject-miss semantics.
//
// Requires the shipped binaries:
//
//	LY_ROUTE_XRAY_BINARY=/root/work/rootfs-prod/usr/bin/xray \
//	LY_ROUTE_SMARTDNS_BINARY=/root/work/rootfs-prod/usr/sbin/smartdns \
//	  go test ./internal/runtime/service -run 'RealSmartDNS' -count=1 -v
const realSmartDNSEnv = "LY_ROUTE_SMARTDNS_BINARY"

func realSmartDNSBinary(t *testing.T) string {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv(realSmartDNSEnv))
	if binary == "" {
		t.Skip(realSmartDNSEnv + " is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("%s does not exist: %v", binary, err)
	}
	return binary
}

// dnsAnswer is the subset of a DNS response the tests need.
type dnsAnswer struct {
	RCode   int
	Answers []string
}

// queryDNS sends one A query and parses the answer records.
func queryDNS(t *testing.T, address, name string, timeout time.Duration) (dnsAnswer, error) {
	t.Helper()
	query, id, err := buildProxyDNSProbeQuery(name)
	if err != nil {
		return dnsAnswer{}, err
	}
	connection, err := net.DialTimeout("udp", address, timeout)
	if err != nil {
		return dnsAnswer{}, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return dnsAnswer{}, err
	}
	if _, err := connection.Write(query); err != nil {
		return dnsAnswer{}, err
	}
	response := make([]byte, 4096)
	read, err := connection.Read(response)
	if err != nil {
		return dnsAnswer{}, err
	}
	response = response[:read]
	if len(response) < 12 || binary.BigEndian.Uint16(response[0:2]) != id {
		return dnsAnswer{}, fmt.Errorf("invalid response")
	}
	answer := dnsAnswer{RCode: int(response[3] & 0x0f)}
	count := int(binary.BigEndian.Uint16(response[6:8]))
	offset := 12
	for offset < len(response) && response[offset] != 0 {
		offset += int(response[offset]) + 1
	}
	offset += 5 // zero label + QTYPE + QCLASS
	for index := 0; index < count; index++ {
		if offset+12 > len(response) {
			break
		}
		if response[offset]&0xc0 == 0xc0 {
			offset += 2
		} else {
			for offset < len(response) && response[offset] != 0 {
				offset += int(response[offset]) + 1
			}
			offset++
		}
		if offset+10 > len(response) {
			break
		}
		recordType := binary.BigEndian.Uint16(response[offset : offset+2])
		class := binary.BigEndian.Uint16(response[offset+2 : offset+4])
		length := int(binary.BigEndian.Uint16(response[offset+8 : offset+10]))
		offset += 10
		if offset+length > len(response) {
			break
		}
		if recordType == 1 && class == 1 && length == 4 {
			answer.Answers = append(answer.Answers, net.IP(response[offset:offset+length]).String())
		}
		offset += length
	}
	return answer, nil
}

type realSmartDNSProcess struct {
	command *exec.Cmd
	output  *lockedBuffer
	done    chan error
	once    sync.Once
}

func startRealSmartDNSProcess(t *testing.T, binary, configPath string) *realSmartDNSProcess {
	t.Helper()
	command := exec.Command(binary, "-f", "-c", configPath, "-p", "-")
	output := &lockedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start smartdns: %v", err)
	}
	process := &realSmartDNSProcess{command: command, output: output, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(func() {
		process.once.Do(func() {
			_ = process.command.Process.Kill()
			select {
			case <-process.done:
			case <-time.After(5 * time.Second):
			}
		})
	})
	return process
}

func (process *realSmartDNSProcess) Wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-process.done:
		t.Fatalf("smartdns exited unexpectedly: %v\n%s", err, process.output.String())
	case <-time.After(50 * time.Millisecond):
	}
}

// realSmartDNSFixture is the product-rendered SmartDNS fragment running on a
// private loopback port.
type realSmartDNSFixture struct {
	address string
	config  string
	process *realSmartDNSProcess
}

func startRealSmartDNSPolicy(t *testing.T, binary string, policy dns.Policy, upstreams []SmartDNSUpstream) *realSmartDNSFixture {
	t.Helper()
	ids := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		ids = append(ids, upstream.ID)
	}
	compiled, err := dns.CompilePolicyWithOptions(policy, dns.CompileOptions{
		ProxyEgresses:              []proxy.Egress{proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound")},
		UpstreamIDs:                ids,
		ValidateUpstreamReferences: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := compiled.RenderSmartDNS()
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 {
		t.Fatalf("endpoint plan problems = %#v", plan.Problems)
	}
	artifacts, err := RenderSmartDNS(SmartDNSPlan{ID: "active", Render: render, Upstreams: upstreams, ProxyDNSEndpoints: plan})
	if err != nil {
		t.Fatal(err)
	}
	fragment := ""
	for _, artifact := range artifacts {
		if artifact.Path == "/etc/smartdns/conf.d/ly-route-active.conf" {
			fragment = artifact.Content
		}
	}
	if fragment == "" {
		t.Fatal("active SmartDNS fragment missing")
	}
	listenPort := freeUDPPort(t)
	config := fmt.Sprintf("bind 127.0.0.1:%d\nbind-tcp 127.0.0.1:%d\n%s", listenPort, listenPort, fragment)
	configPath := filepath.Join(t.TempDir(), "smartdns-proxy-dns.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	process := startRealSmartDNSProcess(t, binary, configPath)
	fixture := &realSmartDNSFixture{
		address: fmt.Sprintf("127.0.0.1:%d", listenPort),
		config:  config,
		process: process,
	}
	fixture.waitReady(t)
	return fixture
}

func (fixture *realSmartDNSFixture) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-fixture.process.done:
			t.Fatalf("smartdns exited: %v\n%s", err, fixture.process.output.String())
		default:
		}
		if _, lastErr = queryDNS(t, fixture.address, "probe.invalid", 300*time.Millisecond); lastErr == nil {
			return
		}
		// SERVFAIL (rcode 2) is also a valid answer: the process is up.
		if lastErr == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("smartdns never answered: %v\n%s", lastErr, fixture.process.output.String())
}

func (fixture *realSmartDNSFixture) query(t *testing.T, name string) dnsAnswer {
	t.Helper()
	answer, err := queryDNS(t, fixture.address, name, 2*time.Second)
	if err != nil {
		t.Fatalf("query %q failed: %v\n%s", name, err, fixture.process.output.String())
	}
	return answer
}

func TestRealSmartDNSAndXrayProxyDNSChain(t *testing.T) {
	xrayBinary := realXrayBinary(t)
	smartDNSBinary := realSmartDNSBinary(t)

	// Fake upstream reachable only through the SOCKS "proxy".
	proxyUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		return fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 7))
	})
	// Second fake upstream used by the direct miss rule, reachable directly.
	directUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		return fakeDNSResponseWithA(query, net.IPv4(198, 51, 100, 20))
	})

	upstreams := []SmartDNSUpstream{
		{ID: "dns-proxy", Servers: []string{realXrayUDPTarget}},
		{ID: "dns-direct", Servers: []string{directUpstreamAddress}},
	}
	policy := dns.NewPolicy(
		dns.Outcome{Kind: dns.OutcomeDirect, UpstreamID: "dns-direct"},
		[]dns.Rule{
			{ID: "proxy-media", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"}},
			{ID: "blocked", Domains: []string{"blocked.example"}, Outcome: dns.Reject()},
		},
	)
	compiled, err := dns.CompilePolicyWithOptions(policy, dns.CompileOptions{
		ProxyEgresses:              []proxy.Egress{proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound")},
		UpstreamIDs:                []string{"dns-proxy", "dns-direct"},
		ValidateUpstreamReferences: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := compiled.RenderSmartDNS()
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	endpoint := plan.Endpoints[0]

	socks := startFakeSocksServer(t, map[int]string{53: proxyUpstreamAddress})
	xrayConfigPath := writeRealXrayConfig(t, realXrayConfigForTest(t, plan, socks.Address(), true))
	xrayProcess := startRealXrayProcess(t, xrayBinary, xrayConfigPath)
	xrayProcess.WaitReady(t, ProxyDNSEndpointProber{Timeout: 2 * time.Second}, endpoint)

	fixture := startRealSmartDNSPolicy(t, smartDNSBinary, policy, upstreams)
	if !strings.Contains(fixture.config, "nameserver /-.media.example/"+endpoint.Group()) {
		t.Fatalf("rendered SmartDNS config does not use the endpoint group:\n%s", fixture.config)
	}

	proxied := fixture.query(t, "media.example")
	if len(proxied.Answers) != 1 || proxied.Answers[0] != "203.0.113.7" {
		t.Fatalf("proxied answer = %#v, want 203.0.113.7 through the endpoint", proxied)
	}
	destinations := strings.Join(socks.Observed(), ",")
	if !strings.Contains(destinations, realXrayUDPTarget+":53") {
		t.Fatalf("proxy outbound never dialled the proxied upstream; observed %q", destinations)
	}

	// The direct miss rule must resolve locally, not through the proxy.
	direct := fixture.query(t, "other.example")
	if len(direct.Answers) != 1 || direct.Answers[0] != "198.51.100.20" {
		t.Fatalf("direct answer = %#v, want the direct upstream", direct)
	}

	// Reject rule returns an empty answer.
	blocked := fixture.query(t, "blocked.example")
	if len(blocked.Answers) != 0 {
		t.Fatalf("reject rule returned answers %#v", blocked.Answers)
	}

	xrayProcess.Wait(t)
	fixture.process.Wait(t)
}

// TestRealSmartDNSRejectMissKeepsExplicitRules is the regression for the
// global `address #` defect: with miss=reject the explicit direct rule must
// still resolve, and unmatched domains must fail closed (SERVFAIL) without
// reaching any upstream.
func TestRealSmartDNSRejectMissKeepsExplicitRules(t *testing.T) {
	smartDNSBinary := realSmartDNSBinary(t)
	var directQueries []string
	var directMu sync.Mutex
	directUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		directMu.Lock()
		directQueries = append(directQueries, decodeDNSQuestionName(query))
		directMu.Unlock()
		return fakeDNSResponseWithA(query, net.IPv4(198, 51, 100, 20))
	})
	upstreams := []SmartDNSUpstream{{ID: "dns-direct", Servers: []string{directUpstreamAddress}}}
	policy := dns.NewPolicy(dns.Reject(), []dns.Rule{
		{ID: "direct-rule", Domains: []string{"direct.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeDirect, UpstreamID: "dns-direct"}},
		{ID: "blocked", Domains: []string{"blocked.example"}, Outcome: dns.Reject()},
	})

	fixture := startRealSmartDNSPolicy(t, smartDNSBinary, policy, upstreams)
	for _, line := range strings.Split(fixture.config, "\n") {
		directive := strings.TrimSpace(line)
		if strings.HasPrefix(directive, "#") {
			continue
		}
		if strings.HasPrefix(directive, "address #") {
			t.Fatalf("reject miss rendered a global address rule:\n%s", fixture.config)
		}
		if strings.HasPrefix(directive, "nameserver /./") && !strings.Contains(directive, SmartDNSRejectGroup) {
			t.Fatalf("reject miss rendered a default nameserver %q:\n%s", directive, fixture.config)
		}
	}
	if !strings.Contains(fixture.config, "nameserver /./"+SmartDNSRejectGroup) {
		t.Fatalf("reject miss does not select the empty reject group:\n%s", fixture.config)
	}

	direct := fixture.query(t, "direct.example")
	if len(direct.Answers) != 1 || direct.Answers[0] != "198.51.100.20" {
		t.Fatalf("explicit direct rule was suppressed by the reject miss: %#v", direct)
	}

	unmatched := fixture.query(t, "unmatched.example")
	if unmatched.RCode != 2 || len(unmatched.Answers) != 0 {
		t.Fatalf("unmatched domain = %#v, want SERVFAIL with no answers (fail closed)", unmatched)
	}

	blocked := fixture.query(t, "blocked.example")
	if len(blocked.Answers) != 0 {
		t.Fatalf("explicit reject rule returned answers %#v", blocked.Answers)
	}

	directMu.Lock()
	observed := append([]string(nil), directQueries...)
	directMu.Unlock()
	for _, query := range observed {
		if query == "unmatched.example" {
			t.Fatalf("unmatched domain leaked to the direct upstream: %#v", observed)
		}
	}
	fixture.process.Wait(t)
}

// TestRealSmartDNSRejectMissKeepsProxyRule runs the same reject-miss policy
// with a proxy outcome through the real Xray endpoint.
func TestRealSmartDNSRejectMissKeepsProxyRule(t *testing.T) {
	xrayBinary := realXrayBinary(t)
	smartDNSBinary := realSmartDNSBinary(t)
	proxyUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		return fakeDNSResponseWithA(query, net.IPv4(203, 0, 113, 7))
	})
	var directQueries []string
	var directMu sync.Mutex
	directUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		directMu.Lock()
		directQueries = append(directQueries, decodeDNSQuestionName(query))
		directMu.Unlock()
		return fakeDNSResponseWithA(query, net.IPv4(198, 51, 100, 20))
	})
	upstreams := []SmartDNSUpstream{
		{ID: "dns-proxy", Servers: []string{realXrayUDPTarget}},
		{ID: "dns-direct", Servers: []string{directUpstreamAddress}},
	}
	policy := dns.NewPolicy(dns.Reject(), []dns.Rule{
		{ID: "proxy-media", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"}},
		{ID: "direct-rule", Domains: []string{"direct.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeDirect, UpstreamID: "dns-direct"}},
	})
	compiled, err := dns.CompilePolicyWithOptions(policy, dns.CompileOptions{
		ProxyEgresses:              []proxy.Egress{proxy.NewProxyEgress("proxy-media", "xray-tproxy-outbound")},
		UpstreamIDs:                []string{"dns-proxy", "dns-direct"},
		ValidateUpstreamReferences: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := compiled.RenderSmartDNS()
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, map[string]string{"proxy-media": realXraySocksTag}, map[string]bool{"proxy-media": true})
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	socks := startFakeSocksServer(t, map[int]string{53: proxyUpstreamAddress})
	xrayProcess := startRealXrayProcess(t, xrayBinary, writeRealXrayConfig(t, realXrayConfigForTest(t, plan, socks.Address(), true)))
	xrayProcess.WaitReady(t, ProxyDNSEndpointProber{Timeout: 2 * time.Second}, plan.Endpoints[0])

	fixture := startRealSmartDNSPolicy(t, smartDNSBinary, policy, upstreams)
	proxied := fixture.query(t, "media.example")
	if len(proxied.Answers) != 1 || proxied.Answers[0] != "203.0.113.7" {
		t.Fatalf("proxy rule was suppressed by the reject miss: %#v", proxied)
	}
	direct := fixture.query(t, "direct.example")
	if len(direct.Answers) != 1 || direct.Answers[0] != "198.51.100.20" {
		t.Fatalf("direct rule was suppressed by the reject miss: %#v", direct)
	}
	unmatched := fixture.query(t, "unmatched.example")
	if unmatched.RCode != 2 || len(unmatched.Answers) != 0 {
		t.Fatalf("unmatched domain = %#v, want SERVFAIL (fail closed)", unmatched)
	}

	destinations := strings.Join(socks.Observed(), ",")
	if !strings.Contains(destinations, realXrayUDPTarget+":53") {
		t.Fatalf("proxy rule did not use the endpoint; observed %q", destinations)
	}
	directMu.Lock()
	observed := append([]string(nil), directQueries...)
	directMu.Unlock()
	for _, query := range observed {
		if query == "unmatched.example" {
			t.Fatalf("unmatched domain leaked to the direct upstream: %#v", observed)
		}
	}
	xrayProcess.Wait(t)
	fixture.process.Wait(t)
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	connection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("unexpected local address")
	}
	return address.Port
}

// TestRealSmartDNSRejectMissSourceScopedRule covers the per-source listener the
// renderer creates with `group-begin ... -inherit none`: the source-scoped rule
// must still resolve and unmatched queries on that listener must fail closed.
func TestRealSmartDNSRejectMissSourceScopedRule(t *testing.T) {
	smartDNSBinary := realSmartDNSBinary(t)
	directUpstreamAddress, _ := startFakeUpstreamDNS(t, localRoutableIPv4(t), func(query []byte) []byte {
		return fakeDNSResponseWithA(query, net.IPv4(198, 51, 100, 20))
	})
	upstreams := []SmartDNSUpstream{{ID: "dns-direct", Servers: []string{directUpstreamAddress}}}
	policy := dns.NewPolicy(dns.Reject(), []dns.Rule{{
		ID:             "scoped",
		SourcePrefixes: []string{"127.0.0.1/32"},
		Domains:        []string{"scoped.example"},
		Outcome:        dns.Outcome{Kind: dns.OutcomeDirect, UpstreamID: "dns-direct"},
	}})
	fixture := startRealSmartDNSPolicy(t, smartDNSBinary, policy, upstreams)

	scopedAddress := ""
	for _, line := range strings.Split(fixture.config, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "bind 127.0.0.1:") || strings.Contains(line, "-group") == false {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			scopedAddress = fields[1]
		}
	}
	if scopedAddress == "" {
		t.Fatalf("rendered config has no source-scoped listener:\n%s", fixture.config)
	}
	if scopedAddress == fixture.address {
		t.Fatalf("source-scoped listener reused the main bind address:\n%s", fixture.config)
	}

	answer, err := queryDNS(t, scopedAddress, "scoped.example", 2*time.Second)
	if err != nil {
		t.Fatalf("scoped query failed: %v\n%s", err, fixture.process.output.String())
	}
	if len(answer.Answers) != 1 || answer.Answers[0] != "198.51.100.20" {
		t.Fatalf("source-scoped direct rule = %#v, want the direct upstream", answer)
	}
	unmatched, err := queryDNS(t, scopedAddress, "unmatched.example", 2*time.Second)
	if err != nil {
		t.Fatalf("scoped unmatched query failed: %v", err)
	}
	if unmatched.RCode != 2 || len(unmatched.Answers) != 0 {
		t.Fatalf("source-scoped unmatched = %#v, want SERVFAIL (fail closed)", unmatched)
	}
	fixture.process.Wait(t)
}

// TestRealSmartDNSRejectMissWithoutRulesFailsClosed covers the factory
// fail-closed policy: no rules, no default resolver, every query SERVFAIL.
func TestRealSmartDNSRejectMissWithoutRulesFailsClosed(t *testing.T) {
	smartDNSBinary := realSmartDNSBinary(t)
	fixture := startRealSmartDNSPolicy(t, smartDNSBinary, dns.NewPolicy(dns.Reject(), nil), nil)
	// With no upstream at all, SmartDNS would otherwise import /etc/resolv.conf
	// and leak rejected domains to the host resolver. The reserved excluded
	// server keeps the resolver set non-empty; no rule selects it.
	if !strings.Contains(fixture.config, "server 127.0.0.1:1 -group lyroute-reserved:no-use -exclude-default-group") {
		t.Fatalf("reject miss without upstreams does not render the reserved server guard:\n%s", fixture.config)
	}
	if !strings.Contains(fixture.config, "nameserver /./"+SmartDNSRejectGroup) {
		t.Fatalf("reject miss does not select the empty reject group:\n%s", fixture.config)
	}
	for _, name := range []string{"example.com", "unmatched.example"} {
		answer := fixture.query(t, name)
		if answer.RCode != 2 || len(answer.Answers) != 0 {
			t.Fatalf("%s = %#v, want SERVFAIL with no answers", name, answer)
		}
	}
	fixture.process.Wait(t)
}
