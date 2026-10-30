package apply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

type injectRouteFailureClient struct {
	*desiredLiveClient
	injected bool
}

func (client *injectRouteFailureClient) OpenChannel(ctx context.Context) (vpp.Channel, error) {
	base, err := client.desiredLiveClient.OpenChannel(ctx)
	if err != nil {
		return nil, err
	}
	return &injectRouteFailureChannel{base: base.(*desiredLiveChannel), client: client}, nil
}

type injectRouteFailureChannel struct {
	base   *desiredLiveChannel
	client *injectRouteFailureClient
}

func (channel *injectRouteFailureChannel) Do(ctx context.Context, operation vpp.Operation) (vpp.Reply, error) {
	if !channel.client.injected && operation.Name == "vpp.route-policy" && !strings.HasSuffix(operation.Name, ".snapshot") {
		channel.client.injected = true
		channel.base.client.operations = append(channel.base.client.operations, operation)
		return vpp.Reply{}, errors.New("injected route apply failure")
	}
	return channel.base.Do(ctx, operation)
}

func (channel *injectRouteFailureChannel) Close() error { return channel.base.Close() }

// A failure after the interfaces node must restore the previous generation's
// interface addresses, verify them, and report the transaction as rolled back
// (not as a rollback failure).
func TestFailedGatewayApplyRestoresPriorLANState(t *testing.T) {
	prior, desired, live := productionGatewayPlans()
	prior.AllowUnprovenOperations = true
	desired.AllowUnprovenOperations = true
	prior.NativePath.AllowUnproven = true
	desired.NativePath.AllowUnproven = true
	client := &injectRouteFailureClient{desiredLiveClient: &desiredLiveClient{state: live}}
	clock := deterministicClock(time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC))
	transaction := NewProductionGatewayTransaction(vpp.Adapter{Client: client}, clock)
	plan := productionReconciliationPlan("txn-rollback-failure", prior, desired)

	result, err := transaction.Run(context.Background(), plan)
	if err == nil {
		t.Fatal("expected the injected route failure to fail the transaction")
	}
	var gatewayErr *GatewayTransactionError
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error = %v, want *GatewayTransactionError", err)
	}
	if gatewayErr.RollbackErr != nil {
		t.Fatalf("rollback failed: %v", gatewayErr.RollbackErr)
	}
	if result.Rollback.Status != ReceiptRolledBack {
		t.Fatalf("rollback status = %q, want rolled_back (rollback restored the previous generation)", result.Rollback.Status)
	}

	var restored, verified bool
	for _, operation := range client.operations {
		if operation.Name == "vpp.interface.address.verify" {
			verified = true
			for _, command := range operation.VPPCtlCommands {
				if strings.Contains(command, "192.0.2.2/24") {
					restored = true
				}
			}
		}
	}
	if !verified {
		t.Fatalf("rollback did not verify the restored interface addresses: %#v", client.operations)
	}
	if !restored {
		t.Fatalf("rollback did not replay the prior LAN address: %#v", client.operations)
	}
}

// Rollback cleanup must not queue a teardown for an interface the previous
// generation owned: deleting it (and bringing it down) is the addressless
// window the rollback has to avoid.
func TestRollbackCleanupKeepsSurvivingInterfaceAddress(t *testing.T) {
	prior := vpp.Snapshot{Interfaces: []vpp.InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.1/24"}}}}
	diff := vpp.GatewayDiff{Interfaces: vpp.InterfaceBondPlan{
		TransactionID: "txn-cleanup-window",
		Interfaces:    []vpp.InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.254/24"}}},
	}}
	adapter := &productionGatewayAdapter{name: "interfaces"}
	operations, err := adapter.cleanupOperations(diff, prior)
	if err != nil {
		t.Fatalf("cleanup operations: %v", err)
	}
	for _, operation := range operations {
		if strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "set interface state lyroute-ens256 down") {
			t.Fatalf("rollback cleanup brought the surviving LAN down: %#v", operation)
		}
	}
	if len(operations) != 0 {
		t.Fatalf("surviving interface was queued for cleanup: %#v", operations)
	}
}

type injectRollbackFailureClient struct {
	*desiredLiveClient
	applyFailed bool
}

