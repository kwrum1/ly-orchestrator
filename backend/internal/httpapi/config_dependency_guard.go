package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ly-route/backend/internal/configgraph"
)

// Referential integrity guard.
//
// The rule: a persisted document that another persisted document names is not
// an independent object. Deleting or re-purposing it silently would produce a
// desired generation that either cannot compile (route policy names a WAN that
// is gone) or compiles into the wrong dataplane (a port served as LAN while its
// WAN line still owns it). The guard refuses such a write and reports every
// referrer, its field and a concrete next step.
//
// Two levels are enforced:
//
//   - delete: any enabled document that names the target blocks the delete with
//     409 referenced_by. There is no implicit cascade. The operator deletes the
//     referrer first (or re-points it), then deletes the target.
//   - identity change: changing the identity of a referenced object (interface
//     role, WAN port binding, bond membership) is a rebuild. It is refused with
//     422 referenced_by unless the request sets force=true; a forced rebuild
//     returns the affected objects as `cascade` so the UI can show the damage
//     before the next apply.
//
// Non-identity fields (name, description, MTU, notes) are always writable, even
// while referenced: renaming a WAN does not invalidate a policy that routes
// through it.

type configDependencyReference struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Field      string   `json:"field"`
	Depth      int      `json:"depth"`
	Path       []string `json:"path"`
	Enabled    bool     `json:"enabled"`
	Suggestion string   `json:"suggestion"`
}

type configDependencyConflict struct {
	Status           int
	Code             string
	Message          string
	Resource         configgraph.NodeRef
	References       []configDependencyReference
	Impact           []configDependencyReference
	Suggestion       string
	GraphGeneration  int64
	ContentHash      string
	RequiresForce    bool
	IdentityChange   bool
	Cascade          []configDependencyReference
	LegacyReferences []map[string]any
}

func (conflict configDependencyConflict) errorBody(requestID string) map[string]any {
	body := map[string]any{
		"code":       conflict.Code,
		"message":    conflict.Message,
		"request_id": requestID,
		"resource": map[string]any{
			"type": conflict.Resource.Type,
			"id":   conflict.Resource.ID,
		},
		"suggestion":       conflict.Suggestion,
		"referenced_by":    conflict.References,
		"graph_generation": conflict.GraphGeneration,
		"content_hash":     conflict.ContentHash,
	}
	if len(conflict.Impact) > 0 {
		body["impact"] = conflict.Impact
	}
	if conflict.RequiresForce {
		body["requires_force"] = true
	}
	if conflict.IdentityChange {
		body["identity_change"] = true
	}
	if len(conflict.Cascade) > 0 {
		body["cascade"] = conflict.Cascade
	}
	if len(conflict.LegacyReferences) > 0 {
		body["references"] = conflict.LegacyReferences
	}
	return body
}

// configDependencyReferences formats the enabled dependents of ref. Direct
// dependents block a write; the transitive list is attached as impact so the
// operator sees the whole chain (WAN -> WAN group -> route policy).
func (server *Server) configDependencyReferences(ctx context.Context, graph *configgraph.Graph, ref configgraph.NodeRef) ([]configDependencyReference, []configDependencyReference) {
	direct := graph.EnabledDependents(ref, false)
	transitive := graph.EnabledDependents(ref, true)
	return server.formatDependencyReferences(graph, direct, ref), server.formatDependencyReferences(graph, transitive, ref)
}

func (server *Server) formatDependencyReferences(graph *configgraph.Graph, dependents []configgraph.Dependent, target configgraph.NodeRef) []configDependencyReference {
	references := make([]configDependencyReference, 0, len(dependents))
	for _, dependent := range dependents {
		node, _ := graph.Node(dependent.Referrer)
		path := make([]string, 0, len(dependent.Path))
		for _, entry := range dependent.Path {
			path = append(path, entry.String())
		}
		references = append(references, configDependencyReference{
			Type:       string(dependent.Referrer.Type),
			ID:         dependent.Referrer.ID,
			Name:       node.Name,
			Field:      dependent.Field,
			Depth:      dependent.Depth,
			Path:       path,
			Enabled:    node.Enabled,
			Suggestion: dependencySuggestion(dependent.Referrer, dependent.Field, target),
		})
	}
	return references
}

