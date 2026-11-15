package httpapi

import "net/http"

func (server *Server) handleProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	// The console's 系统信息 card reads the build version from this endpoint.
	writeJSON(w, http.StatusOK, map[string]any{
		"product":      server.profile.ID(),
		"version":      server.version,
		"capabilities": server.profile.Capabilities(),
	})
}
