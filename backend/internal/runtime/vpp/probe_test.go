package vpp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCapabilityReport(t *testing.T, path, management string, proofs string) {
	t.Helper()
	body := `{"management_interface":"` + management + `","proofs":[` + proofs + `]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nativeProofJSON(t *testing.T, interfaceName string, now time.Time) string {
	t.Helper()
	return `{"linux_interface":"` + interfaceName + `","candidates":[{"tier":"vpp_native","hook":"af_xdp","mode":"zero_copy","source":"runtime_probe","runtime_verified":true,"native":true,"high_performance":true,"observed_at":"` +
		now.Add(-time.Minute).Format(time.RFC3339) + `","valid_until":"` + now.Add(time.Hour).Format(time.RFC3339) + `","performance_score":100,"mac_address":"00:11:22:33:44:55"}]}`
}

func writeCommittedState(t *testing.T, path, interfaceName string) {
	t.Helper()
	body := `{"path":{"tier":"vpp_native","attachments":[{"linux_interface":"` + interfaceName + `","vpp_interface":"lyroute-` +
		interfaceName + `","tier":"vpp_native","hook":"af_xdp","mode":"zero_copy","mac_address":"00:11:22:33:44:55"}],"prerequisites":[]},"applied_at":"2026-08-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCapabilityProberObservesAtRuntime pins the arch-v1 replacement for the
// pre-generated proof file: control runs the probe and treats its report as the
// observed capability.
func TestCapabilityProberObservesAtRuntime(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "capabilities.json")
	writeCapabilityReport(t, reportPath, "eth0", nativeProofJSON(t, "eth1", now))
	probeRan := false
	prober := NewCapabilityProber("/usr/lib/ly-route/runtime-check.sh", reportPath, filepath.Join(directory, "active.json"))
	prober.Now = func() time.Time { return now }
	prober.Run = func(_ context.Context, _ string, env []string) error {
		probeRan = true
		if !containsEnv(env, "LY_ROUTE_VPP_DATA_INTERFACES=eth1") {
			t.Fatalf("probe environment missing data interfaces: %#v", env)
		}
		return nil
	}

	observation := prober.Observe(context.Background(), ProbeTarget{ManagementInterface: "eth0", DataInterfaces: []string{"eth1"}})
	if !probeRan {
		t.Fatal("probe command did not run")
	}
	if observation.Source != ProbeSourceRuntime || observation.Degraded {
		t.Fatalf("observation = %#v, want an undegraded runtime probe", observation)
	}
	if len(observation.Report.Proofs) != 1 || observation.Report.Proofs[0].LinuxInterface != "eth1" {
		t.Fatalf("observed report = %#v", observation.Report)
	}
}

// TestCapabilityProberFallsBackToCommittedStateWithoutLocking pins requirement 2:
// a broken probe degrades observability but keeps a previously committed
// generation forwarding instead of producing dataplane_locked.
func TestCapabilityProberFallsBackToCommittedStateWithoutLocking(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "capabilities.json")
	activePath := filepath.Join(directory, "active.json")
	writeCommittedState(t, activePath, "eth1")
	prober := NewCapabilityProber("/usr/lib/ly-route/runtime-check.sh", reportPath, activePath)
	prober.Now = func() time.Time { return now }
	prober.Run = func(context.Context, string, []string) error {
		return errors.New("vppctl unavailable")
	}

	observation := prober.Observe(context.Background(), ProbeTarget{ManagementInterface: "eth0", DataInterfaces: []string{"eth1"}})
	if observation.Source != ProbeSourceCommitted || !observation.Degraded {
		t.Fatalf("observation = %#v, want a degraded committed-state fallback", observation)
	}
	if observation.Reason == "" {
		t.Fatal("degraded observation must carry a reason")
	}

	path, err := SelectNativePath(observation.NativePathRequest("eth0", []string{"eth1"}, now, false))
	if err != nil {
		t.Fatalf("committed-state fallback must still select a forwarding path: %v", err)
	}
	if len(path.Attachments) != 1 || path.Attachments[0].LinuxInterface != "eth1" {
		t.Fatalf("selected path = %#v", path)
	}
}

// TestCapabilityProberCachesObservations proves the probe does not run on every
// request: the probe creates and removes VPP interfaces.
func TestCapabilityProberCachesObservations(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "capabilities.json")
	writeCapabilityReport(t, reportPath, "eth0", nativeProofJSON(t, "eth1", now))
	runs := 0
	prober := NewCapabilityProber("probe", reportPath, "")
	prober.Now = func() time.Time { return now }
	prober.Run = func(context.Context, string, []string) error {
		runs++
		return nil
	}
	target := ProbeTarget{ManagementInterface: "eth0", DataInterfaces: []string{"eth1"}}
	prober.Observe(context.Background(), target)
	prober.Observe(context.Background(), target)
	if runs != 1 {
		t.Fatalf("probe runs = %d, want 1 within the cache window", runs)
	}
	prober.Invalidate()
	prober.Observe(context.Background(), target)
	if runs != 2 {
		t.Fatalf("probe runs after invalidate = %d, want 2", runs)
	}
}

// TestCapabilityProberRejectsInsecureReport pins the ownership rule: a
// group/world-writable report is not a capability source.
func TestCapabilityProberRejectsInsecureReport(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "capabilities.json")
	writeCapabilityReport(t, reportPath, "eth0", nativeProofJSON(t, "eth1", now))
	if err := os.Chmod(reportPath, 0o666); err != nil {
		t.Fatal(err)
	}
	prober := NewCapabilityProber("", reportPath, "")
	prober.Now = func() time.Time { return now }
	observation := prober.Observe(context.Background(), ProbeTarget{ManagementInterface: "eth0", DataInterfaces: []string{"eth1"}})
	if observation.Source != ProbeSourceAbsent || !observation.Degraded {
		t.Fatalf("observation = %#v, want insecure report rejected as unavailable", observation)
	}
}

func containsEnv(env []string, value string) bool {
	for _, item := range env {
		if item == value {
			return true
		}
	}
	return false
}