func dependencySuggestion(referrer configgraph.NodeRef, field string, target configgraph.NodeRef) string {
	label := configgraph.Label(referrer.Type)
	switch referrer.Type {
	case configgraph.KindRoutePolicy:
		return fmt.Sprintf("re-point the %s egress (%s) or delete it first", label, referrer.ID)
	case configgraph.KindPortMap, configgraph.KindNATStatic:
		return fmt.Sprintf("re-point the %s WAN interface (%s) or delete it first", label, referrer.ID)
	case configgraph.KindProxyEgress:
		return fmt.Sprintf("re-point %s %s to another group/node/underlay or delete it first", label, referrer.ID)
	case configgraph.KindProxyGroup:
		return fmt.Sprintf("remove %s from %s %s (the group keeps at least one member) or delete the group first", target.ID, label, referrer.ID)
	case configgraph.KindWANGroup:
		return fmt.Sprintf("remove %s from %s %s or delete the group first", target.ID, label, referrer.ID)
	case configgraph.KindDHCPServer, configgraph.KindDHCPStaticBinding:
		return fmt.Sprintf("move the %s %s to another LAN port or delete it first", label, referrer.ID)
	case configgraph.KindInterfaceBond:
		return fmt.Sprintf("remove %s from %s %s or delete the bond first", target.ID, label, referrer.ID)
	case configgraph.KindDNSPolicy:
		return fmt.Sprintf("re-point the %s %s reference or delete the policy first", label, referrer.ID)
	default:
		if field != "" {
			return fmt.Sprintf("update %s %s so %s no longer names %s, or delete it first", label, referrer.ID, field, target.ID)
		}
		return fmt.Sprintf("update or delete %s %s first", label, referrer.ID)
	}
}

// configDependencyDeleteConflict computes the refusal for deleting a
// referenced object. A nil result means the delete may proceed.
func (server *Server) configDependencyDeleteConflict(ctx context.Context, resourceType, id string) (*configDependencyConflict, error) {
	ref, ok := server.configGraphNodeRef(ctx, resourceType, id)
	if !ok {
		return nil, nil
	}
	graph, err := server.configGraph(ctx)
	if err != nil {
		return nil, fmt.Errorf("dependency graph: %w", err)
	}
	if _, exists := graph.Node(ref); !exists {
		return nil, nil
	}
	direct, transitive := server.configDependencyReferences(ctx, graph, ref)
	if len(direct) == 0 {
		return nil, nil
	}
	generation, contentHash, _ := server.configGraphGeneration(ctx)
	message := fmt.Sprintf("%s %s is still referenced by %d object(s): %s",
		configgraph.Label(ref.Type), ref.ID, len(direct), dependencySummary(direct))
	return &configDependencyConflict{
		Status:          http.StatusConflict,
		Code:            "referenced_by",
		Message:         message,
		Resource:        ref,
		References:      direct,
		Impact:          transitive,
		Suggestion:      fmt.Sprintf("delete or re-point the referenced objects first; %s is not cascaded automatically", configgraph.Label(ref.Type)),
		GraphGeneration: generation,
		ContentHash:     contentHash,
	}, nil
}

func dependencySummary(references []configDependencyReference) string {
	parts := make([]string, 0, len(references))
	for _, reference := range references {
		label := configgraph.Label(configgraph.Kind(reference.Type))
		if reference.Field != "" {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", label, reference.ID, reference.Field))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", label, reference.ID))
	}
	return strings.Join(parts, ", ")
}

// writeConfigDependencyConflict renders the structured 409/422 body. It keeps
// the legacy code for resource types whose clients already branch on it while
// always adding referenced_by.
func writeConfigDependencyConflict(w http.ResponseWriter, r *http.Request, conflict configDependencyConflict) {
	status := conflict.Status
	if status == 0 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"error": conflict.errorBody(requestID(r))})
}

// legacyDependencyCode keeps the pre-WS-K code for the two resource types that
// already had a referential-integrity guard with published clients.
func legacyDependencyCode(resourceType string) (string, int, bool) {
	switch resourceType {
	case "interface":
		return "interface_role_in_use", http.StatusUnprocessableEntity, true
	case "object_group":
		return "object_group_in_use", http.StatusConflict, true
	default:
		return "", 0, false
	}
}

