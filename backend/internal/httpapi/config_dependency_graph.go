package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/configgraph"
	"ly-route/backend/internal/persistence"
)

// Config dependency graph adapter.
//
// WS-K owns this file. It is deliberately additive: it does not change the
// existing per-resource validation in desired_resource_validation.go,
// interface_role_reference.go or proxy_group_validation.go, and the WS-I
// generation work (runtimeDesiredSourceHash / tryNoopRuntimeApply) keeps its
// own state. The two generations are related, not merged:
//
//   - graph_generation is a monotonic counter bumped by every desired-state
//     mutation. It answers "did the operator change configuration?".
//   - content_hash is the dependency graph fingerprint. It answers "did the
//     dependency structure change?". Apply skip requires both to match the
//     last successful apply (see config_dependency_apply.go).
//
// The graph itself is derived at read time; only the counter and the last
// applied bookkeeping are persisted, as resource_type=config_graph id=state.

const (
	configGraphStateResource = "config_graph"
	configGraphStateID       = "state"
)

type configGraphState struct {
	Generation             int64      `json:"generation"`
	ContentHash            string     `json:"content_hash,omitempty"`
	UpdatedAt              time.Time  `json:"updated_at"`
	LastAppliedGeneration  int64      `json:"last_applied_generation,omitempty"`
	LastAppliedContentHash string     `json:"last_applied_content_hash,omitempty"`
	LastApplyStatus        string     `json:"last_apply_status,omitempty"`
	LastApplyAt            *time.Time `json:"last_apply_at,omitempty"`
	LastTransactionID      string     `json:"last_transaction_id,omitempty"`
	LastReason             string     `json:"last_reason,omitempty"`
}

// configGraphKindForResource maps a desired-state resource_type onto a graph
// kind. Resource types without a kind (system_mode, dhcp_lease, snapshots)
// are not nodes.
func configGraphKindForResource(resourceType string) (configgraph.Kind, bool) {
	switch configgraph.Kind(resourceType) {
	case configgraph.KindInterface,
		configgraph.KindInterfaceBond,
		configgraph.KindManagementNetwork,
		configgraph.KindWANLink,
		configgraph.KindWANGroup,
		configgraph.KindObjectGroup,
		configgraph.KindPortMap,
		configgraph.KindNATStatic,
		configgraph.KindRoutePolicy,
		configgraph.KindProxyNode,
		configgraph.KindProxyGroup,
		configgraph.KindProxySubscription,
		configgraph.KindProxyEgress,
		configgraph.KindDNSUpstream,
		configgraph.KindDNSPolicy,
		configgraph.KindDomainIPSet,
		configgraph.KindDHCPServer,
		configgraph.KindDHCPStaticBinding,
		configgraph.KindSecurityACL,
		configgraph.KindSecurityIPMAC,
		configgraph.KindSecurityThreatIntel,
		configgraph.KindSecurityAttackRule,
		configgraph.KindTrafficControl:
		return configgraph.Kind(resourceType), true
	default:
		return "", false
	}
}

