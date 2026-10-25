package httpapi

import (
	"context"
	"errors"
	"fmt"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/product"
)

// applyPersistedOrchestratorState makes /api/v1/runtime/apply meaningful for
// the Orchestrator product: config import and snapshot restore persist desired
// topology and policy, and the operator then explicitly applies them.  The
// direct PUT paths use the same transparent runtime transaction machinery.
func (server *Server) applyPersistedOrchestratorState(ctx context.Context, transactionID string) error {
	if server.profile.ID() != product.Orchestrator().ID() || server.orchestratorRepository == nil {
		return nil
	}
	topology, _, err := server.orchestratorRepository.Snapshot(ctx)
	if errors.Is(err, orchestrator.ErrTopologyNotFound) {
		runtime, ok := server.orchestratorRuntime.(orchestratorapi.TransparentDisableRuntime)
		if !ok {
			return nil
		}
		if err := runtime.DisableTransparent(ctx, transactionID+"-disable"); err != nil {
			return fmt.Errorf("disable transparent orchestrator: %w", err)
		}
		return server.commitOrchestratorTransaction(ctx)
	}
	if err != nil {
		return fmt.Errorf("read orchestrator topology: %w", err)
	}
	var policy *orchestrator.Policy
	if repository, ok := server.orchestratorRepository.(orchestrator.PolicyRepository); ok {
		current, _, policyErr := repository.PolicySnapshot(ctx)
		switch {
		case policyErr == nil:
			policy = &current
		case errors.Is(policyErr, orchestrator.ErrPolicyNotFound):
		default:
			return fmt.Errorf("read orchestrator policy: %w", policyErr)
		}
	}
	runtime, ok := server.orchestratorRuntime.(orchestratorapi.TransparentRuntime)
	if !ok {
		return errors.New("transparent orchestrator runtime is not configured")
	}
	if err := runtime.ApplyTransparent(ctx, transactionID, topology, policy); err != nil {
		return fmt.Errorf("apply transparent orchestrator: %w", err)
	}
	return server.commitOrchestratorTransaction(ctx)
}

func (server *Server) commitOrchestratorTransaction(ctx context.Context) error {
	if runtime, ok := server.orchestratorRuntime.(orchestratorapi.TransparentTransactionRuntime); ok {
		if err := runtime.CommitTransparentTransaction(ctx); err != nil {
			return fmt.Errorf("commit transparent orchestrator transaction: %w", err)
		}
	}
	return nil
}
