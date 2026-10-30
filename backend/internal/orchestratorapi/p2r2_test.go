package orchestratorapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestHandlerDeleteGroupReferencedByPolicyFailsFastWithGroupInUse(t *testing.T) {
	repository, server := newTestHTTPServer(t)
	topologyFixture, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	putTopology := sendRequest(t, server, http.MethodPut, TopologyPath, topologyFixture)
	if putTopology.StatusCode != http.StatusOK {
		t.Fatalf("PUT topology = %d: %s", putTopology.StatusCode, putTopology.Body)
	}
	policyBody := []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[{"id":"route","position":10,"rules":[{"id":"via-west","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"tcp"},"action":{"kind":"via","group":"inline-west"}}]}],"default":{"kind":"direct"}}`)
	putPolicy := sendRequest(t, server, http.MethodPut, PolicyPath, policyBody)
	if putPolicy.StatusCode != http.StatusOK {
		t.Fatalf("PUT policy = %d: %s", putPolicy.StatusCode, putPolicy.Body)
	}

	deleted := sendRequest(t, server, http.MethodDelete, OrchestrationGroupsPath+"/inline-west", nil)
	if deleted.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE referenced group = %d %s, want 422", deleted.StatusCode, deleted.Body)
	}
	var body errorResponse
	if err := json.Unmarshal(deleted.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "group_in_use" || !strings.Contains(body.Error.Message, "referenced by policy") {
		t.Fatalf("DELETE referenced group = %s", deleted.Body)
	}
	if strings.Contains(body.Error.Message, "via-west") {
		t.Fatalf("DELETE referenced group leaked rule identity: %s", deleted.Body)
	}
	currentTopology, _, err := repository.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	groupStillPresent := false
	for _, group := range currentTopology.View().Groups {
		if group.Name == "inline-west" {
			groupStillPresent = true
		}
	}
	if !groupStillPresent {
		t.Fatal("referenced group was removed")
	}
	if policy, _, err := repository.PolicySnapshot(context.Background()); err != nil || policy.View().Groups[0].Rules[0].ID != "via-west" {
		t.Fatalf("policy changed after rejected group delete: %#v, %v", policy.View(), err)
	}
}
