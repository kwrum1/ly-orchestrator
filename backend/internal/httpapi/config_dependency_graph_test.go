package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/configgraph"
	"ly-route/backend/internal/persistence"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// WS-K regression tests: referential integrity for the "strange operator"
// actions (delete a referenced WAN, clear a referenced LAN port, flip its
// direction, delete the last member of a group, repeat apply unchanged).

func configGraphTestServer(t *testing.T) (*Server, *http.Cookie, *persistence.Store) {
	t.Helper()
	previous := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{
			{"id": "eth0", "name": "eth0", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:00"},
			{"id": "eth2", "name": "eth2", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:02"},
			{"id": "eth3", "name": "eth3", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:03"},
		}
	}
	t.Cleanup(func() { hostInterfaceInventory = previous })
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "eth0")
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared"
	store, err := persistence.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithClock(fixedClock()))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
	}
	return server, login.Result().Cookies()[0], store
}

func seedGraphDocuments(t *testing.T, store *persistence.Store, documents ...persistence.ConfigDocument) {
	t.Helper()
	if err := store.SaveConfigs(context.Background(), documents); err != nil {
		t.Fatalf("seed graph documents: %v", err)
	}
}

func configDocumentAt(t *testing.T, resourceType, id string, payload map[string]any) persistence.ConfigDocument {
	t.Helper()
	return configDocument(t, resourceType, id, payload, fixedClock()())
}

type dependencyErrorBody struct {
	Error struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		ReferencedBy []struct {
			Type  string   `json:"type"`
			ID    string   `json:"id"`
			Field string   `json:"field"`
			Path  []string `json:"path"`
		} `json:"referenced_by"`
		Impact   json.RawMessage `json:"impact"`
		Resource struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"resource"`
		RequiresForce bool   `json:"requires_force"`
		ContentHash   string `json:"content_hash"`
	} `json:"error"`
}

func decodeDependencyError(t *testing.T, body string) dependencyErrorBody {
	t.Helper()
	var decoded dependencyErrorBody
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("conflict body is not JSON: %v\n%s", err, body)
	}
	return decoded
}

func TestConfigDependencyDeleteReferencedWANReturnsReferencedBy(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "wan_link", "wan-main", map[string]any{
			"id": "wan-main", "name": "Main WAN", "enabled": true, "interface_id": "eth2", "wan_type": "static",
		}),
		configDocumentAt(t, "route_policy", "route-office", map[string]any{
			"id": "route-office", "name": "Office", "enabled": true, "priority": float64(100),
			"action": "route", "egress": "wan-main",
		}),
	)

	deleted := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/gateway/wan-links/wan-main", "", cookie)
	if deleted.Code != http.StatusConflict {
		t.Fatalf("delete referenced WAN status = %d, want 409: %s", deleted.Code, deleted.Body.String())
	}
	body := decodeDependencyError(t, deleted.Body.String())
	if body.Error.Code != "referenced_by" {
		t.Fatalf("error code = %q, want referenced_by: %s", body.Error.Code, deleted.Body.String())
	}
	if body.Error.Resource.Type != "wan_link" || body.Error.Resource.ID != "wan-main" {
		t.Fatalf("conflict resource = %#v", body.Error.Resource)
	}
	if len(body.Error.ReferencedBy) != 1 {
		t.Fatalf("referenced_by = %#v, want exactly the route policy", body.Error.ReferencedBy)
	}
	reference := body.Error.ReferencedBy[0]
	if reference.Type != "route_policy" || reference.ID != "route-office" || reference.Field != "egress" {
		t.Fatalf("referenced_by entry = %#v", reference)
	}
	if len(reference.Path) < 2 || reference.Path[0] != "wan_link:wan-main" {
		t.Fatalf("reference path does not start at the WAN: %#v", reference.Path)
	}
	if body.Error.ContentHash == "" {
		t.Fatal("conflict did not report the graph content hash")
	}

	// The WAN is still there: a refused delete must not have side effects.
	if _, found, err := server.desiredItem(context.Background(), "wan_link", "wan-main"); err != nil || !found {
		t.Fatalf("refused delete removed the WAN: found=%v err=%v", found, err)
	}

	// Removing the referrer makes the delete legal again.
	if policyDelete := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/gateway/policies/routes/route-office", "", cookie); policyDelete.Code != http.StatusOK {
		t.Fatalf("delete route policy status = %d: %s", policyDelete.Code, policyDelete.Body.String())
	}
	second := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/gateway/wan-links/wan-main", "", cookie)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"deleted":true`) {
		t.Fatalf("delete unreferenced WAN status = %d: %s", second.Code, second.Body.String())
	}
}

