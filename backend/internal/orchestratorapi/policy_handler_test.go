package orchestratorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

func TestPolicyPersistenceCompileAndServiceChainRoutes(t *testing.T) {
	_, server := newTestHTTPServer(t)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	policy := []byte(`{"schema_version":1,"ip_objects":[{"id":"office","prefixes":["192.0.2.0/24"]}],"policy_groups":[{"id":"route","position":10,"rules":[{"id":"via-east","sequence":10,"match":{"sources":["office"],"destinations":["any"],"protocol":"tcp","destination_ports":[{"start":443,"end":443}]},"action":{"kind":"via","group":"inline-east"}}]}],"default":{"kind":"direct"}}`)
	stored := sendRequest(t, server, http.MethodPut, PolicyPath, policy)
	if stored.StatusCode != http.StatusOK || !bytes.Contains(stored.Body, []byte(`"checksum"`)) {
		t.Fatalf("store policy: %d %s", stored.StatusCode, stored.Body)
	}
	compiled := sendRequest(t, server, http.MethodPost, PolicyCompilePath, []byte(`{"flow":{"source_ip":"192.0.2.10","destination_ip":"198.51.100.20","protocol":"tcp","source_port":41000,"destination_port":443},"prelude":{}}`))
	if compiled.StatusCode != http.StatusOK || !bytes.Contains(compiled.Body, []byte(`"traversal":["inline-east"]`)) {
		t.Fatalf("compile policy: %d %s", compiled.StatusCode, compiled.Body)
	}
	chain := sendRequest(t, server, http.MethodPost, ServiceChainCompilePath, []byte(`{"flow":{"source_ip":"192.0.2.10","destination_ip":"198.51.100.20","protocol":"tcp","source_port":41000,"destination_port":443},"prelude":{},"bindings":[{"group":"inline-east","wan_facing_next_hop":"198.51.100.2","lan_facing_next_hop":"192.0.2.2"}]}`))
	if chain.StatusCode != http.StatusOK || !bytes.Contains(chain.Body, []byte(`"direction":"forward"`)) || !bytes.Contains(chain.Body, []byte(`"direction":"reverse"`)) {
		t.Fatalf("compile chain: %d %s", chain.StatusCode, chain.Body)
	}
}

type healthTestRuntime struct {
	unavailable map[string]bool
	health      []orchestrator.GroupHealth
	applied     orchestrator.ServiceChain
}

func (runtime *healthTestRuntime) ServiceChainUnavailable(context.Context, []orchestrator.ServiceChainBindingInput) (map[string]bool, []orchestrator.GroupHealth, error) {
	return runtime.unavailable, runtime.health, nil
}

func (runtime *healthTestRuntime) ApplyServiceChain(_ context.Context, requestID string, chain orchestrator.ServiceChain, _ []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	runtime.applied = chain
	return vpp.ServiceChainApplyResult{Receipt: vpp.Receipt{RequestID: requestID}}, nil
}