// configGraphSnapshot loads every desired document the graph understands.
// Failures are returned, not ignored: a guard that silently sees half the
// configuration would let a referenced object be deleted.
func (server *Server) configGraphSnapshot(ctx context.Context) (configgraph.Snapshot, error) {
	snapshot := configgraph.Snapshot{}
	if server.store == nil {
		return snapshot, nil
	}
	for _, kind := range configgraph.Kinds() {
		if kind == configgraph.KindDNSPolicy || kind == configgraph.KindProxyEgress {
			// DNS policies live in the policy store (one document per policy).
			// Proxy egress is a logical row derived from the proxy subsystem, not
			// a plain desired collection.
			continue
		}
		if !resourceAllowed(server.profile, string(kind)) {
			continue
		}
		items, err := server.desiredItems(ctx, string(kind))
		if err != nil {
			return configgraph.Snapshot{}, fmt.Errorf("%s: %w", kind, err)
		}
		for _, item := range items {
			if truthy(item["deleted"]) {
				continue
			}
			id := strings.TrimSpace(stringField(item, "id"))
			if id == "" {
				continue
			}
			snapshot.Documents = append(snapshot.Documents, configgraph.Document{
				Kind:    kind,
				ID:      id,
				Name:    configGraphDocumentName(kind, item),
				Enabled: desiredEnabled(item),
				Fields:  item,
			})
		}
	}
	if resourceAllowed(server.profile, string(configgraph.KindDNSPolicy)) {
		policies, err := server.store.Policies(ctx, "dns-policy")
		if err != nil {
			return configgraph.Snapshot{}, fmt.Errorf("dns_policy: %w", err)
		}
		for _, document := range policies {
			var fields map[string]any
			if len(document.Payload) > 0 {
				if err := json.Unmarshal(document.Payload, &fields); err != nil {
					return configgraph.Snapshot{}, fmt.Errorf("dns_policy %s: %w", document.PolicyID, err)
				}
			}
			if fields == nil {
				fields = map[string]any{}
			}
			fields["id"] = document.PolicyID
			snapshot.Documents = append(snapshot.Documents, configgraph.Document{
				Kind:    configgraph.KindDNSPolicy,
				ID:      document.PolicyID,
				Name:    configGraphDocumentName(configgraph.KindDNSPolicy, fields),
				Enabled: document.Enabled,
				Fields:  fields,
			})
		}
	}
	if resourceAllowed(server.profile, string(configgraph.KindProxyEgress)) {
		// Read the raw documents instead of proxyEgressResources: that helper
		// validates the full runtime profile, and a partially written egress
		// must still appear in the graph so the guard can name it instead of
		// failing the reference scan.
		documents, err := server.store.Configs(ctx, "proxy_egress")
		if err != nil {
			return configgraph.Snapshot{}, fmt.Errorf("proxy_egress: %w", err)
		}
		for _, document := range documents {
			var fields map[string]any
			if len(document.Payload) > 0 {
				if err := json.Unmarshal(document.Payload, &fields); err != nil {
					return configgraph.Snapshot{}, fmt.Errorf("proxy_egress %s: %w", document.ResourceID, err)
				}
			}
			if fields == nil {
				fields = map[string]any{}
			}
			if truthy(fields["deleted"]) {
				continue
			}
			id := strings.TrimSpace(firstNonEmpty(stringField(fields, "id"), document.ResourceID))
			if id == "" {
				continue
			}
			fields["id"] = id
			enabled := true
			if configured, ok := fields["enabled"].(bool); ok {
				enabled = configured
			}
			snapshot.Documents = append(snapshot.Documents, configgraph.Document{
				Kind:    configgraph.KindProxyEgress,
				ID:      id,
				Name:    strings.TrimSpace(firstNonEmpty(stringField(fields, "name"), id)),
				Enabled: enabled,
				Fields:  fields,
			})
		}
	}
	for _, identity := range server.interfaceIdentities(ctx) {
		if id := strings.TrimSpace(identity.ID); id != "" {
			snapshot.Ports = append(snapshot.Ports, id)
		}
	}
	snapshot.Resolve = func(kind configgraph.Kind, reference string) string {
		if kind != configgraph.KindInterface {
			return reference
		}
		if id := server.canonicalInterfaceReference(ctx, reference); isProductInterfaceID(id) {
			return id
		}
		return reference
	}
	return snapshot, nil
}

func configGraphDocumentName(kind configgraph.Kind, item map[string]any) string {
	if kind == configgraph.KindInterface {
		return strings.TrimSpace(firstNonEmpty(stringField(item, "display_name"), stringField(item, "name")))
	}
	return strings.TrimSpace(stringField(item, "name"))
}

// configGraph builds the runtime dependency view.
func (server *Server) configGraph(ctx context.Context) (*configgraph.Graph, error) {
	snapshot, err := server.configGraphSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return configgraph.Build(snapshot), nil
}

func (server *Server) loadConfigGraphState(ctx context.Context) (configGraphState, bool, error) {
	if server.store == nil {
		return configGraphState{}, false, nil
	}
	document, err := server.store.Config(ctx, configGraphStateResource, configGraphStateID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return configGraphState{}, false, nil
		}
		return configGraphState{}, false, err
	}
	var state configGraphState
	if err := json.Unmarshal(document.Payload, &state); err != nil {
		return configGraphState{}, false, err
	}
	return state, true, nil
}

func (server *Server) saveConfigGraphState(ctx context.Context, state configGraphState) error {
	if server.store == nil {
		return nil
	}
	raw, hash, err := persistence.MarshalPayload(state)
	if err != nil {
		return err
	}
	return server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: configGraphStateResource,
		ResourceID:   configGraphStateID,
		Payload:      raw,
		PayloadHash:  hash,
		UpdatedAt:    server.now().UTC(),
	})
}

// bumpConfigGraphGeneration records a configuration mutation. It is called
// after the document write succeeded, so a failed save never advances the
// generation. A state write failure is logged and swallowed: the content hash
// stored with the mutation is what the apply skip compares, and a missing
// counter can only make the next apply do real work, never skip it.
func (server *Server) bumpConfigGraphGeneration(ctx context.Context, action, resourceType, id, contentHash string) {
	if server.store == nil {
		return
	}
	server.configGraphMu.Lock()
	defer server.configGraphMu.Unlock()
	state, _, err := server.loadConfigGraphState(ctx)
	if err != nil {
		log.Printf("config graph: state read failed: %v", err)
	}
	state.Generation++
	if action != "" {
		state.LastReason = action + " " + resourceType + " " + id
	}
	// A mutation invalidates the cached content hash even when the caller did
	// not provide one: the value is recomputed at apply time, and the counter
	// alone already forbids a skip. Not building the graph here keeps a create
	// from paying for the full desired-state scan.
	state.ContentHash = contentHash
	state.UpdatedAt = server.now().UTC()
	if saveErr := server.saveConfigGraphState(ctx, state); saveErr != nil {
		log.Printf("config graph: state write failed: %v", saveErr)
	}
}

