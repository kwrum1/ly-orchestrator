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
	for _, item := range items {
		id := strings.TrimSpace(stringField(item, "id"))
		if id == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/run/ly-route/pppoe", id+".json"))
		if err != nil {
			continue
		}
		var status struct {
			State      string `json:"state"`
			RouteReady bool   `json:"route_ready"`
			Interface  string `json:"interface"`
		}
		if json.Unmarshal(data, &status) != nil {
			continue
		}
		item["pppoe_state"] = status.State
		item["pppoe_interface"] = status.Interface
		if status.State == "connected" {
			item["runtime_state"] = "running"
			item["route_ready"] = status.RouteReady
		} else if status.State != "" {
			item["runtime_state"] = status.State
		}
	}
	return items
}