// writeLegacyDependencyConflict preserves the historical status/code and
// message shape of the interface and object-group guards while attaching the
// structured referenced_by list.
func writeLegacyDependencyConflict(w http.ResponseWriter, r *http.Request, resourceType string, ref configgraph.NodeRef, legacyReferences []map[string]any, message string, conflict *configDependencyConflict) {
	code, status, ok := legacyDependencyCode(resourceType)
	if !ok {
		if conflict != nil {
			writeConfigDependencyConflict(w, r, *conflict)
			return
		}
		writeError(w, r, http.StatusConflict, "referenced_by", message)
		return
	}
	var body map[string]any
	if conflict != nil {
		body = conflict.errorBody(requestID(r))
	} else {
		body = map[string]any{
			"code":          code,
			"message":       message,
			"request_id":    requestID(r),
			"resource":      map[string]any{"type": ref.Type, "id": ref.ID},
			"suggestion":    "delete or re-point the referenced objects first",
			"referenced_by": []configDependencyReference{},
		}
	}
	body["code"] = code
	body["message"] = message
	body["resource"] = map[string]any{"type": ref.Type, "id": ref.ID}
	if len(legacyReferences) > 0 {
		body["references"] = legacyReferences
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// identityFields returns the fields whose change turns an update into a
// rebuild. Only these fields are protected; renaming or re-addressing a
// referenced object stays an ordinary update.
func identityFields(resourceType string) []string {
	switch resourceType {
	case "interface":
		return []string{"interface_id", "gateway_role", "role", "kind", "type"}
	case "wan_link":
		return []string{"interface_id", "wan_type", "type", "vlan_id", "pppoe_peer_id"}
	case "interface_bond":
		return []string{"members"}
	case "dhcp_server", "dhcp_static_binding":
		return []string{"interface_id"}
	default:
		return nil
	}
}

func canonicalIdentityValue(resourceType, field string, value any) string {
	if value == nil {
		return ""
	}
	switch field {
	case "members":
		return strings.Join(stringSliceField(map[string]any{"members": value}, "members"), ",")
	default:
		text, _ := value.(string)
		return strings.ToLower(strings.TrimSpace(text))
	}
}

// configGraphIdentityChange reports whether writing payload over previous
// changes an identity field. It returns the changed field names.
func configGraphIdentityChange(resourceType string, previous, payload map[string]any) []string {
	fields := identityFields(resourceType)
	if len(fields) == 0 || previous == nil {
		return nil
	}
	changed := []string{}
	for _, field := range fields {
		before, hasBefore := previous[field]
		after, hasAfter := payload[field]
		if !hasBefore && !hasAfter {
			continue
		}
		if resourceType == "interface" && (field == "gateway_role" || field == "role") {
			// interfaceDesiredRole treats null and empty as the same "no role".
			beforeRole, _ := interfaceDesiredRole(previous)
			afterRole, _ := interfaceDesiredRole(payload)
			if beforeRole == afterRole {
				continue
			}
			changed = append(changed, field)
			continue
		}
		if canonicalIdentityValue(resourceType, field, before) == canonicalIdentityValue(resourceType, field, after) {
			continue
		}
		// A field that was never present and stays absent is not a change; the
		// merge step can add an empty default.
		if !hasBefore && valueEmpty(after) {
			continue
		}
		if hasBefore && valueEmpty(before) && !hasAfter {
			continue
		}
		changed = append(changed, field)
	}
	return changed
}

func valueEmpty(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	}
	return false
}

// configGraphIdentityConflict builds the 422 for a referenced identity change
// when the caller did not force the rebuild.
func (server *Server) configGraphIdentityConflict(ctx context.Context, resourceType, id string, previous, payload map[string]any) (*configDependencyConflict, error) {
	changed := configGraphIdentityChange(resourceType, previous, payload)
	if len(changed) == 0 {
		return nil, nil
	}
	ref, ok := server.configGraphNodeRef(ctx, resourceType, id)
	if !ok {
		return nil, nil
	}
	graph, err := server.configGraph(ctx)
	if err != nil {
		return nil, fmt.Errorf("dependency graph: %w", err)
	}
	if _, exists := graph.Node(ref); !exists {
		return nil, nil
	}
	direct, transitive := server.configDependencyReferences(ctx, graph, ref)
	direct = identityBlockingReferences(resourceType, direct)
	if len(direct) == 0 {
		return nil, nil
	}
	generation, contentHash, _ := server.configGraphGeneration(ctx)
	message := fmt.Sprintf("%s %s: changing %s rebuilds it while %d object(s) still reference it: %s",
		configgraph.Label(ref.Type), ref.ID, strings.Join(changed, ", "), len(direct), dependencySummary(direct))
	conflict := &configDependencyConflict{
		Status:          http.StatusConflict,
		Code:            "referenced_by",
		Message:         message,
		Resource:        ref,
		References:      direct,
		Impact:          transitive,
		Suggestion:      "update the referenced objects first, or repeat the write with force=true to rebuild and accept the cascade",
		GraphGeneration: generation,
		ContentHash:     contentHash,
		RequiresForce:   true,
		IdentityChange:  true,
		Cascade:         transitive,
	}
	if code, status, ok := legacyDependencyCode(resourceType); ok && resourceType == "interface" {
		conflict.Code = code
		conflict.Status = status
		// The legacy message shape is pinned by existing contract tests.
		conflict.Message = fmt.Sprintf("interface %s is still referenced by %s; update or delete it first (force=true rebuilds it)", ref.ID, legacyKindSummary(direct))
	}
	return conflict, nil
}

// identityBlockingReferences removes dependents that the existing write path
// owns and rewrites atomically. A DHCP service is meaningful only on a LAN
// port: prepareInterfaceRoleClear deletes it in the same transaction as the
// role clear and reports it as cascade_deleted, so it must not turn an
// established LAN re-purpose into a hard failure.
func identityBlockingReferences(resourceType string, references []configDependencyReference) []configDependencyReference {
	if resourceType != "interface" {
		return references
	}
	blocking := make([]configDependencyReference, 0, len(references))
	for _, reference := range references {
		switch configgraph.Kind(reference.Type) {
		case configgraph.KindDHCPServer, configgraph.KindDHCPStaticBinding:
			continue
		default:
			blocking = append(blocking, reference)
		}
	}
	return blocking
}

// legacyReferenceLabel matches the wording the pre-WS-K interface guards used.
// Existing clients and contract tests branch on these exact nouns.
func legacyReferenceLabel(kind configgraph.Kind) string {
	switch kind {
	case configgraph.KindWANLink:
		return "wan link"
	case configgraph.KindNATStatic:
		return "static NAT mapping"
	case configgraph.KindDHCPServer:
		return "DHCP service"
	case configgraph.KindDHCPStaticBinding:
		return "DHCP static binding"
	case configgraph.KindDNSUpstream:
		return "DNS upstream"
	case configgraph.KindProxyEgress:
		return "proxy egress underlay"
	case configgraph.KindInterfaceBond:
		return "interface bond"
	case configgraph.KindTrafficControl:
		return "traffic control policy"
	case configgraph.KindRoutePolicy:
		return "route policy"
	case configgraph.KindPortMap:
		return "port map"
	case configgraph.KindInterface:
		return "LAN line"
	default:
		return strings.ToLower(configgraph.Label(kind))
	}
}

func legacyKindSummary(references []configDependencyReference) string {
	parts := make([]string, 0, len(references))
	for _, reference := range references {
		parts = append(parts, fmt.Sprintf("%s %s", legacyReferenceLabel(configgraph.Kind(reference.Type)), reference.ID))
	}
	return strings.Join(parts, ", ")
}

// writeGraphDeleteGuard is the single delete entry point for resource types
// that do not have a legacy guard. It returns true when the request has been
// answered and the caller must stop.
func (server *Server) writeGraphDeleteGuard(w http.ResponseWriter, r *http.Request, resourceType, id string) (bool, error) {
	switch resourceType {
	case "interface", "object_group":
		// Handled by the pre-existing guards, which now also attach
		// referenced_by; re-checking here would duplicate the scan.
		return false, nil
	}
	conflict, err := server.configDependencyDeleteConflict(r.Context(), resourceType, id)
	if err != nil {
		return true, err
	}
	if conflict == nil {
		return false, nil
	}
	writeConfigDependencyConflict(w, r, *conflict)
	return true, nil
}

// forcedIdentityCascade lists the objects a forced rebuild leaves behind. They
// are not deleted: the next apply will compile them against the new identity.
func forcedIdentityCascade(ctx context.Context, server *Server, graph *configgraph.Graph, ref configgraph.NodeRef) []configDependencyReference {
	return server.formatDependencyReferences(graph, graph.EnabledDependents(ref, true), ref)
}

// errDependencyCycle is returned by the apply preflight when the graph has a
// cycle. It is a distinct error so the handler can answer 422 with the cycle
// path instead of a generic plan failure.
type errDependencyCycle struct {
	path string
}

func (err errDependencyCycle) Error() string {
	return "configuration dependency cycle: " + err.path
}

func dependencyCycleError(graph *configgraph.Graph) error {
	if len(graph.Cycles) == 0 {
		return nil
	}
	parts := []string{}
	for _, cycle := range graph.Cycles {
		refs := make([]string, 0, len(cycle))
		for _, ref := range cycle {
			refs = append(refs, ref.String())
		}
		parts = append(parts, strings.Join(refs, " -> "))
	}
	return errDependencyCycle{path: strings.Join(parts, "; ")}
}

var errDependencyGuardUnavailable = errors.New("dependency guard is unavailable")
