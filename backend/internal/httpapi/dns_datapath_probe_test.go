package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controlapi "ly-route/backend/internal/api"
)

// dnsDatapathTestServer builds a gateway server whose data-plane DNS probe is
// driven by the test instead of a real VPP tap.
func dnsDatapathTestServer(t *testing.T, probe func(context.Context) (dnsProbeResult, error)) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vcl.conf")
	if err := os.WriteFile(configPath, []byte("vcl {\n}\n"), 0o600); err != nil {
		t.Fatalf("write vcl config: %v", err)
	}
	server := New()
	server.dnsDatapathConfigPath = configPath
	server.dnsDatapathStateFile = filepath.Join(dir, "dns-datapath.json")
	server.dnsDatapathProbe = probe
	return server, dir
}

func healthDependency(t *testing.T, server *Server, name string) (controlapi.CapabilityState, bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	server.handleHealth(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}
	var response struct {
		Status       string                       `json:"status"`
		Dependencies []controlapi.CapabilityState `json:"dependencies"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	for _, dependency := range response.Dependencies {
		if dependency.Name == name {
			return dependency, true
		}
	}
	return controlapi.CapabilityState{}, false
}

// The probe must pass only on a real answer, and /health must follow it.
func TestDNSDatapathCapabilityAvailableOnAnswer(t *testing.T) {
	t.Parallel()
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		return dnsProbeResult{OK: true, Answer: "NOERROR"}, nil
	})

	capability, ok := server.dnsDatapathCapability(context.Background())
	if !ok {
		t.Fatal("dns_datapath capability was not published for a gateway with the VPP DNS adapter")
	}
	if !capability.Available || capability.State != controlapi.CapabilityAvailable {
		t.Fatalf("capability = %#v, want available", capability)
	}
	dependency, present := healthDependency(t, server, "dns_datapath")
	if !present {
		t.Fatal("health response does not list dns_datapath")
	}
	if !dependency.Available {
		t.Fatalf("health dependency = %#v, want available", dependency)
	}
}

// A timeout is the field symptom: units active, no answer.  Health must turn
// degraded and carry the reason.
func TestHealthDegradedWhenDNSDatapathProbeTimesOut(t *testing.T) {
	t.Parallel()
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		return dnsProbeResult{}, context.DeadlineExceeded
	})

	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}
	var response struct {
		Status       string                       `json:"status"`
		Dependencies []controlapi.CapabilityState `json:"dependencies"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.Status != "degraded" {
		t.Fatalf("health status = %q, want degraded", response.Status)
	}
	var reason string
	for _, dependency := range response.Dependencies {
		if dependency.Name == "dns_datapath" {
			reason = dependency.Reason
			if dependency.Available {
				t.Fatalf("dns_datapath dependency = %#v, want unavailable", dependency)
			}
		}
	}
	if reason == "" {
		t.Fatal("degraded health response does not explain the DNS data-path failure")
	}
}

func TestDNSDatapathSERVFAILIsDegraded(t *testing.T) {
	t.Parallel()
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		return dnsProbeResult{OK: false, Reason: "DNS data-path probe reached the resolver but it answered SERVFAIL"}, nil
	})
	capability, ok := server.dnsDatapathCapability(context.Background())
	if !ok || capability.Available {
		t.Fatalf("capability = %#v (published=%v), want degraded", capability, ok)
	}
	if !strings.Contains(capability.Reason, "SERVFAIL") {
		t.Fatalf("reason = %q, want the resolver rcode", capability.Reason)
	}
}

