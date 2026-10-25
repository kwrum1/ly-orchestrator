package apply

import (
	"context"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/vpp"
)

// TestProductionGatewaySkipsQoSWithoutPluginAndRollsBack pins the runtime
// blocker: a VPP build without the QoS plugin must not plan or execute QoS
// operations, and a later rollback must not try to delete objects the
// dataplane never created.
func TestProductionGatewaySkipsQoSWithoutPluginAndRollsBack(t *testing.T) {
	prior, desired, live := productionGatewayPlans()
	// The shared fixture uses symbolic group names; use the real QoS plugin
	// kinds so the skip predicate is exercised exactly as production sees it.
	qosGroup := flow.VPPObjectGroup{Kind: "vpp.qos.classify", Objects: []flow.VPPObject{{RuleID: "voice", DSCP: "46"}}}
	prior.Flow.VPPGroups = []flow.VPPObjectGroup{qosGroup}
	desired.Flow.VPPGroups = []flow.VPPObjectGroup{qosGroup}
	live.QoS = []flow.VPPObjectGroup{qosGroup}
	desired.SkipQoS = true
	// This fixture is a pure declarative reconciliation: its native capability
	// proof expired long ago, exactly like a persisted prior generation. Allow
	// operation reconstruction so the test exercises QoS skipping rather than
	// an unrelated dataplane-lock prerequisite.
	prior.AllowUnprovenOperations = true
	desired.AllowUnprovenOperations = true
	prior.NativePath.AllowUnproven = true
	desired.NativePath.AllowUnproven = true
	client := &desiredLiveClient{state: live}
	clock := deterministicClock(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	transaction := NewProductionGatewayTransaction(vpp.Adapter{Client: client}, clock)
	plan := productionReconciliationPlan("txn-skip-qos", prior, desired)

	result, err := transaction.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply with the QoS plugin absent failed: %v", err)
	}
	if result.Rollback.Status != ReceiptApplied {
		t.Fatalf("apply did not commit: %#v", result.Rollback)
	}
	for _, operation := range client.mutations() {
		if operation.Name == "vpp.qos" || strings.Contains(operation.Name, "qos") {
			t.Fatalf("QoS was mutated without the plugin: %#v", operation)
		}
	}
	if result.Deletions["qos"] {
		t.Fatalf("QoS deletion was attempted without the plugin: %#v", result.Deletions)
	}
	if _, present := result.Deletions["qos"]; present {
		t.Fatalf("QoS has deletion evidence without the plugin: %#v", result.Deletions)
	}

	before := len(client.operations)
	if err := transaction.Rollback(context.Background(), plan); err != nil {
		t.Fatalf("rollback with the QoS plugin absent failed: %v", err)
	}
	for _, operation := range client.operations[before:] {
		if operation.Name == "vpp.qos" || strings.Contains(operation.Name, "qos") {
			t.Fatalf("rollback touched QoS without the plugin: %#v", operation)
		}
	}
}

// TestGatewayDiffSkipQoSKeepsPriorInventory is the planning half of the same
// contract: the live diff and readback request must omit QoS entirely so a
// prior generation cannot create a delete operation.
func TestGatewayDiffSkipQoSKeepsPriorInventory(t *testing.T) {
	prior, desired, live := productionGatewayPlans()
	priorQoS := len(prior.Flow.VPPGroups)
	if priorQoS == 0 {
		t.Fatal("test fixture prior plan has no QoS objects")
	}
	desired.SkipQoS = true
	request := vpp.GatewayDiffSnapshotRequest("txn-skip-qos", prior, desired)
	if len(request.QoS) != 0 || len(request.Candidates.QoS) != 0 {
		t.Fatalf("snapshot request still asks for QoS: %#v", request)
	}
	for _, capability := range request.Capabilities {
		if capability == vpp.SnapshotCapabilityQoS {
			t.Fatalf("snapshot request still requests the QoS capability: %#v", request.Capabilities)
		}
	}
	diff, err := vpp.ReconcileGatewayPlan(vpp.GatewayReconciliationInput{TransactionID: "txn-skip-qos", Prior: prior, Desired: desired, Live: live})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.QoS.QoS) != 0 || len(diff.QoS.DeleteQoS) != 0 {
		t.Fatalf("diff still manages QoS: %#v", diff.QoS)
	}
}
