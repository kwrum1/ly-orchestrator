package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/persistence"
)

// DataplanePolicy is the persisted, backend-owned dataplane setting.
//
// Takeover is automatic: the backend enumerates the physical data ports,
// excludes the management port and decides the mode from the built-in rule
// library (docs/TAKEOVER-OUT-OF-BOX.md). This document therefore only records
// audit-ish compatibility data. It has no field that can select a takeover
// mode, and the API rejects any attempt to send one.
type DataplanePolicy struct {
	// AFPacketFallback is retained for wire compatibility with older clients.
	// The compatibility path is now the automatic last resort and is always
	// available; the field never gates a takeover decision.
	AFPacketFallback bool `json:"af_packet_fallback"`
	// Ports is the legacy consent port list. Kept so an old document still
	// decodes; it does not influence the takeover decision.
	Ports     []string  `json:"ports"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// unsupportedTakeoverFields are the client-controlled takeover switches the
// product used to expose. They are now refused explicitly (422) instead of
// being ignored, so an old UI or script learns that the mode is not selectable.
var unsupportedTakeoverFields = []string{"takeover_switches", "confirmed", "takeover_mode", "mode"}

const (
	dataplanePolicyResource = "dataplane_policy"
	dataplanePolicyID       = "default"

	takeoverNotSelectableCode    = "takeover_not_selectable"
	takeoverNotSelectableMessage = "接管模式由后端根据网卡与规则库自动决定，不能手工选择；请删除 takeover_switches/confirmed 字段后重试"
)

func (server *Server) dataplanePolicy(ctx context.Context) DataplanePolicy {
	policy := DataplanePolicy{Ports: []string{}}
	if server.store == nil {
		return policy
	}
	documents, err := server.store.Configs(ctx, dataplanePolicyResource)
	if err != nil {
		return policy
	}
	for _, document := range documents {
		if document.ResourceID != dataplanePolicyID {
			continue
		}
		var decoded struct {
			AFPacketFallback bool      `json:"af_packet_fallback"`
			Ports            []string  `json:"ports"`
			UpdatedAt        time.Time `json:"updated_at"`
		}
		if err := json.Unmarshal(document.Payload, &decoded); err != nil {
			return policy
		}
		policy.AFPacketFallback = decoded.AFPacketFallback
		policy.Ports = server.canonicalPolicyPorts(ctx, decoded.Ports)
		policy.UpdatedAt = decoded.UpdatedAt
		return policy
	}
	return policy
}

func normalizePolicyPorts(ports []string) []string {
	result := make([]string, 0, len(ports))
	seen := map[string]bool{}
	for _, port := range ports {
		port = strings.TrimSpace(port)
		if port == "" || seen[port] {
			continue
		}
		seen[port] = true
		result = append(result, port)
	}
	return result
}

// canonicalPolicyPorts resolves saved legacy consent to product ids and
// persists only the canonical form. Linux names are accepted for backwards
// compatibility.
func (server *Server) canonicalPolicyPorts(ctx context.Context, ports []string) []string {
	result := make([]string, 0, len(ports))
	seen := map[string]bool{}
	for _, port := range normalizePolicyPorts(ports) {
		canonical := server.canonicalInterfaceID(ctx, port)
		if canonical == "" || seen[canonical] {
			continue
		}
		seen[canonical] = true
		result = append(result, canonical)
	}
	return result
}

// allowsAFPacketFor reports whether the compatibility path may be used.
//
// The out-of-the-box contract makes AF_PACKET the guaranteed last resort: the
// appliance must restore LAN/DHCP/DNS without operator input, so there is no
// consent gate left. The policy/name arguments are retained because callers
// still pass them, but they can no longer block the fallback.
func (server *Server) allowsAFPacketFor(_ context.Context, _ DataplanePolicy, _ string) bool {
	return true
}

func (server *Server) saveDataplanePolicy(ctx context.Context, policy DataplanePolicy) error {
	if server.store == nil {
		return fmt.Errorf("local store is not configured")
	}
	policy.Ports = server.canonicalPolicyPorts(ctx, policy.Ports)
	if policy.UpdatedAt.IsZero() {
		policy.UpdatedAt = server.now().UTC()
	}
	payload, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: dataplanePolicyResource,
		ResourceID:   dataplanePolicyID,
		Payload:      payload,
		PayloadHash:  hashObject(policy),
		UpdatedAt:    policy.UpdatedAt,
	})
}

// rejectManualTakeover returns a 422 when the request body names a takeover
// switch. Checking the raw body keeps the rejection explicit even when the
// decoder would otherwise ignore unknown fields.
func rejectManualTakeover(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false
	}
	for _, forbidden := range unsupportedTakeoverFields {
		if _, exists := fields[forbidden]; !exists {
			continue
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      takeoverNotSelectableCode,
			"message":    takeoverNotSelectableMessage,
			"field":      forbidden,
			"request_id": requestID(r),
		})
		return true
	}
	return false
}

func (server *Server) handleDataplanePolicy(w http.ResponseWriter, r *http.Request) {
	policy := server.dataplanePolicy(r.Context())
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"af_packet_fallback": policy.AFPacketFallback,
			"ports":              policy.Ports,
			// Read-only contract: the UI must not offer a mode selector.
			"takeover_automatic": true,
			"manual_takeover":    false,
			"takeover_selection": "backend",
			"request_id":         requestID(r),
		})
		return
	case http.MethodPut, http.MethodPatch, http.MethodPost:
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	session, ok := server.sessionFromRequest(r)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if session.Role != "admin" {
		writeError(w, r, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if server.passwordChangeRequired(w, r, session, r.URL.Path, strings.ToLower(r.Method)) {
		return
	}
	if server.store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "local store is not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body could not be read")
		return
	}
	if rejectManualTakeover(w, r, body) {
		server.recordAudit(session.Username, session.Role, r.URL.Path, "update", "denied", takeoverNotSelectableCode, r)
		return
	}
	var payload struct {
		AFPacketFallback *bool    `json:"af_packet_fallback"`
		Ports            []string `json:"ports"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.AFPacketFallback == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must contain af_packet_fallback")
		return
	}
	policy.AFPacketFallback = *payload.AFPacketFallback
	policy.Ports = server.canonicalPolicyPorts(r.Context(), payload.Ports)
	policy.UpdatedAt = server.now().UTC()
	if err := server.saveDataplanePolicy(r.Context(), policy); err != nil {
		writeError(w, r, http.StatusInternalServerError, "dataplane_policy_persist_failed", err.Error())
		return
	}
	server.recordAudit(session.Username, session.Role, r.URL.Path, "update", "success", "", r)
	writeJSON(w, http.StatusOK, map[string]any{
		"af_packet_fallback": policy.AFPacketFallback,
		"ports":              policy.Ports,
		"takeover_automatic": true,
		"manual_takeover":    false,
		"runtime_state":      "desired_not_applied",
		"request_id":         requestID(r),
	})
}