func (client *injectRollbackFailureClient) OpenChannel(ctx context.Context) (vpp.Channel, error) {
	base, err := client.desiredLiveClient.OpenChannel(ctx)
	if err != nil {
		return nil, err
	}
	return &injectRollbackFailureChannel{base: base.(*desiredLiveChannel), client: client}, nil
}

type injectRollbackFailureChannel struct {
	base   *desiredLiveChannel
	client *injectRollbackFailureClient
}

func (channel *injectRollbackFailureChannel) Do(ctx context.Context, operation vpp.Operation) (vpp.Reply, error) {
	if !channel.client.applyFailed && operation.Name == "vpp.route-policy" && !strings.HasSuffix(operation.Name, ".snapshot") {
		channel.client.applyFailed = true
		channel.base.client.operations = append(channel.base.client.operations, operation)
		return vpp.Reply{}, errors.New("injected route apply failure")
	}
	if channel.client.applyFailed && strings.Contains(operation.Name, "rollback") {
		channel.base.client.operations = append(channel.base.client.operations, operation)
		return vpp.Reply{}, errors.New("injected rollback failure")
	}
	return channel.base.Do(ctx, operation)
}

func (channel *injectRollbackFailureChannel) Close() error { return channel.base.Close() }

// A rollback that itself fails must still leave the previous LAN address live
// and must report the failing step/object with a suggestion. The safety net is
// add-only, so it can repair a broken restore without another outage.
func TestFailedRollbackStillKeepsLANAndIsActionable(t *testing.T) {
	prior, desired, live := productionGatewayPlans()
	prior.AllowUnprovenOperations = true
	desired.AllowUnprovenOperations = true
	prior.NativePath.AllowUnproven = true
	desired.NativePath.AllowUnproven = true
	client := &injectRollbackFailureClient{desiredLiveClient: &desiredLiveClient{state: live}}
	transaction := NewProductionGatewayTransaction(vpp.Adapter{Client: client}, deterministicClock(time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)))
	plan := productionReconciliationPlan("txn-rollback-failure-twice", prior, desired)

	result, err := transaction.Run(context.Background(), plan)
	if err == nil {
		t.Fatal("expected the injected failure to fail the transaction")
	}
	var gatewayErr *GatewayTransactionError
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error = %v, want *GatewayTransactionError", err)
	}
	if gatewayErr.RollbackErr == nil {
		t.Fatal("expected the rollback failure to be reported")
	}
	if result.Rollback.Status != ReceiptFailed {
		t.Fatalf("rollback status = %q, want failed", result.Rollback.Status)
	}
	message := gatewayErr.RollbackErr.Error()
	if !strings.Contains(message, "rollback step") || !strings.Contains(message, "suggestion:") {
		t.Fatalf("rollback error is not actionable: %s", message)
	}

	// The interfaces safety net runs even though every restore step failed.
	var repaired bool
	for _, operation := range client.operations {
		if operation.Name != "vpp.interface.address.verify" {
			continue
		}
		if strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "192.0.2.2/24") {
			repaired = true
		}
	}
	if !repaired {
		t.Fatalf("rollback failure did not replay the previous LAN address: %#v", client.operations)
	}
}

// Rollback cleanup must never queue an LCP/native-attachment teardown for an
// object that also exists in the previous generation. When the prior plan
// cannot be compiled into operations at all, the helper fails safe: it returns
// no teardown instead of guessing which live prerequisite is disposable.
func TestRollbackCleanupKeepsPreexistingSupplementalObjects(t *testing.T) {
	prior := vpp.Plan{
		RequestID:          "txn-supplemental",
		AddressAssignments: []vpp.AddressAssignment{{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"}},
		Interfaces:         []vpp.InterfaceState{{Name: "lyroute-ens256", AdminState: "up", Addresses: []string{"192.168.10.1/24"}}},
	}
	desired := prior
	cleanup, err := vpp.SupplementalIntroducedCleanupOperations(prior, desired, vpp.SupplementalInterfaces)
	if err != nil {
		t.Fatalf("introduced cleanup must fail safe, got error: %v", err)
	}
	if len(cleanup) != 0 {
		for _, operation := range cleanup {
			if strings.Contains(operation.Name, "lan-control-lcp") || strings.Contains(operation.Name, "dataplane.attach") {
				t.Fatalf("preexisting prerequisite queued for rollback teardown: %#v", operation)
			}
		}
	}
}
