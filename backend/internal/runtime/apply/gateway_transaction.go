package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

var ErrGatewayTransactionFailed = errors.New("gateway multi-resource transaction failed")

type GatewayResourceAdapter interface {
	Name() string
	Apply(context.Context, Plan) (GatewayResourceResult, error)
	Rollback(context.Context, Plan) error
}

type GatewayResourceApplicability interface {
	Applicable(Plan) bool
}

type GatewayResourceDeleter interface {
	Delete(context.Context, Plan) (bool, error)
}

type GatewayResourceRollbackPhases interface {
	RollbackCleanup(context.Context, Plan) error
	RollbackRestore(context.Context, Plan) error
}

type GatewayResourceResult struct {
	Receipt              ApplyReceipt
	Readback             Readback
	SupplementalReadback []vpp.SupplementalOperationReadback
	Deleted              bool
	Before               vpp.Snapshot
	After                vpp.Snapshot
}

type GatewayTransactionResult struct {
	Order      []string
	Receipts   []ApplyReceipt
	Readbacks  []Readback
	Deletions  map[string]bool
	Evidence   []GatewayResourceEvidence
	Rollback   RollbackReceipt
	FailedName string
}

type GatewayMultiResourceTransaction struct {
	Adapters []GatewayResourceAdapter
	Now      Clock
	Prepare  func(context.Context, Plan) error
	mu       sync.Mutex
	active   map[string]*gatewayGraphExecution
}

type GatewayTransactionRunner interface {
	Run(context.Context, Plan) (GatewayTransactionResult, error)
	Rollback(context.Context, Plan) error
}

// ApplyGraphProvider marks a runner that executes Plan.Hooks through the
// dependency graph. Legacy/test doubles do not implement it and are driven by
// the compatibility before/apply callbacks.
type ApplyGraphProvider interface {
	ApplyGraphSupported() bool
}

func (transaction *GatewayMultiResourceTransaction) Run(ctx context.Context, plan Plan) (GatewayTransactionResult, error) {
	if transaction.Prepare != nil {
		if err := transaction.Prepare(ctx, plan); err != nil {
			return GatewayTransactionResult{}, fmt.Errorf("gateway reconciliation prepare: %w", err)
		}
	}
	adapters, err := transaction.orderedAdapters(plan)
	if err != nil {
		return GatewayTransactionResult{}, err
	}
	now := transaction.now()
	execution := newGatewayGraphExecution(transaction, plan, adapters, now)
	result := GatewayTransactionResult{Deletions: map[string]bool{}}
	// Stale resources are removed in reverse dependency order before the graph
	// applies the desired generation.
	for index := len(adapters) - 1; index >= 0; index-- {
		adapter := adapters[index]
		deleter, ok := adapter.(GatewayResourceDeleter)
		if !ok {
			continue
		}
		resourcePlan := plan
		resourcePlan.Request.Resource = adapter.Name()
		deleted, deleteErr := deleter.Delete(ctx, resourcePlan)
		if deleteErr != nil {
			result.FailedName = adapter.Name()
			execution.restore[adapter.Name()] = true
			return transaction.fail(ctx, plan, result, execution, deleteErr, now)
		}
		if deleted {
			execution.restore[adapter.Name()] = true
			result.Deletions[adapter.Name()] = true
		}
	}
	graph, err := execution.buildGraph()
	if err != nil {
		return GatewayTransactionResult{}, fmt.Errorf("build gateway dependency graph: %w", err)
	}
	graphResult, runErr := graph.Run(ctx)
	execution.executed = append(execution.executed, graphResult.Executed...)
	for _, name := range execution.executed {
		if _, ok := execution.adapter(name); ok {
			execution.restore[name] = true
		}
	}
	if runErr != nil {
		var graphErr *GraphError
		if errors.As(runErr, &graphErr) && graphErr.Node != "" {
			result.FailedName = graphErr.Node
		}
		return transaction.fail(ctx, plan, result, execution, runErr, now)
	}
	result = execution.applyResult()
	result.Rollback = RollbackReceipt{TransactionID: plan.Request.TransactionID, Capability: plan.Request.Resource, Status: ReceiptApplied, Cause: "multi-resource transaction committed", Timestamp: now}
	transaction.mu.Lock()
	if transaction.active == nil {
		transaction.active = map[string]*gatewayGraphExecution{}
	}
	transaction.active[plan.Request.TransactionID] = execution
	transaction.mu.Unlock()
	return result, nil
}

