package apply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApplyGraphRunsTopologicalOrderAndWaitsOnEdges(t *testing.T) {
	var trace []string
	ready := false
	graph := ApplyGraph{
		PollInterval: time.Millisecond,
		Nodes: []ApplyNode{
			{Name: "interfaces", Run: func(context.Context) error { trace = append(trace, "interfaces"); ready = true; return nil }},
			{Name: "pppoe", Run: func(context.Context) error { trace = append(trace, "pppoe"); return nil }},
			{Name: "service-networks", Run: func(context.Context) error { trace = append(trace, "service-networks"); return nil }},
			{Name: "routes", Run: func(context.Context) error { trace = append(trace, "routes"); return nil }},
		},
		Edges: []ApplyEdge{
			{From: "interfaces", To: "pppoe"},
			{From: "interfaces", To: "service-networks", Wait: func(context.Context) error {
				if !ready {
					return errors.New("interfaces not ready")
				}
				return nil
			}, Timeout: 50 * time.Millisecond},
			{From: "pppoe", To: "service-networks"},
			{From: "service-networks", To: "routes"},
		},
	}
	result, err := graph.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"interfaces", "pppoe", "service-networks", "routes"}
	if !reflect.DeepEqual(result.Order, want) || !reflect.DeepEqual(trace, want) {
		t.Fatalf("order=%v trace=%v, want %v", result.Order, trace, want)
	}
}

func TestApplyGraphRejectsMissingDependencyAndCycle(t *testing.T) {
	missing := ApplyGraph{
		Nodes: []ApplyNode{{Name: "routes", Run: func(context.Context) error { return nil }}},
		Edges: []ApplyEdge{{From: "service-networks", To: "routes"}},
	}
	if _, err := missing.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "missing node") {
		t.Fatalf("missing dependency error = %v", err)
	}
	cycle := ApplyGraph{
		Nodes: []ApplyNode{
			{Name: "a", Run: func(context.Context) error { return nil }},
			{Name: "b", Run: func(context.Context) error { return nil }},
		},
		Edges: []ApplyEdge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	if _, err := cycle.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestApplyGraphEdgePredicateTimeoutIsActionable(t *testing.T) {
	graph := ApplyGraph{
		PollInterval: time.Millisecond,
		Nodes: []ApplyNode{
			{Name: "pppoe", Run: func(context.Context) error { return nil }},
			{Name: "service-networks", Run: func(context.Context) error { return nil }},
		},
		Edges: []ApplyEdge{{From: "pppoe", To: "service-networks", Wait: func(context.Context) error {
			return errors.New("PPPoE status missing")
		}, Timeout: 20 * time.Millisecond}},
	}
	_, err := graph.Run(context.Background())
	if err == nil {
		t.Fatal("edge timeout must fail the graph")
	}
	var graphErr *GraphError
	if !errors.As(err, &graphErr) || graphErr.From != "pppoe" || graphErr.To != "service-networks" {
		t.Fatalf("error = %#v", err)
	}
	if !strings.Contains(err.Error(), "PPPoE status missing") || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestApplyGraphRollbackIsReverseOrderAndContinuesAfterFailure(t *testing.T) {
	var trace []string
	graph := ApplyGraph{
		Nodes: []ApplyNode{
			{Name: "interfaces", Run: func(context.Context) error { return nil }, Rollback: func(context.Context) error { trace = append(trace, "cleanup:interfaces"); return nil }},
			{Name: "pppoe", Run: func(context.Context) error { return nil }, Rollback: func(context.Context) error { trace = append(trace, "cleanup:pppoe"); return nil }},
			{Name: "service-networks", Run: func(context.Context) error { return nil }, Rollback: func(context.Context) error {
				trace = append(trace, "cleanup:service-networks")
				return errors.New("NAT role stuck")
			}},
			{Name: "routes", Run: func(context.Context) error { return nil }, Rollback: func(context.Context) error { trace = append(trace, "cleanup:routes"); return nil }},
		},
		Edges: []ApplyEdge{
			{From: "interfaces", To: "pppoe"},
			{From: "pppoe", To: "service-networks"},
			{From: "service-networks", To: "routes"},
		},
	}
	failures := graph.Rollback(context.Background(), []string{"interfaces", "pppoe", "service-networks", "routes"})
	want := []string{"cleanup:routes", "cleanup:service-networks", "cleanup:pppoe", "cleanup:interfaces"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("rollback trace = %v, want %v", trace, want)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "service-networks") {
		t.Fatalf("rollback failures = %v", failures)
	}
}

func TestApplyGraphNodeTimeoutAndVerify(t *testing.T) {
	graph := ApplyGraph{
		Nodes: []ApplyNode{
			{Name: "slow", Timeout: 10 * time.Millisecond, Run: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}},
			{Name: "verify", Run: func(context.Context) error { return nil }, Verify: func(context.Context) error {
				return errors.New("NAT44 input role missing")
			}},
		},
	}
	if _, err := graph.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("node timeout error = %v", err)
	}
	graph.Nodes = graph.Nodes[1:]
	if _, err := graph.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "NAT44 input role missing") {
		t.Fatalf("verify error = %v", err)
	}
}
