package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ApplyGraph is the apply transaction's dependency graph.  Every node owns one
// observable resource (VPP attachment, interface/LCP, PPPoE helper, DNS service
// network, NAT44 role, service handoff, ...).  Edges carry the wait predicate
// that must hold before the target node is allowed to run.
//
// The graph deliberately knows nothing about VPP or systemd: callers provide
// the node functions.  This keeps ordering, timeout, rollback and failure
// reporting in one place instead of scattering them across ad-hoc phases.
type ApplyGraph struct {
	Nodes        []ApplyNode
	Edges        []ApplyEdge
	PollInterval time.Duration
}

type ApplyNode struct {
	Name     string
	Timeout  time.Duration
	Run      func(context.Context) error
	Verify   func(context.Context) error
	Rollback func(context.Context) error
}

type ApplyEdge struct {
	From    string
	To      string
	Wait    func(context.Context) error
	Timeout time.Duration
	// Optional edges are reported as degraded but do not abort the graph.
	Optional bool
}

type GraphResult struct {
	Executed []string
	Order    []string
}

const (
	defaultGraphNodeTimeout = 2 * time.Minute
	defaultGraphEdgeTimeout = 45 * time.Second
	defaultGraphPoll        = 500 * time.Millisecond
)

type GraphError struct {
	Node    string
	From    string
	To      string
	Phase   string
	Timeout time.Duration
	Cause   error
}

func (err *GraphError) Error() string {
	switch {
	case err.From != "" || err.To != "":
		return fmt.Sprintf("apply dependency %s -> %s did not become ready within %s: %v", err.From, err.To, err.Timeout, err.Cause)
	case err.Node != "":
		return fmt.Sprintf("apply node %s %s failed after %s: %v", err.Node, err.Phase, err.Timeout, err.Cause)
	default:
		return fmt.Sprintf("apply graph %s failed: %v", err.Phase, err.Cause)
	}
}

func (err *GraphError) Unwrap() error { return err.Cause }

func (graph ApplyGraph) Run(ctx context.Context) (GraphResult, error) {
	order, err := graph.topologicalOrder()
	if err != nil {
		return GraphResult{}, err
	}
	result := GraphResult{Order: order}
	for _, name := range order {
		node, _ := graph.node(name)
		for _, edge := range graph.incoming(name) {
			if err := graph.waitEdge(ctx, edge); err != nil {
				if edge.Optional {
					continue
				}
				if graphErr, ok := err.(*GraphError); ok {
					graphErr.Node = name
					return result, graphErr
				}
				return result, err
			}
		}
		runCtx, cancel, timeout := graph.nodeContext(ctx, node.Timeout)
		runErr := node.Run(runCtx)
		if runErr == nil && node.Verify != nil {
			runErr = node.Verify(runCtx)
		}
		cancel()
		if runErr != nil {
			// A node may have mutated objects before failing; include it in the
			// rollback set and let its cleanup be idempotent.
			result.Executed = append(result.Executed, name)
			return result, &GraphError{Node: name, Phase: "run", Timeout: timeout, Cause: runErr}
		}
		result.Executed = append(result.Executed, name)
	}
	return result, nil
}

// Rollback tears down every executed node in strict reverse topological order.
// It continues after failures so a single stuck resource cannot leave the rest
// of the generation behind, and returns one error per failed node.
func (graph ApplyGraph) Rollback(ctx context.Context, executed []string) []error {
	order, err := graph.topologicalOrder()
	if err != nil {
		return []error{err}
	}
	done := make(map[string]bool, len(executed))
	for _, name := range executed {
		done[name] = true
	}
	var failures []error
	for index := len(order) - 1; index >= 0; index-- {
		name := order[index]
		if !done[name] {
			continue
		}
		node, ok := graph.node(name)
		if !ok || node.Rollback == nil {
			continue
		}
		rollbackCtx, cancel, _ := graph.nodeContext(ctx, node.Timeout)
		rollbackErr := node.Rollback(rollbackCtx)
		cancel()
		if rollbackErr != nil {
			failures = append(failures, fmt.Errorf("rollback %s: %w", name, rollbackErr))
		}
	}
	return failures
}

