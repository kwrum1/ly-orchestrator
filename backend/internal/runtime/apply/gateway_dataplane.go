package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/vpp"
)

type DataplaneController interface {
	Apply(context.Context, dataplane.Request) (dataplane.Receipt, error)
	Rollback(context.Context, string) (dataplane.Receipt, error)
}

type productionGatewayWithDataplane struct {
	inner      GatewayTransactionRunner
	adapter    vpp.Adapter
	dataplane  DataplaneController
	mu         sync.Mutex
	dpdkActive map[string]bool
}

type ProductionGatewayComposition interface {
	GatewayResourceNames() []string
	HasDataplaneController() bool
}

func (transaction *productionGatewayWithDataplane) GatewayResourceNames() []string {
	return append([]string(nil), gatewayResourceOrder...)
}

func (transaction *productionGatewayWithDataplane) HasDataplaneController() bool {
	return transaction.dataplane != nil
}

func (transaction *productionGatewayWithDataplane) ApplyGraphSupported() bool {
	if provider, ok := transaction.inner.(ApplyGraphProvider); ok {
		return provider.ApplyGraphSupported()
	}
	return false
}

func NewProductionGatewayTransactionWithDataplane(adapter vpp.Adapter, controller DataplaneController, now Clock) GatewayTransactionRunner {
	return &productionGatewayWithDataplane{inner: NewProductionGatewayTransaction(adapter, now), adapter: adapter, dataplane: controller, dpdkActive: map[string]bool{}}
}

// attachNativeUnderlay materializes the AF_PACKET/AF_XDP/TAP native underlay
// before any higher-level resource adapter runs. The gateway resource adapters
// own addresses, routes, NAT and PPPoE; they deliberately do not create the
// host attachment. A fresh image therefore has to replay the attach operations
// explicitly, otherwise the first PPPoE operation targets a non-existent VPP
// interface and the transaction hangs.
func (transaction *productionGatewayWithDataplane) attachNativeUnderlay(ctx context.Context, plan Plan) error {
	operations, err := vpp.BuildOperations(plan.GatewayPlan)
	if err != nil {
		return fmt.Errorf("build native attach operations: %w", err)
	}
	attach := make([]vpp.Operation, 0, len(operations))
	for _, operation := range operations {
		if operation.Name == "vpp.dataplane.attach" {
			attach = append(attach, operation)
		}
	}
	if len(attach) == 0 {
		return nil
	}
	if transaction.adapter.Client == nil {
		return fmt.Errorf("vpp client is not configured for native attach")
	}
	return transaction.adapter.ExecuteOperations(ctx, attach)
}

// setPlanLANVPPEnvironment scopes the legacy VPP command placeholder to the
// LAN assignment of the current transaction. Existing command templates still
// contain $LY_ROUTE_LAN_INTERFACE, and the vppctl channel intentionally reads
// that process variable for backwards compatibility. Leaving it unset makes a
// command name literal "lyroute-$LY_ROUTE_...", which the VPP CLI cannot
// recover from; the plan is authoritative while the transaction runs.
func setPlanLANVPPEnvironment(plan vpp.Plan) func() {
	linux := ""
	for _, assignment := range plan.AddressAssignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			linux = strings.TrimSpace(assignment.LinuxInterface)
			break
		}
	}
	if linux == "" {
		// Older persisted plans may only contain rendered interface states.
		for _, state := range plan.Interfaces {
			name := strings.TrimSpace(state.Name)
			if strings.HasPrefix(name, "lyroute-") && len(state.Addresses) > 0 {
				linux = strings.TrimPrefix(name, "lyroute-")
				break
			}
		}
	}
	if linux == "" {
		return func() {}
	}
	previous, found := os.LookupEnv("LY_ROUTE_LAN_INTERFACE")
	_ = os.Setenv("LY_ROUTE_LAN_INTERFACE", linux)
	return func() {
		if found {
			_ = os.Setenv("LY_ROUTE_LAN_INTERFACE", previous)
			return
		}
		_ = os.Unsetenv("LY_ROUTE_LAN_INTERFACE")
	}
}

func (transaction *productionGatewayWithDataplane) Run(ctx context.Context, plan Plan) (GatewayTransactionResult, error) {
	if len(plan.GatewayPlan.NativePath.Assignments) == 0 {
		return transaction.inner.Run(ctx, plan)
	}
	path, err := vpp.SelectNativePath(plan.GatewayPlan.NativePath)
	if err != nil {
		return GatewayTransactionResult{}, err
	}
	if transaction.dataplane == nil {
		return GatewayTransactionResult{}, fmt.Errorf("dataplane controller is unavailable")
	}
	request := dataplane.Request{TransactionID: plan.Request.TransactionID, Path: path}
	dataplaneReceipt, err := transaction.dataplane.Apply(ctx, request)
	if err != nil {
		return GatewayTransactionResult{}, err
	}
	if path.Tier == vpp.DataplaneTierNative {
		if err := transaction.attachNativeUnderlay(ctx, plan); err != nil {
			return GatewayTransactionResult{}, fmt.Errorf("attach native VPP underlay: %w", err)
		}
	}
	restoreLAN := setPlanLANVPPEnvironment(plan.GatewayPlan)
	defer restoreLAN()
	plan.GatewayPlan.DataplanePrepared = true
	result, applyErr := transaction.inner.Run(ctx, plan)
	if applyErr != nil {
		if dataplaneReceipt.Changed {
			_, rollbackErr := transaction.dataplane.Rollback(ctx, request.TransactionID)
			applyErr = errors.Join(applyErr, rollbackErr)
		}
		return result, applyErr
	}
	if dataplaneReceipt.Changed {
		if rebinder, ok := transaction.dataplane.(dataplane.KeaRebinder); ok {
			if err := rebinder.RebindKea(ctx); err != nil {
				_, rollbackErr := transaction.dataplane.Rollback(ctx, request.TransactionID)
				return result, errors.Join(fmt.Errorf("rebind Kea after VPP restart: %w", err), rollbackErr)
			}
		}
		transaction.mu.Lock()
		transaction.dpdkActive[request.TransactionID] = true
		transaction.mu.Unlock()
	}
	return result, nil
}

func (transaction *productionGatewayWithDataplane) Rollback(ctx context.Context, plan Plan) error {
	innerErr := transaction.inner.Rollback(ctx, plan)
	transaction.mu.Lock()
	active := transaction.dpdkActive[plan.Request.TransactionID]
	delete(transaction.dpdkActive, plan.Request.TransactionID)
	transaction.mu.Unlock()
	if !active {
		return innerErr
	}
	_, dataplaneErr := transaction.dataplane.Rollback(ctx, plan.Request.TransactionID)
	return errors.Join(innerErr, dataplaneErr)
}
