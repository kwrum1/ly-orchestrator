package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/vpp"
)

func orchestratorPackageServer(t *testing.T) (*Server, *orchestrator.Repository, *http.Cookie) {
	t.Helper()
	server, store, cookie := productTestServer(t, product.Orchestrator())
	repository, err := orchestrator.NewRepository(store, orchestrator.RepositoryOptions{Now: func() time.Time {
		return time.Date(2026, 7, 19, 3, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatalf("create orchestrator repository: %v", err)
	}
	server.orchestratorRepository = repository
	return server, repository, cookie
}

func seedOrchestratorPackageState(t *testing.T, repository *orchestrator.Repository) (orchestrator.Topology, orchestrator.Policy) {
	t.Helper()
	topology, err := orchestrator.ParseTopology(orchestrator.TopologyInput{
		SchemaVersion: orchestrator.SchemaVersion, ManagementInterface: "eth0",
		Interfaces: []orchestrator.InterfaceInput{
			{Name: "lan", Role: orchestrator.RoleLAN, Port: "eth1"},
			{Name: "wan", Role: orchestrator.RoleWAN, Port: "eth2"},
		},
	})
	if err != nil {
		t.Fatalf("parse topology: %v", err)
	}
	if err := repository.Replace(context.Background(), topology); err != nil {
		t.Fatalf("replace topology: %v", err)
	}
	policy, err := orchestrator.ParsePolicy(topology, orchestrator.PolicyInput{
		SchemaVersion: orchestrator.PolicySchemaVersion,
		IPObjects:     []orchestrator.IPObjectInput{{ID: "office", Prefixes: []string{"192.0.2.0/24"}}},
		Groups: []orchestrator.PolicyGroupInput{{
			ID: "group-a", Position: 10,
			Rules: []orchestrator.PolicyRuleInput{{
				ID: "allow-office", Sequence: 10,
				Match:  orchestrator.PolicyMatchInput{Sources: []string{"office"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolAny},
				Action: orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
			}},
		}},
		Default: orchestrator.ActionInput{Kind: orchestrator.ActionDrop},
	})
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	if err := repository.ReplacePolicy(context.Background(), policy); err != nil {
		t.Fatalf("replace policy: %v", err)
	}
	return topology, policy
}

func exportedConfigPackage(t *testing.T, server *Server, cookie *http.Cookie) ConfigPackagePayload {
	t.Helper()
	exported := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/export", "", cookie)
	if exported.Code != http.StatusOK {
		t.Fatalf("export status = %d: %s", exported.Code, exported.Body.String())
	}
	var body struct {
		Payload ConfigPackagePayload `json:"payload"`
	}
	decode(t, exported, &body)
	return body.Payload
}

func TestOrchestratorConfigExportIncludesTopologyAndPolicy(t *testing.T) {
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)

	payload := exportedConfigPackage(t, server, cookie)
	topologyItems := payload.Resources[orchestratorTopologyConfigResource]
	policyItems := payload.Resources[orchestratorPolicyConfigResource]
	if len(topologyItems) != 1 || len(policyItems) != 1 {
		t.Fatalf("orchestrator export = topology %d policy %d, want one each", len(topologyItems), len(policyItems))
	}
	var topology map[string]any
	if err := json.Unmarshal(topologyItems[0], &topology); err != nil {
		t.Fatalf("decode exported topology: %v", err)
	}
	if topology["management_interface"] != "eth0" {
		t.Fatalf("exported topology management = %#v", topology["management_interface"])
	}
	var policy map[string]any
	if err := json.Unmarshal(policyItems[0], &policy); err != nil {
		t.Fatalf("decode exported policy: %v", err)
	}
	if defaultAction, _ := policy["default"].(map[string]any); defaultAction["kind"] != "drop" {
		t.Fatalf("exported policy default = %#v", policy["default"])
	}
}

func TestOrchestratorSnapshotRestoreRestoresTopologyAndPolicy(t *testing.T) {
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/snapshots", `{"name":"orchestrator-roundtrip"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create snapshot status = %d: %s", created.Code, created.Body.String())
	}
	var createdBody struct {
		Snapshot struct {
			ID string `json:"id"`
		} `json:"snapshot"`
	}
	decode(t, created, &createdBody)

	changedTopology, err := orchestrator.ParseTopology(orchestrator.TopologyInput{
		SchemaVersion: orchestrator.SchemaVersion, ManagementInterface: "eth0",
		Interfaces: []orchestrator.InterfaceInput{
			{Name: "lan", Role: orchestrator.RoleLAN, Port: "eth3"},
			{Name: "wan", Role: orchestrator.RoleWAN, Port: "eth4"},
		},
	})
	if err != nil {
		t.Fatalf("parse changed topology: %v", err)
	}
	if err := repository.Replace(context.Background(), changedTopology); err != nil {
		t.Fatalf("replace changed topology: %v", err)
	}
	changedPolicy, err := orchestrator.ParsePolicy(changedTopology, orchestrator.PolicyInput{
		SchemaVersion: orchestrator.PolicySchemaVersion,
		Default:       orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
	})
	if err != nil {
		t.Fatalf("parse changed policy: %v", err)
	}
	if err := repository.ReplacePolicy(context.Background(), changedPolicy); err != nil {
		t.Fatalf("replace changed policy: %v", err)
	}

	restored := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/snapshots/"+createdBody.Snapshot.ID+"/restore", `{}`, cookie)
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status = %d: %s", restored.Code, restored.Body.String())
	}
	topology, _, err := repository.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("read restored topology: %v", err)
	}
	if view := topology.View(); view.Interfaces[0].Port != "eth1" || view.Interfaces[1].Port != "eth2" {
		t.Fatalf("restored topology = %#v", view.Interfaces)
	}
	policy, _, err := repository.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatalf("read restored policy: %v", err)
	}
	if policy.View().Default.Kind != orchestrator.ActionDrop {
		t.Fatalf("restored policy default = %q, want drop", policy.View().Default.Kind)
	}
}

