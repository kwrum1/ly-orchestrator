package httpapi

import (
	"context"
	"strings"

	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// decorateDNSRuntimeCapabilities replaces the legacy factory default
// "SmartDNS process manager not wired" marker with the live service state.
// The defaults remain useful for an unconfigured appliance, but once the
// process manager exists the readback must reflect smartdns health.
func (server *Server) decorateDNSRuntimeCapabilities(ctx context.Context, items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return items
	}
	capability := server.serviceCapability(ctx, serviceRuntime.SmartDNS, "smartdns", "SmartDNS service runtime is not configured")
	applied := runtimeTransactionCommitted(server.runtimeEvidence()) && capability.Available
	for _, item := range items {
		item["degraded"] = !capability.Available
		if capability.Available {
			delete(item, "degraded_reason")
		} else {
			reason := strings.TrimSpace(capability.Reason)
			if reason == "" {
				reason = "SmartDNS service runtime is unavailable"
			}
			item["degraded_reason"] = reason
		}
		// DNS resources use the same settled-transaction rule as every other
		// runtime resource. A committed transaction with a degraded component
		// (for example AF_PACKET compatibility) must not leave DNS readback
		// stuck at desired_not_applied while SmartDNS is serving it.
		if applied && desiredEnabled(item) {
			switch strings.TrimSpace(stringField(item, "runtime_state")) {
			case "", "desired_not_applied":
				item["runtime_state"] = "applied"
			}
		}
	}
	return items
}