func TestConfigDependencyDeleteLastGroupMemberFailsClosed(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "proxy_node", "node-hk", map[string]any{"id": "node-hk", "name": "HK", "enabled": true}),
		configDocumentAt(t, "proxy_group", "group-main", map[string]any{"id": "group-main", "name": "Main", "enabled": true, "members": []any{"node-hk"}}),
	)

	// Deleting the only member is refused rather than leaving an empty,
	// unappliable group behind. This is the repo's existing fail-closed model
	// (proxy_group requires at least one member) exposed through the graph.
	deleted := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/proxy/nodes/node-hk", "", cookie)
	if deleted.Code != http.StatusConflict {
		t.Fatalf("delete last group member status = %d, want 409: %s", deleted.Code, deleted.Body.String())
	}
	body := decodeDependencyError(t, deleted.Body.String())
	if len(body.Error.ReferencedBy) != 1 || body.Error.ReferencedBy[0].Type != "proxy_group" || body.Error.ReferencedBy[0].ID != "group-main" {
		t.Fatalf("referenced_by = %#v, want the proxy group", body.Error.ReferencedBy)
	}

	// Deleting the group first then the node succeeds.
	if groupDelete := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/proxy/groups/group-main", "", cookie); groupDelete.Code != http.StatusOK {
		t.Fatalf("delete group status = %d: %s", groupDelete.Code, groupDelete.Body.String())
	}
	if nodeDelete := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/proxy/nodes/node-hk", "", cookie); nodeDelete.Code != http.StatusOK {
		t.Fatalf("delete node status = %d: %s", nodeDelete.Code, nodeDelete.Body.String())
	}
}

func TestConfigDependencyGraphMarksEmptyGroupInvalid(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "proxy_group", "group-empty", map[string]any{"id": "group-empty", "name": "Empty", "enabled": true, "members": []any{}}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/dependencies", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("dependencies status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Invalid []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"invalid"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("dependencies body: %v", err)
	}
	found := false
	for _, entry := range payload.Invalid {
		if entry.ID == "group-empty" && entry.Reason == "empty_members" {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty proxy group is not marked invalid: %s", response.Body.String())
	}
}

func TestConfigDependencyClearReferencedPortIsRefused(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")
	seedInterfaceRoleReference(t, server, "route_policy", "route-lan", `{
		"id":"route-lan","name":"route-lan","enabled":true,"ingress_interface":"`+port+`","action":"route","egress":"wan-main"
	}`)

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port, `{"gateway_role":null}`, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("clear referenced port status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := decodeDependencyError(t, response.Body.String())
	if body.Error.Code != "interface_role_in_use" {
		t.Fatalf("error code = %q, want the legacy interface code: %s", body.Error.Code, response.Body.String())
	}
	if len(body.Error.ReferencedBy) == 0 || body.Error.ReferencedBy[0].Type != "route_policy" {
		t.Fatalf("referenced_by = %#v, want the route policy", body.Error.ReferencedBy)
	}
	if body.Error.Resource.ID != port {
		t.Fatalf("conflict resource = %#v", body.Error.Resource)
	}

	// Nothing was written: the line identity is intact.
	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after refusal: found=%v err=%v", found, err)
	}
	if item["gateway_role"] != "lan" || item["display_name"] != "lan-main" {
		t.Fatalf("refused clear mutated the document: %#v", item)
	}

	// The same write with force=true is accepted and reports the cascade.
	forced := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port, `{"gateway_role":null,"force":true}`, cookie)
	if forced.Code != http.StatusOK {
		t.Fatalf("forced clear status = %d, want 200: %s", forced.Code, forced.Body.String())
	}
	if !strings.Contains(forced.Body.String(), `"forced_rebuild":true`) || !strings.Contains(forced.Body.String(), `"cascade"`) {
		t.Fatalf("forced clear did not report the cascade: %s", forced.Body.String())
	}
}

