package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// A freshly installed appliance has no data interface mapping yet, so the
// ownership preflight runs successfully without binding anything. That must be
// recorded as a failed rule-preferred attempt, otherwise the watcher would keep
// re-running the preflight and never adopt the AF_PACKET fallback.
func TestBootstrapRecordsAnAttemptWhenThePreflightBindsNothing(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(fixture.root, "inventory.json"))
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "4096")
	dataplaneBootstrapBlockedUntil = time.Time{}
	resetAllAutomaticModeSwitchBudgetsForTest()
	t.Cleanup(func() {
		dataplaneBootstrapBlockedUntil = time.Time{}
		resetAllAutomaticModeSwitchBudgetsForTest()
	})

	ctx := context.Background()
	if attempted := fixture.server.bootstrapDataplaneOwnership(ctx); attempted {
		t.Fatal("a preflight that bound no device was reported as an accomplished takeover")
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, "dpdk-attempts.json"))
	if err != nil {
		t.Fatalf("no durable attempt record: %v", err)
	}
	if !strings.Contains(string(raw), "enp2s0") || !strings.Contains(string(raw), `"dpdk_attempt_failed": true`) {
		t.Fatalf("attempt record = %s", raw)
	}
	if target, upgrade := fixture.server.automaticTakeoverUpgrade(ctx, "enp2s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{}); upgrade {
		t.Fatalf("the failed rule-preferred attempt was retried as %q", target)
	}
}
