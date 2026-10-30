package apply

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

func NewProductionGatewayTransaction(adapter vpp.Adapter, now Clock) *GatewayMultiResourceTransaction {
	recorder := vpp.NewExecutionRecorder()
	adapter.Client = vpp.NewRecordingClient(adapter.Client, recorder)
	return newProductionGatewayTransactionWithRecorder(adapter, now, recorder)
}

func newProductionGatewayTransactionWithRecorder(adapter vpp.Adapter, now Clock, recorder *vpp.ExecutionRecorder) *GatewayMultiResourceTransaction {
	reconciler := &productionGatewayReconciler{adapter: adapter}
	wanGroups := vpp.NewWANGroupsContext(nil, nil)
	transaction := &GatewayMultiResourceTransaction{Now: now, Prepare: reconciler.prepare, recorder: recorder}
	for _, name := range gatewayResourceOrder {
		transaction.Adapters = append(transaction.Adapters, &productionGatewayAdapter{name: name, reconciler: reconciler, now: now, deleted: map[string]GatewayResourceResult{}, wanGroups: &wanGroups})
	}
	return transaction
}

type productionGatewayReconciler struct {
	adapter vpp.Adapter
	mu      sync.Mutex
	diffs   map[string]vpp.GatewayDiff
	live    map[string]vpp.Snapshot
}

func (reconciler *productionGatewayReconciler) prepare(ctx context.Context, plan Plan) error {
	if plan.Previous.Available && plan.Previous.GatewayPlan == nil {
		return &LegacyGatewayPlanError{SnapshotID: plan.Request.PreviousSnapshotID}
	}
	prior := vpp.Plan{}
	if plan.Previous.GatewayPlan != nil {
		prior = *plan.Previous.GatewayPlan
	}
	request := vpp.GatewayDiffSnapshotRequest(plan.Request.TransactionID, prior, plan.GatewayPlan)
	live := vpp.Snapshot{RequestID: plan.Request.TransactionID, TransactionID: plan.Request.TransactionID}
	// A stale TAP can make even a no-op snapshot ambiguous. Clean managed
	// service handoffs before deciding whether the snapshot needs capabilities;
	// this is deliberately independent of the diff list.
	if err := reconciler.adapter.CleanupManagedTAPs(ctx, prior, plan.GatewayPlan); err != nil {
		return fmt.Errorf("managed TAP cleanup: %w", err)
	}
	// A port has exactly one dataplane owner. A leftover installer TAP with the
	// product name would make the snapshot ambiguous (or be duplicated by the
	// next rename), so release it before the diff is taken.
	if err := reconciler.adapter.CleanupDuplicateDataplaneTAPs(ctx, plan.GatewayPlan); err != nil {
		return fmt.Errorf("duplicate dataplane TAP cleanup: %w", err)
	}
	if len(request.Capabilities) > 0 {
		readback, err := reconciler.adapter.Snapshot(ctx, request)
		if err != nil {
			return fmt.Errorf("desired/live snapshot: %w", err)
		}
		live = readback
	}
	diff, err := vpp.ReconcileGatewayPlan(vpp.GatewayReconciliationInput{TransactionID: plan.Request.TransactionID, Prior: prior, Desired: plan.GatewayPlan, Live: live, RepairVerifiedDrift: true})
	if err != nil {
		return fmt.Errorf("desired/live diff: %w", err)
	}
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	if reconciler.diffs == nil {
		reconciler.diffs = map[string]vpp.GatewayDiff{}
		reconciler.live = map[string]vpp.Snapshot{}
	}
	reconciler.diffs[plan.Request.TransactionID] = diff
	reconciler.live[plan.Request.TransactionID] = live
	return nil
}

func (reconciler *productionGatewayReconciler) state(transactionID string) (vpp.GatewayDiff, vpp.Snapshot, bool) {
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	diff, found := reconciler.diffs[transactionID]
	return diff, reconciler.live[transactionID], found
}

type productionGatewayAdapter struct {
	name       string
	reconciler *productionGatewayReconciler
	now        Clock
	deleted    map[string]GatewayResourceResult
	wanGroups  *vpp.WANGroupsContext
}

func (adapter *productionGatewayAdapter) Name() string { return adapter.name }

func (adapter *productionGatewayAdapter) Applicable(plan Plan) bool {
	if adapter.name == "qos" && plan.GatewayPlan.SkipQoS {
		// The running VPP build has no QoS plugin. Never schedule an adapter
		// that could only fail planning, apply or rollback for an object the
		// dataplane cannot represent.
		return false
	}
	diff, live, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found {
		return false
	}
	return gatewayDiffApplies(adapter.name, diff) || gatewaySnapshotHasResource(adapter.name, live) || adapter.hasSupplemental(plan.GatewayPlan) || adapter.hasSupplementalReconciliation(plan)
}