// configGraphGeneration returns the current counter and content hash. When no
// state exists yet (fresh install, imported legacy configuration) the counter
// is zero and the hash is computed from the live graph.
func (server *Server) configGraphGeneration(ctx context.Context) (int64, string, error) {
	graph, err := server.configGraph(ctx)
	if err != nil {
		return 0, "", err
	}
	return server.configGraphGenerationFor(ctx, graph)
}

func (server *Server) configGraphGenerationFor(ctx context.Context, graph *configgraph.Graph) (int64, string, error) {
	state, found, err := server.loadConfigGraphState(ctx)
	if err != nil {
		return 0, "", err
	}
	hash := graph.ContentHash()
	if !found {
		return 0, hash, nil
	}
	return state.Generation, hash, nil
}

// handleConfigDependencies serves GET /api/v1/config/dependencies: the whole
// graph plus the persisted generation bookkeeping. It is read-only and safe to
// call while an apply is running.
func (server *Server) handleConfigDependencies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if _, ok := server.sessionFromRequest(r); !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	graph, err := server.configGraph(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "dependency_graph_failed", err.Error())
		return
	}
	state, found, err := server.loadConfigGraphState(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "dependency_graph_failed", err.Error())
		return
	}
	invalid := []map[string]any{}
	for _, node := range graph.Nodes {
		if node.Invalid == "" {
			continue
		}
		invalid = append(invalid, map[string]any{"type": node.Type, "id": node.ID, "name": node.Name, "reason": node.Invalid})
	}
	response := map[string]any{
		"graph_generation": 0,
		"content_hash":     graph.ContentHash(),
		"generation_hash":  graph.ShortHash(),
		"nodes":            graph.Nodes,
		"edges":            graph.Edges,
		"dangling":         graph.Dangling,
		"create_order":     graph.CreateOrder,
		"delete_order":     graph.DeleteOrder,
		"phases":           graph.Phases,
		"cycles":           graph.Cycles,
		"invalid":          invalid,
		"summary": map[string]any{
			"nodes":    len(graph.Nodes),
			"edges":    len(graph.Edges),
			"dangling": len(graph.Dangling),
			"invalid":  len(invalid),
			"cycles":   len(graph.Cycles),
		},
		"request_id": requestID(r),
	}
	if found {
		response["graph_generation"] = state.Generation
		response["updated_at"] = state.UpdatedAt
		response["last_apply"] = map[string]any{
			"status":         state.LastApplyStatus,
			"generation":     state.LastAppliedGeneration,
			"content_hash":   state.LastAppliedContentHash,
			"transaction_id": state.LastTransactionID,
			"applied_at":     state.LastApplyAt,
			"reason":         state.LastReason,
		}
	}
	if len(graph.Cycles) > 0 {
		response["error"] = map[string]any{"code": "dependency_cycle", "message": graph.BuildError().Error()}
	}
	writeJSON(w, http.StatusOK, response)
}

// configGraphNodeRef resolves a resource spelling onto the graph node it
// addresses. Interface documents answer to the product id, so callers that
// pass a kernel name still hit the right node.
func (server *Server) configGraphNodeRef(ctx context.Context, resourceType, id string) (configgraph.NodeRef, bool) {
	kind, ok := configGraphKindForResource(resourceType)
	if !ok {
		return configgraph.NodeRef{}, false
	}
	id = strings.TrimSpace(id)
	if kind == configgraph.KindInterface && id != "" {
		if canonical := server.canonicalInterfaceReference(ctx, id); isProductInterfaceID(canonical) {
			// A logical LAN line document keeps its own id; a bare port
			// reference (kernel name, MAC, PCI) belongs to the port node.
			if _, found, err := server.desiredItem(ctx, "interface", id); err == nil && found {
				return configgraph.NodeRef{Type: kind, ID: id}, true
			}
			return configgraph.NodeRef{Type: kind, ID: canonical}, true
		}
	}
	return configgraph.NodeRef{Type: kind, ID: id}, true
}

// sortedKindsForResponse keeps a stable order for diagnostics.
func sortedKindsForResponse(kinds []configgraph.Kind) []string {
	result := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		result = append(result, string(kind))
	}
	sort.Strings(result)
	return result
}
