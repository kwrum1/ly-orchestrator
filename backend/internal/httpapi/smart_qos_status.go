package httpapi

import (
	"net/http"

	"ly-route/backend/internal/runtime/vpp"
)

// handleSmartQoSStatus preserves the retired endpoint so an old client gets an
// explicit, machine-readable answer instead of a route-not-found page. Built-in
// Smart QoS is retired in arch-v1: the plugin is no longer built or shipped and
// the dataplane never gates on it. QoS is provided by traffic-control and, once
// qualified, the external VPP QoS plugin.
func (server *Server) handleSmartQoSStatus(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusGone, "feature_retired", vpp.SmartQoSRetiredReason)
}