func (adapter *productionGatewayAdapter) Apply(ctx context.Context, plan Plan) (GatewayResourceResult, error) {
	diff, prior, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found {
		return GatewayResourceResult{}, fmt.Errorf("%s apply: reconciliation is unavailable", adapter.name)
	}
	serviceNetworksDeleted := adapter.name == "service-networks" && supplementalServiceNetworksDeleted(plan)
	if adapter.name == "service-networks" && plan.Previous.GatewayPlan != nil {
		owner := adapter.supplementalOwner()
		equal, equalErr := vpp.SupplementalOperationsEqual(plan.GatewayPlan, *plan.Previous.GatewayPlan, owner)
		if equalErr == nil && equal {
			if readback, readErr := adapter.reconciler.adapter.ReadbackSupplemental(ctx, plan.GatewayPlan, owner); readErr == nil {
				result := adapter.result(plan.Request.TransactionID, false, prior, prior)
				result.SupplementalReadback = readback
				return result, nil
			}
			// A failed readback means the live handoff drifted (for example a
			// missing NAT44 role); fall through to the idempotent apply/repair.
		}
	}
	supplemental, err := adapter.reconcileSupplemental(ctx, plan)
	if err != nil {
		return GatewayResourceResult{}, fmt.Errorf("%s supplemental apply: %w", adapter.name, err)
	}
	if !gatewayDiffHasApply(adapter.name, diff) {
		result, deleted := adapter.deleted[plan.Request.TransactionID]
		if deleted {
			result.SupplementalReadback = supplemental
			return result, nil
		}
		// No mutation was emitted. The prepared snapshot is the live state;
		// do not commit this generation unless that live state actually
		// satisfies the desired plan. Committing an unexecuted mismatch is
		// what made the old live payload look like "desired applied but
		// different" and deadlocked every later apply.
		if err := adapter.verifyDesiredReadback(prior, plan.GatewayPlan); err != nil {
			return GatewayResourceResult{}, err
		}
		result = adapter.result(plan.Request.TransactionID, serviceNetworksDeleted, prior, prior)
		result.SupplementalReadback = supplemental
		return result, nil
	}
	_, err = adapter.applyDiff(ctx, diff, prior, plan.GatewayPlan, plan.Previous.GatewayPlan)
	if err != nil {
		return GatewayResourceResult{}, fmt.Errorf("%s apply: %w", adapter.name, err)
	}
	request, err := vpp.GatewayResourceSnapshotRequest(plan.Request.TransactionID, adapter.name, plan.GatewayPlan)
	if err != nil {
		return GatewayResourceResult{}, fmt.Errorf("%s desired readback request: %w", adapter.name, err)
	}
	after, err := adapter.reconciler.adapter.Snapshot(ctx, request)
	if err != nil {
		return GatewayResourceResult{}, fmt.Errorf("%s desired readback: %w", adapter.name, err)
	}
	if err := adapter.verifyDesiredReadback(after, plan.GatewayPlan); err != nil {
		return GatewayResourceResult{}, err
	}
	_, deleted := adapter.deleted[plan.Request.TransactionID]
	result := adapter.result(plan.Request.TransactionID, deleted || serviceNetworksDeleted, prior, after)
	result.SupplementalReadback = supplemental
	return result, nil
}

func (adapter *productionGatewayAdapter) supplementalOwner() vpp.SupplementalOwner {
	switch adapter.name {
	case "interfaces":
		return vpp.SupplementalInterfaces
	case "service-networks":
		return vpp.SupplementalServiceNetworks
	case "routes":
		return vpp.SupplementalRoutes
	case "acls":
		return vpp.SupplementalSecurity
	case "qos":
		return vpp.SupplementalQoS
	default:
		return ""
	}
}

// supplementalServiceNetworksDeleted reports a generation that removed every
// service network the prior generation owned. Such a generation has no
// service-networks row in the desired VPP plan (the TAP/NAT objects are
// supplemental operations), so its evidence must be marked as deletion
// evidence instead of being rejected as an unplanned live resource.
func supplementalServiceNetworksDeleted(plan Plan) bool {
	if plan.Previous.GatewayPlan == nil {
		return false
	}
	prior, desired := *plan.Previous.GatewayPlan, plan.GatewayPlan
	priorCount := len(prior.DNSServiceNetworks) + len(prior.Proxy.VPPSteering)
	desiredCount := len(desired.DNSServiceNetworks) + len(desired.Proxy.VPPSteering)
	return priorCount > 0 && desiredCount == 0
}

func (adapter *productionGatewayAdapter) hasSupplemental(plan vpp.Plan) bool {
	owner := adapter.supplementalOwner()
	return owner != "" && vpp.HasSupplementalOperations(plan, owner)
}

func (adapter *productionGatewayAdapter) supplementalChanged(plan Plan) (bool, error) {
	owner := adapter.supplementalOwner()
	if owner == "" {
		return false, nil
	}
	if plan.Previous.GatewayPlan == nil {
		return vpp.HasSupplementalOperations(plan.GatewayPlan, owner), nil
	}
	equal, err := vpp.SupplementalOperationsEqual(plan.GatewayPlan, *plan.Previous.GatewayPlan, owner)
	return !equal, err
}

