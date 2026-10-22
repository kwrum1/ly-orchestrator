package apply

import (
	"context"
	"fmt"
	"time"
)

// gatewayResourceOrder is the graph's declaration order.  It is not the
// execution contract: gatewayResourceDependencies defines the edges and the
// graph derives a stable topological order from them.
var gatewayResourceOrder = []string{
	"interfaces",
	"bonds",
	"wan-groups",
	"service-networks",
	"routes",
	"acls",
	"qos",
	"nat44",
	"port-maps",
}

var gatewayResourceDependencies = map[string][]string{
	"interfaces":       {},
	"bonds":            {"interfaces"},
	"wan-groups":       {"bonds"},
	"service-networks": {"interfaces", "pppoe"},
	"routes":           {"wan-groups", "service-networks"},
	"acls":             {"routes"},
	"qos":              {"routes", "interfaces"},
	"nat44":            {"interfaces", "service-networks"},
	"port-maps":        {"nat44"},
}

const (
	nodePPPoE           = "pppoe"
	nodeServiceNetworks = "service-networks"
	nodeServices        = "services"
)

// gatewayGraphExecution owns the request-scoped state of one graph run so the
// long-lived transaction can be reused without leaking receipts between
// requests.
type gatewayGraphExecution struct {
	transaction *GatewayMultiResourceTransaction
	plan        Plan
	adapters    []GatewayResourceAdapter
	results     map[string]GatewayResourceResult
	evidence    []GatewayResourceEvidence
	deletions   map[string]bool
	executed    []string
	restore     map[string]bool
	now         time.Time
}

func newGatewayGraphExecution(transaction *GatewayMultiResourceTransaction, plan Plan, adapters []GatewayResourceAdapter, now time.Time) *gatewayGraphExecution {
	return &gatewayGraphExecution{
		transaction: transaction,
		plan:        plan,
		adapters:    adapters,
		results:     map[string]GatewayResourceResult{},
		deletions:   map[string]bool{},
		restore:     map[string]bool{},
		now:         now,
	}
}

func (execution *gatewayGraphExecution) adapter(name string) (GatewayResourceAdapter, bool) {
	for _, adapter := range execution.adapters {
		if adapter.Name() == name {
			return adapter, true
		}
	}
	return nil, false
}

func (execution *gatewayGraphExecution) buildGraph() (ApplyGraph, error) {
	present := make(map[string]bool, len(execution.adapters))
	for _, adapter := range execution.adapters {
		present[adapter.Name()] = true
	}
	hooks := execution.plan.Hooks
	if hooks != nil && hooks.EnsurePPPoE != nil {
		present[nodePPPoE] = true
	}
	graph := ApplyGraph{}
	for _, adapter := range execution.adapters {
		adapter := adapter
		graph.Nodes = append(graph.Nodes, ApplyNode{
			Name:    adapter.Name(),
			Timeout: gatewayNodeTimeout(adapter.Name()),
			Run: func(ctx context.Context) error {
				return execution.applyAdapter(ctx, adapter)
			},
			Rollback: func(ctx context.Context) error {
				return execution.rollbackAdapter(ctx, adapter)
			},
		})
	}
	if present[nodePPPoE] {
		graph.Nodes = append(graph.Nodes, ApplyNode{
			Name:    nodePPPoE,
			Timeout: 3 * time.Minute,
			Run: func(ctx context.Context) error {
				return hooks.EnsurePPPoE(ctx, execution.plan)
			},
			Rollback: func(ctx context.Context) error {
				if hooks.RollbackPPPoE == nil {
					return nil
				}
				return hooks.RollbackPPPoE(ctx, execution.plan)
			},
		})
	}
	if hooks != nil && hooks.ApplyServices != nil {
		graph.Nodes = append(graph.Nodes, ApplyNode{
			Name:    nodeServices,
			Timeout: 5 * time.Minute,
			Run: func(ctx context.Context) error {
				return hooks.ApplyServices(ctx, execution.plan)
			},
			Rollback: func(ctx context.Context) error {
				if hooks.RollbackServices == nil {
					return nil
				}
				return hooks.RollbackServices(ctx, execution.plan)
			},
		})
	}

	for name, dependencies := range gatewayResourceDependencies {
		if !present[name] {
			continue
		}
		for _, dependency := range dependencies {
			if !present[dependency] {
				continue
			}
			graph.Edges = append(graph.Edges, gatewayEdge(dependency, name, execution.plan))
		}
	}
	if hooks != nil && hooks.ApplyServices != nil {
		for _, adapter := range execution.adapters {
			graph.Edges = append(graph.Edges, ApplyEdge{From: adapter.Name(), To: nodeServices})
		}
		if present[nodePPPoE] {
			graph.Edges = append(graph.Edges, ApplyEdge{From: nodePPPoE, To: nodeServices})
		}
	}
	return graph, nil
}

