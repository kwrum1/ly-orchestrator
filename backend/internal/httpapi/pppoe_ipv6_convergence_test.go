package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePPPoEConvergenceFakeVPPCTL(t *testing.T, address, ra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vppctl")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  \"show interface address lyroute-enp0s4\")\n    echo " + shellQuoteForTest(address) + ";;\n" +
		"  \"show ip6 interface lyroute-enp0s4\")\n    echo " + shellQuoteForTest(ra) + ";;\n" +
		"  *)\n    echo \"unsupported: $*\" >&2\n    exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writePPPoEStatusFile(t *testing.T, wanID string, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wanID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWaitForPPPoEIPv6ConvergenceAcceptsDelegatedLAN(t *testing.T) {
	statusDir := writePPPoEStatusFile(t, "wan6",
		`{"state":"connected","interface":"pppoe_session_wan6","session":{"ipv6_ready":true,"delegated_prefix":"2001:db8:100::/56"}}`)
	t.Setenv("LY_ROUTE_PPPOE_STATUS_DIR", statusDir)
	t.Setenv("LY_ROUTE_VPPCTL", writePPPoEConvergenceFakeVPPCTL(t,
		"lyroute-enp0s4 2001:db8:100::1/64",
		"prefix 2001:db8:100::, length 64"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitForPPPoEIPv6Convergence(ctx, "wan6", []string{"lyroute-enp0s4"}, 0); err != nil {
		t.Fatalf("converged delegation was rejected: %v", err)
	}
}

func TestWaitForPPPoEIPv6ConvergenceReportsMissingPrefix(t *testing.T) {
	statusDir := writePPPoEStatusFile(t, "wan6",
		`{"state":"connected","interface":"pppoe_session_wan6","session":{"ipv6_ready":true}}`)
	t.Setenv("LY_ROUTE_PPPOE_STATUS_DIR", statusDir)
	t.Setenv("LY_ROUTE_VPPCTL", writePPPoEConvergenceFakeVPPCTL(t,
		"lyroute-enp0s4 192.168.77.2/24",
		""))
	t.Setenv("LY_ROUTE_PPPOE_PD_TIMEOUT_SECONDS", "30")
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	err := waitForPPPoEIPv6Convergence(ctx, "wan6", []string{"lyroute-enp0s4"}, 0)
	if err == nil {
		t.Fatal("a session without a delegated prefix must not converge")
	}
	if !strings.Contains(err.Error(), "delegated prefix is unavailable") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("unexpected convergence error: %v", err)
	}
}

func TestPPPoEIPv6ConvergenceTimeoutIsBounded(t *testing.T) {
	t.Setenv("LY_ROUTE_PPPOE_PD_TIMEOUT_SECONDS", "")
	if got := pppoeIPv6ConvergenceTimeout(); got != 180*time.Second {
		t.Fatalf("default timeout = %s, want 3m", got)
	}
	t.Setenv("LY_ROUTE_PPPOE_PD_TIMEOUT_SECONDS", "5")
	if got := pppoeIPv6ConvergenceTimeout(); got != 30*time.Second {
		t.Fatalf("minimum timeout = %s, want 30s", got)
	}
	t.Setenv("LY_ROUTE_PPPOE_PD_TIMEOUT_SECONDS", "9999")
	if got := pppoeIPv6ConvergenceTimeout(); got != 600*time.Second {
		t.Fatalf("maximum timeout = %s, want 10m", got)
	}
	t.Setenv("LY_ROUTE_PPPOE_PD_TIMEOUT_SECONDS", "120")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if deadline := pppoeIPv6ConvergenceDeadline(ctx); time.Until(deadline) > 5*time.Second {
		t.Fatal("convergence deadline must not exceed the apply context")
	}
}