func TestServiceChainApplyBypassesUnavailableGroup(t *testing.T) {
	runtime := &healthTestRuntime{unavailable: map[string]bool{"inline-east": true}, health: []orchestrator.GroupHealth{{Group: "inline-east", WANReachable: true, LANReachable: false, Unavailable: true, RequiredSuccesses: 3}}}
	repository, handler, server := newTestHTTPServerWithHandler(t, runtime)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	policy := []byte(`{"schema_version":1,"ip_objects":[{"id":"office","prefixes":["192.0.2.0/24"]}],"policy_groups":[{"id":"route","position":10,"rules":[{"id":"via-east","sequence":10,"match":{"sources":["office"],"destinations":["any"],"protocol":"tcp","destination_ports":[{"start":443,"end":443}]},"action":{"kind":"via","group":"inline-east"}}]}],"default":{"kind":"direct"}}`)
	if response := sendRequest(t, server, http.MethodPut, PolicyPath, policy); response.StatusCode != http.StatusOK {
		t.Fatalf("policy: %d %s", response.StatusCode, response.Body)
	}
	request := []byte(`{"flow":{"source_ip":"192.0.2.10","destination_ip":"198.51.100.20","protocol":"tcp","source_port":41000,"destination_port":443},"prelude":{},"bindings":[{"group":"inline-east","wan_facing_next_hop":"198.51.100.2","lan_facing_next_hop":"192.0.2.2"}],"attachments":[]}`)
	response := sendRequest(t, server, http.MethodPost, ServiceChainApplyPath, request)
	if response.StatusCode != http.StatusOK || !bytes.Contains(response.Body, []byte(`"direct":true`)) || !bytes.Contains(response.Body, []byte(`"bypassed_groups":["inline-east"]`)) || !bytes.Contains(response.Body, []byte(`"unavailable":true`)) {
		t.Fatalf("apply bypass: %d %s", response.StatusCode, response.Body)
	}
	if !runtime.applied.Direct || len(runtime.applied.BypassedGroups) != 1 {
		t.Fatalf("applied chain = %#v", runtime.applied)
	}
	intents, err := repository.ServiceChainIntents(context.Background())
	if err != nil || len(intents) != 1 {
		t.Fatalf("persisted intents = %#v, err=%v", intents, err)
	}
	runtime.unavailable = map[string]bool{}
	runtime.health = []orchestrator.GroupHealth{{Group: "inline-east", WANReachable: true, LANReachable: true, RequiredSuccesses: 3, RecoverySuccesses: 3}}
	results := handler.ReconcileServiceChains(context.Background())
	if len(results) != 1 || results[0].State != "applied" || len(results[0].BypassedGroups) != 0 || runtime.applied.Direct {
		t.Fatalf("reconcile results=%#v applied=%#v", results, runtime.applied)
	}
}

func TestPolicyPutPersistsCollectionOrderWithoutNumericFields(t *testing.T) {
	repository, server := newTestHTTPServer(t)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	rule := func(id, action string) string {
		return `{"id":"` + id + `","match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"` + action + `"}}`
	}
	policy := func(first, second string, firstRules, secondRules string) []byte {
		return []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[` +
			`{"id":"` + first + `","rules":[` + firstRules + `]},` +
			`{"id":"` + second + `","rules":[` + secondRules + `]}],` +
			`"default":{"kind":"direct"}}`)
	}
	forward := policy("alpha", "beta", rule("alpha-1", "direct")+","+rule("alpha-2", "drop"), rule("beta-1", "direct"))
	if response := sendRequest(t, server, http.MethodPut, PolicyPath, forward); response.StatusCode != http.StatusOK {
		t.Fatalf("forward policy: %d %s", response.StatusCode, response.Body)
	}
	stored, _, err := repository.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.View(); got.Groups[0].ID != "alpha" || got.Groups[1].ID != "beta" ||
		got.Groups[0].Rules[0].ID != "alpha-1" || got.Groups[0].Rules[1].ID != "alpha-2" {
		t.Fatalf("forward persisted order = %#v", got.Groups)
	}
	if got := stored.View(); got.Groups[0].Position != 10 || got.Groups[1].Position != 20 || got.Groups[0].Rules[0].Sequence != 10 || got.Groups[0].Rules[1].Sequence != 20 {
		t.Fatalf("derived ordinals = %#v", got.Groups)
	}

	reversed := policy("beta", "alpha", rule("beta-1", "direct"), rule("alpha-2", "drop")+","+rule("alpha-1", "direct"))
	if response := sendRequest(t, server, http.MethodPut, PolicyPath, reversed); response.StatusCode != http.StatusOK {
		t.Fatalf("reversed policy: %d %s", response.StatusCode, response.Body)
	}
	readback := sendRequest(t, server, http.MethodGet, PolicyPath, nil)
	if readback.StatusCode != http.StatusOK {
		t.Fatalf("readback: %d %s", readback.StatusCode, readback.Body)
	}
	var payload struct {
		Item orchestrator.PolicyView `json:"item"`
	}
	if err := json.Unmarshal(readback.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Item.Groups[0].ID != "beta" || payload.Item.Groups[1].ID != "alpha" ||
		payload.Item.Groups[1].Rules[0].ID != "alpha-2" || payload.Item.Groups[1].Rules[1].ID != "alpha-1" {
		t.Fatalf("reversed readback order = %#v", payload.Item.Groups)
	}
}