func gatewayEdge(from, to string, plan Plan) ApplyEdge {
	edge := ApplyEdge{From: from, To: to}
	hooks := plan.Hooks
	switch {
	case from == nodePPPoE && to == nodeServiceNetworks:
		edge.Timeout = 90 * time.Second
		if hooks != nil && hooks.VerifyPPPoE != nil {
			edge.Wait = func(ctx context.Context) error { return hooks.VerifyPPPoE(ctx, plan) }
		}
	case from == nodeServiceNetworks && (to == "routes" || to == "nat44"):
		edge.Timeout = 30 * time.Second
		if hooks != nil && hooks.VerifyServiceNetworks != nil {
			edge.Wait = func(ctx context.Context) error { return hooks.VerifyServiceNetworks(ctx, plan) }
		}
	case from == "nat44" && to == "port-maps":
		// NAT44 initialization is verified by the NAT adapter's own readback.
		// Keep the edge for ordering without an additional blocking predicate.
		edge.Optional = true
	}
	return edge
}

func gatewayNodeTimeout(name string) time.Duration {
	switch name {
	case nodeServiceNetworks:
		return 2 * time.Minute
	case "nat44", "port-maps":
		return 3 * time.Minute
	default:
		return 2 * time.Minute
	}
}

func (execution *gatewayGraphExecution) applyAdapter(ctx context.Context, adapter GatewayResourceAdapter) error {
	resourcePlan := execution.plan
	resourcePlan.Request.Resource = adapter.Name()
	resource, err := adapter.Apply(ctx, resourcePlan)
	if err != nil {
		return err
	}
	now := execution.transaction.now()
	if err := resource.Receipt.validate(resourcePlan.Request, now); err != nil {
		return err
	}
	if err := resource.Readback.validate(resourcePlan.Request, now); err != nil {
		return err
	}
	evidence, err := gatewayEvidence(resource, adapter.Name(), execution.plan.Request.TransactionID, now)
	if err != nil {
		return err
	}
	execution.results[adapter.Name()] = resource
	execution.evidence = append(execution.evidence, evidence)
	execution.deletions[adapter.Name()] = resource.Deleted
	return nil
}

func (execution *gatewayGraphExecution) rollbackAdapter(ctx context.Context, adapter GatewayResourceAdapter) error {
	if phased, ok := adapter.(GatewayResourceRollbackPhases); ok {
		return phased.RollbackCleanup(ctx, execution.plan)
	}
	return adapter.Rollback(ctx, execution.plan)
}

// restorePrior replays the persisted prior generation in forward dependency
// order after cleanup removed the introduced objects.
func (execution *gatewayGraphExecution) restorePrior(ctx context.Context) []error {
	var failures []error
	for _, adapter := range execution.adapters {
		if !execution.restore[adapter.Name()] {
			continue
		}
		phased, ok := adapter.(GatewayResourceRollbackPhases)
		if !ok {
			continue
		}
		if err := phased.RollbackRestore(ctx, execution.plan); err != nil {
			failures = append(failures, fmt.Errorf("restore %s: %w", adapter.Name(), err))
		}
	}
	return failures
}

func (execution *gatewayGraphExecution) rollback(ctx context.Context) error {
	graph, err := execution.buildGraph()
	if err != nil {
		return err
	}
	failures := graph.Rollback(ctx, execution.executed)
	failures = append(failures, execution.restorePrior(ctx)...)
	return joinGraphRollbackErrors(failures)
}

func (execution *gatewayGraphExecution) applyResult() GatewayTransactionResult {
	result := GatewayTransactionResult{
		Deletions: map[string]bool{},
	}
	for _, name := range execution.executed {
		adapter, ok := execution.adapter(name)
		if !ok {
			continue
		}
		resource := execution.results[name]
		result.Order = append(result.Order, adapter.Name())
		result.Receipts = append(result.Receipts, resource.Receipt)
		result.Readbacks = append(result.Readbacks, resource.Readback)
	}
	for key, value := range execution.deletions {
		result.Deletions[key] = value
	}
	result.Evidence = append([]GatewayResourceEvidence(nil), execution.evidence...)
	return result
}
