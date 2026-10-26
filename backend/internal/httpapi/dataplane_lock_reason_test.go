package httpapi

import "testing"

// A dataplane lock must never be explained by an unrelated degradation note:
// the first Gate A cold-boot failure reported "qos plugin unavailable" as the
// reason the operator's interfaces were rejected.
func TestDataplaneLockReasonIgnoresUnrelatedWarnings(t *testing.T) {
	plan := RuntimePlan{
		DataplaneState: "dataplane_locked",
		Warnings: []string{
			"qos plugin unavailable: skipping QoS object management",
			"smartdns artifact not rendered",
		},
	}
	if got := dataplaneLockReason(plan); got != "dataplane prerequisites are not satisfied" {
		t.Fatalf("lock reason = %q, want the generic dataplane reason", got)
	}
}

func TestDataplaneLockReasonUsesDataplaneWarning(t *testing.T) {
	plan := RuntimePlan{
		DataplaneState: "dataplane_locked",
		Warnings: []string{
			"qos plugin unavailable: skipping QoS object management",
			"interface eth1: no verified takeover path",
		},
	}
	if got := dataplaneLockReason(plan); got != "interface eth1: no verified takeover path" {
		t.Fatalf("lock reason = %q, want the interface warning", got)
	}
}

func TestDataplaneLockReasonDegradedFallback(t *testing.T) {
	plan := RuntimePlan{DataplaneState: "dataplane_degraded"}
	if got := dataplaneLockReason(plan); got == "" || got == "dataplane prerequisites are not satisfied" {
		t.Fatalf("degraded lock reason = %q, want the observation message", got)
	}
}