func TestConfigDependencyLANToWANReferencedDirectionIsRefused(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")
	seedInterfaceRoleReference(t, server, "port_map", "port-map-lan", `{
		"id":"port-map-lan","name":"port-map-lan","enabled":true,"interface_id":"`+port+`",
		"protocol":"tcp","external_port":8080,"internal_host":"192.168.89.20","internal_port":80
	}`)

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port, `{"gateway_role":"wan"}`, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("LAN->WAN referenced status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := decodeDependencyError(t, response.Body.String())
	if len(body.Error.ReferencedBy) == 0 || body.Error.ReferencedBy[0].Type != "port_map" {
		t.Fatalf("referenced_by = %#v, want the port map", body.Error.ReferencedBy)
	}
	if !body.Error.RequiresForce {
		t.Fatal("direction change did not report requires_force")
	}
}

func TestConfigDependencyDeleteUnreferencedObjectSucceeds(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "wan_link", "wan-free", map[string]any{"id": "wan-free", "name": "Free", "enabled": true, "interface_id": "eth3"}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/gateway/wan-links/wan-free", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("delete unreferenced WAN status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestConfigDependencyGraphEndpointExposesOrderAndPhases(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "wan_link", "wan-main", map[string]any{"id": "wan-main", "enabled": true, "interface_id": "eth2"}),
		configDocumentAt(t, "route_policy", "route-office", map[string]any{"id": "route-office", "enabled": true, "action": "route", "egress": "wan-main"}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/dependencies", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("dependencies status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		GraphGeneration int64 `json:"graph_generation"`
		Nodes           []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"nodes"`
		Edges []struct {
			From struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"from"`
			To struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"to"`
			Field string `json:"field"`
		} `json:"edges"`
		CreateOrder []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"create_order"`
		DeleteOrder []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"delete_order"`
		Phases []struct {
			Name  string            `json:"name"`
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("dependencies body: %v\n%s", err, response.Body.String())
	}
	if len(payload.Nodes) == 0 || len(payload.Edges) == 0 || len(payload.Phases) == 0 {
		t.Fatalf("graph is empty: %s", response.Body.String())
	}
	indexOf := func(order []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}, kind, id string) int {
		for index, entry := range order {
			if entry.Type == kind && entry.ID == id {
				return index
			}
		}
		return -1
	}
	wanIndex := indexOf(payload.CreateOrder, "wan_link", "wan-main")
	policyIndex := indexOf(payload.CreateOrder, "route_policy", "route-office")
	if wanIndex < 0 || policyIndex < 0 || wanIndex >= policyIndex {
		t.Fatalf("create order does not build the WAN before the policy: %+v", payload.CreateOrder)
	}
	wanDelete := indexOf(payload.DeleteOrder, "wan_link", "wan-main")
	policyDelete := indexOf(payload.DeleteOrder, "route_policy", "route-office")
	if wanDelete < 0 || policyDelete < 0 || policyDelete >= wanDelete {
		t.Fatalf("delete order does not remove the policy before the WAN: %+v", payload.DeleteOrder)
	}
}

func TestConfigDependencyCycleBlocksApplyWithPath(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "object_group", "group-a", map[string]any{"id": "group-a", "name": "A", "enabled": true, "kind": "ip", "references": []any{"group-b"}}),
		configDocumentAt(t, "object_group", "group-b", map[string]any{"id": "group-b", "name": "B", "enabled": true, "kind": "ip", "references": []any{"group-a"}}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cyclic apply status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "dependency_cycle") || !strings.Contains(body, "object_group:group-a") || !strings.Contains(body, "object_group:group-b") {
		t.Fatalf("cyclic apply does not report the path: %s", body)
	}
	if strings.Contains(body, `"status":"committed"`) {
		t.Fatalf("cyclic apply reported a commit: %s", body)
	}
}

func TestConfigDependencyRepeatApplySkipsUnchangedGraph(t *testing.T) {
	server, cookie, _ := configGraphTestServer(t)
	// Creating through the API bumps the graph generation.
	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links", `{"id":"wan-main","name":"Main","enabled":true,"wan_type":"static","cidr":"10.1.18.24/24","gateway":"10.1.18.1"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create WAN status = %d: %s", created.Code, created.Body.String())
	}
	state, found, err := server.loadConfigGraphState(context.Background())
	if err != nil || !found || state.Generation == 0 {
		t.Fatalf("graph generation was not recorded: found=%v state=%+v err=%v", found, state, err)
	}
	graph, err := server.configGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server.recordConfigGraphApplied(context.Background(), "txn-first", graph)
	server.lastRuntime = &RuntimeApplyResult{Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning}

	repeated := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if repeated.Code != http.StatusOK || !strings.Contains(repeated.Body.String(), `"status":"skipped"`) {
		t.Fatalf("repeat apply status = %d, want 200 skipped: %s", repeated.Code, repeated.Body.String())
	}
	if !strings.Contains(repeated.Body.String(), "graph_generation") || !strings.Contains(repeated.Body.String(), "dependency_plan") {
		t.Fatalf("skip response lacks graph evidence: %s", repeated.Body.String())
	}

	// A configuration change invalidates the skip.
	second := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links", `{"id":"wan-backup","name":"Backup","enabled":true,"wan_type":"static","cidr":"10.1.19.24/24","gateway":"10.1.19.1"}`, cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("second WAN create status = %d: %s", second.Code, second.Body.String())
	}
	graph, err = server.configGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, _, _ = server.loadConfigGraphState(context.Background())
	decision := server.configGraphApplySkip(context.Background(), ConfigApplyRequest{}, graph, state)
	if decision.Skip {
		t.Fatalf("skip survived a configuration change: %+v", decision)
	}

	// A failed last apply also forbids a skip.
	server.recordConfigGraphApplyStatus(context.Background(), "failed", "txn-failed", "fault injected")
	state, _, _ = server.loadConfigGraphState(context.Background())
	decision = server.configGraphApplySkip(context.Background(), ConfigApplyRequest{}, graph, state)
	if decision.Skip {
		t.Fatalf("skip survived a failed last apply: %+v", decision)
	}
}

func TestConfigDependencyGenerationBumpsOnMutations(t *testing.T) {
	server, cookie, _ := configGraphTestServer(t)
	before, _, err := server.loadConfigGraphState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links", `{"id":"wan-one","name":"One","enabled":true,"wan_type":"static","cidr":"10.1.20.24/24","gateway":"10.1.20.1"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	afterCreate, found, err := server.loadConfigGraphState(context.Background())
	if err != nil || !found {
		t.Fatalf("state after create: found=%v err=%v", found, err)
	}
	if afterCreate.Generation <= before.Generation {
		t.Fatalf("generation did not increase on create: %d -> %d", before.Generation, afterCreate.Generation)
	}
	deleted := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/gateway/wan-links/wan-one", "", cookie)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	afterDelete, _, err := server.loadConfigGraphState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.Generation <= afterCreate.Generation {
		t.Fatalf("generation did not increase on delete: %d -> %d", afterCreate.Generation, afterDelete.Generation)
	}
}

