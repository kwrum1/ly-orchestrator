package httpapi

// A deliberately narrow diagnostic channel for acceptance testing.
//
// The appliance has no shell for support, yet traffic tests and dataplane
// forensics need the VPP CLI (`packet-generator`, `show ...`). This endpoint
// accepts only those two verbs, only for an admin session, and audits every
// call. It is not a general shell: the verb allow-list is checked per command.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

var diagnosticsVPPCTLVerbs = []string{"show ", "packet-generator "}

func (server *Server) handleOrchestratorDiagnosticsVPPCTL(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
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
	var payload struct {
		Commands []string `json:"commands"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024)).Decode(&payload); err != nil {
		writeError(writer, request, http.StatusBadRequest, "invalid_json", "request body must be {\"commands\":[...]}")
		return
	}
	if len(payload.Commands) == 0 || len(payload.Commands) > 16 {
		writeError(writer, request, http.StatusBadRequest, "invalid_commands", "1 to 16 commands are required")
		return
	}
	for _, command := range payload.Commands {
		trimmed := strings.TrimSpace(command)
		allowed := false
		for _, verb := range diagnosticsVPPCTLVerbs {
			if strings.HasPrefix(trimmed, verb) {
				allowed = true
				break
			}
		}
		if !allowed || strings.ContainsAny(trimmed, ";\n") {
			writeError(writer, request, http.StatusBadRequest, "command_not_allowed",
				"only 'show ...' and 'packet-generator ...' commands are accepted")
			return
		}
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	results := make([]map[string]any, 0, len(payload.Commands))
	for _, command := range payload.Commands {
		trimmed := strings.TrimSpace(command)
		ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
		output, err := execCommandOutput(ctx, vppctl, strings.Fields(trimmed)...)
		cancel()
		entry := map[string]any{"command": trimmed, "output": output}
		if err != nil {
			entry["error"] = err.Error()
		}
		results = append(results, entry)
	}
	server.recordAudit(session.Username, session.Role, "/api/v1/orchestrator/diagnostics/vppctl", "diagnostics", "success", "", request)
	writeJSON(writer, http.StatusOK, map[string]any{"results": results, "request_id": requestID(request)})
}