func (adapter *productionGatewayAdapter) applySupplemental(ctx context.Context, plan vpp.Plan) ([]vpp.SupplementalOperationReadback, error) {
	owner := adapter.supplementalOwner()
	if owner == "" || !vpp.HasSupplementalOperations(plan, owner) {
		return nil, nil
	}
	return adapter.reconciler.adapter.ApplySupplemental(ctx, plan, owner)
}

func (adapter *productionGatewayAdapter) hasSupplementalReconciliation(plan Plan) bool {
	owner := adapter.supplementalOwner()
	if owner == "" || plan.Previous.GatewayPlan == nil {
		return false
	}
	cleanup, err := vpp.SupplementalReconciliationCleanupOperations(*plan.Previous.GatewayPlan, plan.GatewayPlan, owner)
	return err != nil || len(cleanup) > 0
}

func (adapter *productionGatewayAdapter) reconcileSupplemental(ctx context.Context, plan Plan) ([]vpp.SupplementalOperationReadback, error) {
	owner := adapter.supplementalOwner()
	if owner == "" {
		return nil, nil
	}
	if plan.Previous.GatewayPlan != nil {
		cleanup, err := vpp.SupplementalReconciliationCleanupOperations(*plan.Previous.GatewayPlan, plan.GatewayPlan, owner)
		if err != nil {
			return nil, err
		}
		if err := adapter.reconciler.adapter.ExecuteOperations(ctx, cleanup); err != nil {
			return nil, err
		}
	}
	return adapter.applySupplemental(ctx, plan.GatewayPlan)
}

func gatewaySnapshotHasResource(name string, snapshot vpp.Snapshot) bool {
	switch name {
	case "interfaces":
		return len(snapshot.Interfaces) > 0
	case "bonds":
		return len(snapshot.Bonds) > 0
	case "wan-groups":
		return len(snapshot.WANGroups) > 0
	case "routes":
		return len(snapshot.RoutePolicies) > 0
	case "acls":
		return len(snapshot.ACLs) > 0
	case "qos":
		return len(snapshot.QoS) > 0
	case "nat44":
		return len(snapshot.NAT.StaticMappings) > 0
	case "port-maps":
		return len(snapshot.NAT.PortMappings) > 0
	default:
		return false
	}
}

func (adapter *productionGatewayAdapter) Delete(ctx context.Context, plan Plan) (bool, error) {
	diff, prior, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found || !gatewayDiffHasDelete(adapter.name, diff) {
		return false, nil
	}
	after, err := adapter.deleteDiff(ctx, diff, prior, plan.GatewayPlan, plan.Previous.GatewayPlan)
	if err != nil {
		return false, fmt.Errorf("%s delete: %w", adapter.name, err)
	}
	adapter.deleted[plan.Request.TransactionID] = adapter.result(plan.Request.TransactionID, true, prior, after)
	return true, nil
}

func (adapter *productionGatewayAdapter) Rollback(ctx context.Context, plan Plan) error {
	diff, prior, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found {
		return fmt.Errorf("%s rollback: prior snapshot is unavailable", adapter.name)
	}
	return adapter.rollback(ctx, productionRollbackInput{transactionID: plan.Request.TransactionID, desired: plan.GatewayPlan, prior: prior, attempted: diff})
}

// verifyDesiredReadback keeps a mutation that silently did not take effect
// from being committed. Returning an error fails the gateway transaction, so
// the graph rolls back to the previous committed generation and the next
// apply can retry from a consistent base instead of staying degraded forever.
func (adapter *productionGatewayAdapter) verifyDesiredReadback(snapshot vpp.Snapshot, desired vpp.Plan) error {
	if adapter.name == "service-networks" {
		// Supplemental-only owner; verified through its operation readback.
		return nil
	}
	if vpp.GatewaySnapshotMatchesResource(adapter.name, snapshot, desired) {
		return nil
	}
	return fmt.Errorf("%s readback does not match the desired generation: %s; the previous committed generation was restored",
		adapter.name, vpp.DescribeGatewaySnapshotMismatch(adapter.name, snapshot, desired))
}

func (adapter *productionGatewayAdapter) result(transactionID string, deleted bool, before, after vpp.Snapshot) GatewayResourceResult {
	timestamp := adapter.timestamp()
	return GatewayResourceResult{
		Receipt:  ApplyReceipt{TransactionID: transactionID, Capability: adapter.name, Status: ReceiptApplied, AppliedAt: timestamp},
		Readback: Readback{TransactionID: transactionID, Capability: adapter.name, Timestamp: timestamp, Fresh: true},
		Deleted:  deleted,
		Before:   before,
		After:    after,
	}
}

func (adapter *productionGatewayAdapter) timestamp() time.Time {
	if adapter.now == nil {
		return time.Now().UTC()
	}
	return adapter.now().UTC()
}