func TestOrchestratorConfigImportRejectsPolicyThatDoesNotMatchTopology(t *testing.T) {
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)

	payload := exportedConfigPackage(t, server, cookie)
	payload.Resources[orchestratorPolicyConfigResource] = []json.RawMessage{json.RawMessage(`{
		"schema_version":1,
		"ip_objects":[],
		"policy_groups":[{"id":"group-a","position":10,"rules":[{"id":"bad","sequence":10,
			"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},
			"action":{"kind":"via","group":"missing-group"}}]}],
		"default":{"kind":"direct"}
	}`)}
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/import", configImportJSON(t, ConfigImportRequest{
		DryRun:          true,
		PackageManifest: manifestForPayload(payload),
		Payload:         payload,
	}), cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid orchestrator import status = %d: %s", response.Code, response.Body.String())
	}
}

func TestOrchestratorFactorySnapshotClearsTopologyAndPolicy(t *testing.T) {
	server, repository, _ := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	snapshot, err := defaultConfigSnapshot(product.Orchestrator())
	if err != nil {
		t.Fatalf("build default snapshot: %v", err)
	}
	if err := server.restoreConfigSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("restore default snapshot: %v", err)
	}
	if _, _, err := repository.Snapshot(context.Background()); !errors.Is(err, orchestrator.ErrTopologyNotFound) {
		t.Fatalf("topology after factory snapshot = %v, want ErrTopologyNotFound", err)
	}
	policyRepository := repository
	if _, _, err := policyRepository.PolicySnapshot(context.Background()); !errors.Is(err, orchestrator.ErrPolicyNotFound) && !errors.Is(err, orchestrator.ErrTopologyNotFound) {
		t.Fatalf("policy after factory snapshot = %v, want policy or topology not found", err)
	}
}

func TestOrchestratorLegacySnapshotPreservesCurrentState(t *testing.T) {
	server, repository, _ := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	legacy := ConfigPackagePayload{
		SchemaVersion: ConfigPackageSchemaVersion, ContentType: configContentType,
		Product: product.Orchestrator().ID(), DeviceMode: "orchestrator",
		Resources: map[string][]json.RawMessage{},
	}
	raw, hash, err := persistence.MarshalPayload(legacy)
	if err != nil {
		t.Fatalf("marshal legacy snapshot: %v", err)
	}
	if err := server.restoreConfigSnapshot(context.Background(), persistence.RuntimeSnapshot{Payload: raw, PayloadHash: hash}); err != nil {
		t.Fatalf("restore legacy snapshot: %v", err)
	}
	topology, _, err := repository.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("read topology after legacy restore: %v", err)
	}
	if topology.View().Interfaces[0].Port != "eth1" {
		t.Fatalf("legacy restore changed topology: %#v", topology.View().Interfaces)
	}
}

func TestOrchestratorRuntimeApplyReappliesPersistedGeneration(t *testing.T) {
	t.Setenv("LY_ROUTE_VPP_PROBE_COMMAND", filepath.Join(t.TempDir(), "missing-runtime-check"))
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	runtime := &recordingOrchestratorRuntime{}
	server.orchestratorRuntime = runtime

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusAccepted && response.Code != http.StatusOK {
		t.Fatalf("orchestrator runtime apply status = %d: %s", response.Code, response.Body.String())
	}
	if !runtime.applied || !runtime.committed {
		t.Fatalf("orchestrator runtime apply = applied=%t committed=%t", runtime.applied, runtime.committed)
	}
	if runtime.topology.ManagementInterface != "eth0" || runtime.policy == nil || runtime.policy.Default.Kind != orchestrator.ActionDrop {
		t.Fatalf("applied orchestrator generation = topology %#v policy %#v", runtime.topology, runtime.policy)
	}
}

type recordingOrchestratorRuntime struct {
	applied   bool
	committed bool
	topology  orchestrator.TopologyView
	policy    *orchestrator.PolicyView
}

func (runtime *recordingOrchestratorRuntime) ApplyServiceChain(context.Context, string, orchestrator.ServiceChain, []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	return vpp.ServiceChainApplyResult{}, nil
}

func (runtime *recordingOrchestratorRuntime) ApplyTransparent(_ context.Context, _ string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	runtime.applied = true
	runtime.topology = topology.View()
	if policy != nil {
		view := policy.View()
		runtime.policy = &view
	}
	return nil
}

func (runtime *recordingOrchestratorRuntime) DisableTransparent(context.Context, string) error {
	runtime.applied = true
	return nil
}

func (runtime *recordingOrchestratorRuntime) CommitTransparentTransaction(context.Context) error {
	runtime.committed = true
	return nil
}