func TestConfigDependencyGraphBuildWiresProxyChain(t *testing.T) {
	server, _, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "proxy_subscription", "sub-1", map[string]any{"id": "sub-1", "name": "Sub", "enabled": true}),
		configDocumentAt(t, "proxy_node", "node-1", map[string]any{"id": "node-1", "name": "Node", "enabled": true, "subscription_id": "sub-1"}),
		configDocumentAt(t, "proxy_group", "group-1", map[string]any{"id": "group-1", "name": "Group", "enabled": true, "members": []any{"node-1"}}),
		configDocumentAt(t, "proxy_egress", "egress-proxy", map[string]any{
			"id": "egress-proxy", "name": "Proxy", "enabled": true, "group_id": "group-1",
			"semantic_type": "proxy_egress", "display_list": "wan",
			"runtime_profile": "xray-tproxy-outbound", "capture_path": "vpp_service_interface",
			"engine": "xray", "handoff": "vpp_to_service", "listener_mode": "vpp-service",
		}),
		configDocumentAt(t, "route_policy", "route-proxy", map[string]any{"id": "route-proxy", "enabled": true, "action": "route", "egress": "egress-proxy"}),
	)
	graph, err := server.configGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	chain := graph.EnabledDependents(configgraph.NodeRef{Type: configgraph.KindProxySubscription, ID: "sub-1"}, true)
	ids := []string{}
	for _, dependent := range chain {
		ids = append(ids, dependent.Referrer.String())
	}
	for _, want := range []string{"proxy_node:node-1", "proxy_group:group-1", "proxy_egress:egress-proxy", "route_policy:route-proxy"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("proxy chain misses %s: %v", want, ids)
		}
	}
}

