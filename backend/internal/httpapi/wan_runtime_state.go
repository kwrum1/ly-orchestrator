package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// decorateWANLinkRuntimeStates reflects the live PPPoE status file in the
// desired-state readback. A successfully negotiated WAN must not continue to
// report desired_not_applied just because the declarative item is unchanged.
func (server *Server) decorateWANLinkRuntimeStates(ctx context.Context, items []map[string]any) []map[string]any {
	committed := wanLinkTransactionCommitted(server.runtimeEvidence())
	for _, item := range items {
		id := strings.TrimSpace(stringField(item, "id"))
		if id == "" {
			continue
		}
		handled := false
		data, err := os.ReadFile(filepath.Join("/run/ly-route/pppoe", id+".json"))
		if err == nil {
			var status struct {
				State      string `json:"state"`
				RouteReady *bool  `json:"route_ready"`
				Interface  string `json:"interface"`
			}
			if json.Unmarshal(data, &status) != nil {
				continue
			}
			handled = true
			item["pppoe_state"] = status.State
			item["pppoe_interface"] = status.Interface
			if status.State == "connected" {
				item["runtime_state"] = "running"
				if status.RouteReady != nil {
					item["route_ready"] = *status.RouteReady
				} else {
					// A connected PPPoE session without an explicit readiness field
					// has installed its default route; reflect that rather than the
					// zero value of an absent JSON field.
					item["route_ready"] = true
				}
			} else if status.State != "" {
				item["runtime_state"] = status.State
			}
		}
		// Static and DHCP WANs have no PPPoE status file. Once the runtime
		// transaction is committed and the receipt is applied, leaving them at
		// desired_not_applied makes the UI report a permanent pending state.
		if !handled && wanLinkAppliedAfterCommit(committed, item) {
			item["runtime_state"] = "applied"
		}
	}
	return items
}

// wanLinkTransactionCommitted reports whether the last runtime transaction
// actually committed. A component-level degradation (for example AF_PACKET
// compatibility) still means the gateway resource graph was applied; it must
// not leave WAN links permanently pending. A failed or unreconciled
// transaction never promotes anything.
func wanLinkTransactionCommitted(result RuntimeApplyResult) bool {
	return runtimeTransactionCommitted(result)
}

// wanLinkAppliedAfterCommit is deliberately conservative: PPPoE keeps its own
// live session state, and only an enabled link still marked pending is
// promoted after a committed transaction.
func wanLinkAppliedAfterCommit(committed bool, item map[string]any) bool {
	if !committed || !desiredEnabled(item) {
		return false
	}
	if stringField(item, "runtime_state") != "desired_not_applied" {
		return false
	}
	return firstStringField(item, "wan_type", "type") != "pppoe"
}
