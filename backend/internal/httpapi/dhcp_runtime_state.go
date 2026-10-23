package httpapi

import (
	"context"

	"ly-route/backend/internal/runtime/apply"
)

// decorateDHCPRuntimeStates mirrors the UI's committed-state rule at the API
// boundary. A DHCP item is only marked applied when the last apply committed
// and Kea is actually running. This keeps the resource list truthful even if
// the browser's separate runtime query is slow or unavailable.
func (server *Server) decorateDHCPRuntimeStates(ctx context.Context, items []map[string]any) []map[string]any {
	result := server.runtimeEvidence()
	committed := result.Status == RuntimeStatusCommitted &&
		result.RuntimeState == RuntimeStateRunning &&
		result.Receipt.Status == apply.ReceiptApplied
	if !committed {
		return items
	}
	keaRunning := false
	for _, component := range server.statusHealthComponents(ctx) {
		if component.Name == "kea" && component.State == "running" && component.Available {
			keaRunning = true
			break
		}
	}
	if !keaRunning {
		return items
	}
	for _, item := range items {
		if stringField(item, "runtime_state") == "desired_not_applied" && desiredEnabled(item) {
			item["runtime_state"] = "applied"
		}
	}
	return items
}