func TestConfigDependencyPhasesAppearInVPPPlanOutput(t *testing.T) {
	operations := []vpp.Operation{
		{Name: "vpp.interface.address", Resource: "eth2"},
		{Name: "vpp.route-policy", Resource: "route-office"},
		{Name: "vpp.dns.service-network", Resource: "dns"},
	}
	annotateConfigPhases(operations, &ConfigDependencyPlan{})
	want := []string{"interfaces", "routing", "proxy"}
	for index, phase := range want {
		if operations[index].ConfigPhase != phase {
			t.Fatalf("operation %s config_phase = %q, want %q", operations[index].Name, operations[index].ConfigPhase, phase)
		}
	}
	artifacts, err := serviceRuntime.RenderVPPOperations(operations)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("rendered artifacts = %d, want one operations.json", len(artifacts))
	}
	for _, required := range []string{`"config_phase": "interfaces"`, `"config_phase": "routing"`, `"config_phase": "proxy"`} {
		if !strings.Contains(artifacts[0].Content, required) {
			t.Fatalf("operations.json does not show the phase %s:\n%s", required, artifacts[0].Content)
		}
	}
}
func TestConfigDependencyWANRepointReferencedReturnsConflict(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "wan_link", "wan-main", map[string]any{"id": "wan-main", "name": "Main", "enabled": true, "interface_id": "eth2"}),
		configDocumentAt(t, "route_policy", "route-office", map[string]any{"id": "route-office", "enabled": true, "action": "route", "egress": "wan-main"}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/gateway/wan-links/wan-main", `{"interface_id":"eth3","cidr":"10.1.18.24/24","gateway":"10.1.18.1"}`, cookie)
	if response.Code != http.StatusConflict {
		t.Fatalf("re-point referenced WAN status = %d, want 409: %s", response.Code, response.Body.String())
	}
	body := decodeDependencyError(t, response.Body.String())
	if body.Error.Code != "referenced_by" || !body.Error.RequiresForce {
		t.Fatalf("re-point conflict = %s", response.Body.String())
	}
	if len(body.Error.ReferencedBy) == 0 || body.Error.ReferencedBy[0].Type != "route_policy" {
		t.Fatalf("referenced_by = %#v", body.Error.ReferencedBy)
	}
	// Nothing was persisted.
	item, found, err := server.desiredItem(context.Background(), "wan_link", "wan-main")
	if err != nil || !found {
		t.Fatalf("WAN missing after refusal: found=%v err=%v", found, err)
	}
	if item["interface_id"] != "eth2" {
		t.Fatalf("refused re-point mutated the WAN: %#v", item)
	}
}
func TestConfigDependencyRenameReferencedObjectIsAllowed(t *testing.T) {
	server, cookie, store := configGraphTestServer(t)
	seedGraphDocuments(t, store,
		configDocumentAt(t, "wan_link", "wan-main", map[string]any{
			"id": "wan-main", "name": "Main", "enabled": true, "interface_id": "eth2",
			"wan_type": "static", "cidr": "10.1.18.24/24", "gateway": "10.1.18.1",
		}),
		configDocumentAt(t, "route_policy", "route-office", map[string]any{"id": "route-office", "enabled": true, "action": "route", "egress": "wan-main"}),
	)
	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/gateway/wan-links/wan-main", `{"name":"Renamed WAN","description":"office uplink","mtu":1400}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("rename referenced WAN status = %d, want 200: %s", response.Code, response.Body.String())
	}
	item, found, err := server.desiredItem(context.Background(), "wan_link", "wan-main")
	if err != nil || !found {
		t.Fatalf("WAN missing after rename: found=%v err=%v", found, err)
	}
	if item["name"] != "Renamed WAN" || item["mtu"] != float64(1400) {
		t.Fatalf("non-identity fields were not updated: %#v", item)
	}
	// The reference set survived the rename.
	graph, err := server.configGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dependents := graph.EnabledDependents(configgraph.NodeRef{Type: configgraph.KindWANLink, ID: "wan-main"}, false); len(dependents) != 1 {
		t.Fatalf("rename changed the dependency graph: %#v", dependents)
	}
}
