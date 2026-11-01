package vpp

import (
	"testing"
)

// TestGatewayVerifiedLiveDriftReappliesMissingResource pins the anti-wedge
// rule: after a verified readback, a still-desired object that is missing from
// the live snapshot (for example an ABF policy removed by the DNS data-path
// heal, or a VPP restart that replayed only part of the plan) must be queued
// for re-apply instead of failing every later apply forever.
func TestGatewayVerifiedLiveDriftReappliesMissingResource(t *testing.T) {
	prior, desired, live := gatewayDiffFixture()
	if len(desired.Policy.RoutePolicies) == 0 {
		t.Fatal("fixture has no route policies")
	}
	missing := desired.Policy.RoutePolicies[0]
	incomplete := live
	incomplete.RoutePolicies = nil
	for _, route := range live.RoutePolicies {
		if route.ID != missing.ID {
			incomplete.RoutePolicies = append(incomplete.RoutePolicies, route)
		}
	}

	diff, err := ReconcileGatewayPlan(GatewayReconciliationInput{
		TransactionID:       "txn-verified-drift",
		Prior:               prior,
		Desired:             desired,
		Live:                incomplete,
		RepairVerifiedDrift: true,
	})
	if err != nil {
		t.Fatalf("verified drift must be repairable, got %v", err)
	}
	found := false
	for _, route := range diff.Routes.Routes {
		if route.ID == missing.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing route %q was not queued for re-apply: %#v", missing.ID, diff.Routes.Routes)
	}
}