func (graph ApplyGraph) waitEdge(ctx context.Context, edge ApplyEdge) error {
	if edge.Wait == nil {
		return nil
	}
	timeout := edge.Timeout
	if timeout <= 0 {
		timeout = defaultGraphEdgeTimeout
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := edge.Wait(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return &GraphError{From: edge.From, To: edge.To, Phase: "wait", Timeout: timeout, Cause: last}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(graph.pollInterval()):
		}
	}
}

func (graph ApplyGraph) pollInterval() time.Duration {
	if graph.PollInterval > 0 {
		return graph.PollInterval
	}
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_APPLY_GRAPH_POLL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultGraphPoll
}

func (graph ApplyGraph) nodeContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	if timeout <= 0 {
		timeout = defaultGraphNodeTimeout
	}
	nodeCtx, cancel := context.WithTimeout(ctx, timeout)
	return nodeCtx, cancel, timeout
}

func (graph ApplyGraph) node(name string) (ApplyNode, bool) {
	for _, node := range graph.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return ApplyNode{}, false
}

func (graph ApplyGraph) incoming(name string) []ApplyEdge {
	edges := make([]ApplyEdge, 0)
	for _, edge := range graph.Edges {
		if edge.To == name {
			edges = append(edges, edge)
		}
	}
	return edges
}

// topologicalOrder validates the graph and returns a stable order.
func (graph ApplyGraph) topologicalOrder() ([]string, error) {
	index := make(map[string]int, len(graph.Nodes))
	nodes := make(map[string]ApplyNode, len(graph.Nodes))
	for position, node := range graph.Nodes {
		name := strings.TrimSpace(node.Name)
		if name == "" {
			return nil, fmt.Errorf("apply graph node at position %d has no name", position)
		}
		if _, exists := index[name]; exists {
			return nil, fmt.Errorf("apply graph has duplicate node %q", name)
		}
		index[name] = position
		node.Name = name
		nodes[name] = node
	}
	indegree := make(map[string]int, len(graph.Nodes))
	adjacency := make(map[string][]string, len(graph.Nodes))
	for _, edge := range graph.Edges {
		from := strings.TrimSpace(edge.From)
		to := strings.TrimSpace(edge.To)
		if _, ok := nodes[from]; !ok {
			return nil, fmt.Errorf("apply graph edge %q -> %q references missing node %q", from, to, from)
		}
		if _, ok := nodes[to]; !ok {
			return nil, fmt.Errorf("apply graph edge %q -> %q references missing node %q", from, to, to)
		}
		adjacency[from] = append(adjacency[from], to)
		indegree[to]++
	}
	ready := make([]string, 0, len(nodes))
	for name := range nodes {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	sort.Slice(ready, func(left, right int) bool { return index[ready[left]] < index[ready[right]] })
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		next := adjacency[name]
		sort.Slice(next, func(left, right int) bool { return index[next[left]] < index[next[right]] })
		for _, target := range next {
			indegree[target]--
			if indegree[target] == 0 {
				ready = append(ready, target)
			}
		}
		sort.Slice(ready, func(left, right int) bool { return index[ready[left]] < index[ready[right]] })
	}
	if len(order) != len(nodes) {
		return nil, fmt.Errorf("apply graph contains a dependency cycle")
	}
	return order, nil
}

func joinGraphRollbackErrors(failures []error) error {
	failures = compactErrors(failures)
	if len(failures) == 0 {
		return nil
	}
	return errors.Join(failures...)
}

func compactErrors(failures []error) []error {
	result := failures[:0]
	for _, failure := range failures {
		if failure == nil {
			continue
		}
		result = append(result, failure)
	}
	return result
}