func containsGatewayAdapter(adapters []GatewayResourceAdapter, wanted GatewayResourceAdapter) bool {
	for _, adapter := range adapters {
		if adapter.Name() == wanted.Name() {
			return true
		}
	}
	return false
}

func (transaction *GatewayMultiResourceTransaction) fail(ctx context.Context, plan Plan, result GatewayTransactionResult, execution *gatewayGraphExecution, cause error, now time.Time) (GatewayTransactionResult, error) {
	rollback := RollbackReceipt{TransactionID: plan.Request.TransactionID, Capability: plan.Request.Resource, Status: ReceiptRolledBack, Cause: cause.Error(), Timestamp: now}
	var rollbackErr error
	if execution != nil {
		rollbackErr = execution.rollback(ctx)
	}
	if rollbackErr != nil {
		rollback.Status = ReceiptFailed
		rollback.RollbackError = rollbackErr.Error()
	}
	result.Rollback = rollback
	result.Evidence = nil
	return result, &GatewayTransactionError{Resource: result.FailedName, Cause: cause, Rollback: rollback, RollbackErr: rollbackErr}
}

type GatewayTransactionError struct {
	Resource    string
	Cause       error
	Rollback    RollbackReceipt
	RollbackErr error
}

func (err *GatewayTransactionError) Error() string {
	if err.RollbackErr != nil {
		return fmt.Sprintf("%s: resource %s: %v; rollback: %v", ErrGatewayTransactionFailed, err.Resource, err.Cause, err.RollbackErr)
	}
	return fmt.Sprintf("%s: resource %s: %v", ErrGatewayTransactionFailed, err.Resource, err.Cause)
}

func (err *GatewayTransactionError) Unwrap() error {
	return errors.Join(err.Cause, err.RollbackErr)
}

func (err *GatewayTransactionError) Is(target error) bool {
	return target == ErrGatewayTransactionFailed
}

func (transaction *GatewayMultiResourceTransaction) Rollback(ctx context.Context, plan Plan) error {
	transaction.mu.Lock()
	execution := transaction.active[plan.Request.TransactionID]
	delete(transaction.active, plan.Request.TransactionID)
	transaction.mu.Unlock()
	if execution == nil {
		return nil
	}
	return execution.rollback(ctx)
}

func (transaction *GatewayMultiResourceTransaction) orderedAdapters(plan Plan) ([]GatewayResourceAdapter, error) {
	byName := make(map[string]GatewayResourceAdapter, len(transaction.Adapters))
	for _, adapter := range transaction.Adapters {
		if adapter == nil || strings.TrimSpace(adapter.Name()) == "" {
			return nil, fmt.Errorf("%w: resource adapter name is required", ErrGatewayTransactionFailed)
		}
		name := strings.TrimSpace(adapter.Name())
		if applicability, ok := adapter.(GatewayResourceApplicability); ok && !applicability.Applicable(plan) {
			continue
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("%w: duplicate resource adapter %q", ErrGatewayTransactionFailed, name)
		}
		byName[name] = adapter
	}
	ordered := make([]GatewayResourceAdapter, 0, len(transaction.Adapters))
	for _, name := range gatewayResourceOrder {
		if adapter, ok := byName[name]; ok {
			ordered = append(ordered, adapter)
			delete(byName, name)
		}
	}
	for name := range byName {
		return nil, fmt.Errorf("%w: unsupported resource adapter %q", ErrGatewayTransactionFailed, name)
	}
	return ordered, nil
}

func (transaction *GatewayMultiResourceTransaction) now() time.Time {
	if transaction.Now == nil {
		return time.Now().UTC()
	}
	return transaction.Now().UTC()
}

func (*GatewayMultiResourceTransaction) ApplyGraphSupported() bool { return true }
