package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/persistence"
)

// DataplanePolicy is the persisted operator consent for the last-resort
// AF_PACKET compatibility path. Ports uses the product interface ids exposed by
// /api/v1/interfaces (eth0, eth1, ...). An empty list means every port that
// would otherwise require AF_PACKET.
type DataplanePolicy struct {
	AFPacketFallback bool      `json:"af_packet_fallback"`
	Ports            []string  `json:"ports"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

const (
	dataplanePolicyResource = "dataplane_policy"
	dataplanePolicyID       = "default"
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
		policy.Ports = normalizePolicyPorts(decoded.Ports)
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

// policyAllowsAFPort answers whether the operator consented to AF_PACKET for a
// specific product interface id. An empty ports list is the explicit "all
// ports that need it" form the UI sends after the consent modal.
func policyAllowsAFPort(policy DataplanePolicy, port string) bool {
	if !policy.AFPacketFallback {
		return false
	}
	if len(policy.Ports) == 0 {
		return true
	}
	port = strings.TrimSpace(port)
	for _, allowed := range policy.Ports {
		if strings.TrimSpace(allowed) == port {
			return true
		}
	}
	return false
}

func (server *Server) saveDataplanePolicy(ctx context.Context, policy DataplanePolicy) error {
	if server.store == nil {
		return fmt.Errorf("local store is not configured")
	}
	policy.Ports = normalizePolicyPorts(policy.Ports)
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

func (server *Server) handleDataplanePolicy(w http.ResponseWriter, r *http.Request) {
	policy := server.dataplanePolicy(r.Context())
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"af_packet_fallback": policy.AFPacketFallback,
			"ports":              policy.Ports,
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
	var payload struct {
		AFPacketFallback *bool    `json:"af_packet_fallback"`
		Ports            []string `json:"ports"`
	}
	if err := decodeStrictJSON(r, &payload); err != nil || payload.AFPacketFallback == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must contain af_packet_fallback")
		return
	}
	policy.AFPacketFallback = *payload.AFPacketFallback
	policy.Ports = normalizePolicyPorts(payload.Ports)
	policy.UpdatedAt = server.now().UTC()
	if err := server.saveDataplanePolicy(r.Context(), policy); err != nil {
		writeError(w, r, http.StatusInternalServerError, "dataplane_policy_persist_failed", err.Error())
		return
	}
	server.recordAudit(session.Username, session.Role, r.URL.Path, "update", "success", "", r)
	writeJSON(w, http.StatusOK, map[string]any{
		"af_packet_fallback": policy.AFPacketFallback,
		"ports":              policy.Ports,
		"runtime_state":      "desired_not_applied",
		"request_id":         requestID(r),
	})
}