// The repair helper knows about state the API cannot read (session rules, FIB
// table 100, plugin attachment).  Its verdict must win over a passing probe.
func TestDNSDatapathUsesRepairVerdict(t *testing.T) {
	t.Parallel()
	server, dir := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		return dnsProbeResult{OK: true, Answer: "NOERROR"}, nil
	})
	state := dnsDatapathState{
		OK:          false,
		Configured:  true,
		Reason:      "FIB table 100 is missing the return route for 192.168.88.0/24",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(server.dnsDatapathStateFile, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	_ = dir

	capability, ok := server.dnsDatapathCapability(context.Background())
	if !ok || capability.Available {
		t.Fatalf("capability = %#v, want degraded from the repair verdict", capability)
	}
	if !strings.Contains(capability.Reason, "return route") {
		t.Fatalf("reason = %q, want the repair reason", capability.Reason)
	}
}

// A stale repair verdict must not mask a live path, and a probe result is
// cached so health polling cannot flood the resolver.
func TestDNSDatapathProbeCacheAndStaleVerdict(t *testing.T) {
	t.Parallel()
	calls := 0
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		calls++
		return dnsProbeResult{OK: true, Answer: "NOERROR"}, nil
	})
	state := dnsDatapathState{
		OK:          false,
		Configured:  true,
		Reason:      "stale failure",
		GeneratedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(server.dnsDatapathStateFile, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	for i := 0; i < 3; i++ {
		capability, ok := server.dnsDatapathCapability(context.Background())
		if !ok || !capability.Available {
			t.Fatalf("call %d: capability = %#v, want available", i, capability)
		}
	}
	if calls != 1 {
		t.Fatalf("probe ran %d times, want 1 (cached)", calls)
	}
}

// Appliances without the VPP DNS adapter must not gain a permanent
// degradation.
func TestDNSDatapathNotPublishedWithoutAdapter(t *testing.T) {
	t.Parallel()
	server := New()
	server.dnsDatapathConfigPath = filepath.Join(t.TempDir(), "absent.conf")
	server.dnsDatapathStateFile = filepath.Join(t.TempDir(), "absent.json")
	server.dnsDatapathProbe = func(context.Context) (dnsProbeResult, error) {
		t.Fatal("the probe must not run when the VPP DNS adapter is not installed")
		return dnsProbeResult{}, nil
	}
	if _, ok := server.dnsDatapathCapability(context.Background()); ok {
		t.Fatal("dns_datapath was published for a product without the VPP DNS adapter")
	}
	if _, present := healthDependency(t, server, "dns_datapath"); present {
		t.Fatal("health lists dns_datapath for a product without the VPP DNS adapter")
	}
}

// A broken path must clear again once the probe answers: the health signal is
// a live verdict, not a latch.
func TestDNSDatapathHealthRecoversAfterProbeSuccess(t *testing.T) {
	t.Parallel()
	healthy := false
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		if !healthy {
			return dnsProbeResult{}, errors.New("DNS data-path probe timed out")
		}
		return dnsProbeResult{OK: true, Answer: "NOERROR"}, nil
	})
	degraded, ok := server.dnsDatapathCapability(context.Background())
	if !ok || degraded.Available {
		t.Fatalf("capability = %#v, want degraded before the probe answers", degraded)
	}
	healthy = true
	server.dnsProbeMu.Lock()
	server.dnsProbeCached = nil
	server.dnsProbeMu.Unlock()
	recovered, ok := server.dnsDatapathCapability(context.Background())
	if !ok || !recovered.Available {
		t.Fatalf("capability = %#v, want available after the probe answers", recovered)
	}
}

// An appliance that is deployed without the diagnostics probe TAP marks the
// probe "disabled" in the repair verdict; health must stay available instead of
// degrading forever for a probe that is not supposed to exist.
func TestDNSDatapathProbeDisabledByIntent(t *testing.T) {
	t.Parallel()
	server, _ := dnsDatapathTestServer(t, func(context.Context) (dnsProbeResult, error) {
		t.Fatal("the probe must not run when the datapath helper reports it disabled")
		return dnsProbeResult{}, nil
	})
	state := dnsDatapathState{
		OK:          true,
		Configured:  true,
		Checks:      map[string]string{"vpp": "ok", "probe": "disabled"},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(server.dnsDatapathStateFile, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	capability, ok := server.dnsDatapathCapability(context.Background())
	if !ok {
		t.Fatal("dns_datapath capability was not published")
	}
	if !capability.Available || capability.State != controlapi.CapabilityAvailable {
		t.Fatalf("capability = %#v, want available with the probe disabled", capability)
	}
}

func TestDNSProbeQueryIsWellFormed(t *testing.T) {
	t.Parallel()
	query, id, err := dnsProbeQuery()
	if err != nil {
		t.Fatalf("build query: %v", err)
	}
	if len(query) < 17 {
		t.Fatalf("query length = %d, want at least a header and one question", len(query))
	}
	if got := binary.BigEndian.Uint16(query[0:2]); got != id {
		t.Fatalf("transaction id = %d, want %d", got, id)
	}
	if binary.BigEndian.Uint16(query[4:6]) != 1 {
		t.Fatalf("qdcount = %d, want 1", binary.BigEndian.Uint16(query[4:6]))
	}
	if query[12] == 0 || query[12] > 63 {
		t.Fatalf("first label length = %d", query[12])
	}
	labelLength := int(query[12])
	if got := string(query[13 : 13+labelLength]); !strings.HasPrefix(got, "probe-") {
		t.Fatalf("first label = %q, want the probe prefix", got)
	}
	if !strings.Contains(string(query), "ly-route") || !strings.Contains(string(query), "invalid") {
		t.Fatalf("probe question is not in the reserved .invalid domain: %q", string(query))
	}
}
