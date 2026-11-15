package httpapi

// A read-only view of the orchestrator plugin's own CLI state. Support has no
// shell on the appliance, and almost every dataplane question ("which groups
// does the plugin know?", "is the span row there?") is answered by this output.

import (
	"context"
	"net/http"
	"time"
)

func (server *Server) handleOrchestratorPluginSnapshot(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, request, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	session, ok := server.sessionFromRequest(request)
	if !ok {
		writeError(writer, request, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if session.Role != "admin" {
		writeError(writer, request, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	ctx, cancel := contextWithTimeout(request, 15)
	defer cancel()
	output, err := execCommandOutput(ctx, vppctl, "show", "ly-route", "orchestrator")
	body := map[string]any{"command": "show ly-route orchestrator", "output": output}
	if err != nil {
		body["error"] = err.Error()
		writeJSON(writer, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(writer, http.StatusOK, body)
}

// contextWithTimeout keeps the helper independent of the caller's imports.
func contextWithTimeout(request *http.Request, seconds int) (ctx context.Context, cancel context.CancelFunc) {
	return context.WithTimeout(request.Context(), time.Duration(seconds)*time.Second)
}
