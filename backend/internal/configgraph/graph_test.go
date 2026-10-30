package configgraph

import (
	"strings"
	"testing"
)

func document(kind Kind, id string, fields map[string]any) Document {
	return Document{Kind: kind, ID: id, Name: id, Enabled: true, Fields: fields}
}

func buildBasic() *Graph {
	return Build(Snapshot{
		Ports: []string{"eth2"},
		Documents: []Document{
			document(KindWANLink, "wan-main", map[string]any{"id": "wan-main", "interface_id": "eth2"}),
			document(KindRoutePolicy, "policy-1", map[string]any{"id": "policy-1", "egress": "wan-main"}),
			document(KindPortMap, "pm-1", map[string]any{"id": "pm-1", "wan_interface": "wan-main"}),
		},
	})
}

func refIndex(order []NodeRef, ref NodeRef) int {
	for index, entry := range order {
		if entry == ref {
			return index
		}
	}
	return -1
}

func TestBuildOrdersDependenciesBeforeDependents(t *testing.T) {
	graph := buildBasic()
	if len(graph.Cycles) != 0 {
		t.Fatalf("unexpected cycles: %v", graph.Cycles)
	}
	port := NodeRef{Type: KindInterface, ID: "eth2"}
	wan := NodeRef{Type: KindWANLink, ID: "wan-main"}
	policy := NodeRef{Type: KindRoutePolicy, ID: "policy-1"}
	portMap := NodeRef{Type: KindPortMap, ID: "pm-1"}

	if !(refIndex(graph.CreateOrder, port) < refIndex(graph.CreateOrder, wan)) {
		t.Fatalf("port is not created before the WAN line: %v", graph.CreateOrder)
	}
	for _, dependent := range []NodeRef{policy, portMap} {
		if !(refIndex(graph.CreateOrder, wan) < refIndex(graph.CreateOrder, dependent)) {
			t.Fatalf("WAN line is not created before %s: %v", dependent, graph.CreateOrder)
		}
	}
	// Delete is the exact reverse: dependents first, then the object they name.
	if len(graph.DeleteOrder) != len(graph.CreateOrder) {
		t.Fatalf("delete order length %d != create order length %d", len(graph.DeleteOrder), len(graph.CreateOrder))
	}
	for left, right := 0, len(graph.CreateOrder)-1; left < len(graph.CreateOrder); left, right = left+1, right-1 {
		if graph.DeleteOrder[left] != graph.CreateOrder[right] {
			t.Fatalf("delete order is not the reverse of create order: create=%v delete=%v", graph.CreateOrder, graph.DeleteOrder)
		}
	}
	if len(graph.Phases) < 2 {
		t.Fatalf("expected at least two dependency phases, got %#v", graph.Phases)
	}
}

func TestTransitiveDependentsFollowTheWholeChain(t *testing.T) {
	graph := buildBasic()
	port := NodeRef{Type: KindInterface, ID: "eth2"}
	dependents := graph.EnabledDependents(port, true)
	seen := map[NodeRef]bool{}
	for _, dependent := range dependents {
		seen[dependent.Referrer] = true
		if len(dependent.Path) < 2 || dependent.Path[0] != port {
			t.Fatalf("dependent path does not start at the port: %#v", dependent)
		}
	}
	for _, want := range []NodeRef{
		{Type: KindWANLink, ID: "wan-main"},
		{Type: KindRoutePolicy, ID: "policy-1"},
		{Type: KindPortMap, ID: "pm-1"},
	} {
		if !seen[want] {
			t.Fatalf("transitive dependents miss %s: %#v", want, dependents)
		}
	}
	direct := graph.EnabledDependents(port, false)
	if len(direct) != 1 || direct[0].Referrer.Type != KindWANLink {
		t.Fatalf("direct dependents of the port = %#v, want only the WAN line", direct)
	}
}

func TestCycleIsReportedWithPath(t *testing.T) {
	graph := Build(Snapshot{Documents: []Document{
		document(KindObjectGroup, "group-a", map[string]any{"id": "group-a", "references": []any{"group-b"}}),
		document(KindObjectGroup, "group-b", map[string]any{"id": "group-b", "references": []any{"group-a"}}),
	}})
	if len(graph.Cycles) == 0 {
		t.Fatal("cycle was not detected")
	}
	err := graph.BuildError()
	if err == nil {
		t.Fatal("BuildError returned nil for a cyclic graph")
	}
	message := err.Error()
	if !strings.Contains(message, "group-a") || !strings.Contains(message, "group-b") || !strings.Contains(message, "->") {
		t.Fatalf("cycle error does not name the path: %q", message)
	}
	if len(graph.CreateOrder) != 0 {
		t.Fatalf("cyclic graph produced a partial create order: %v", graph.CreateOrder)
	}
}

func TestDanglingReferenceIsReportedNotFatal(t *testing.T) {
	graph := Build(Snapshot{Documents: []Document{
		document(KindRoutePolicy, "policy-1", map[string]any{"id": "policy-1", "egress": "wan-missing"}),
	}})
	if len(graph.Dangling) != 1 {
		t.Fatalf("dangling = %#v, want one entry", graph.Dangling)
	}
	entry := graph.Dangling[0]
	if entry.Reference != "wan-missing" || entry.Field != "egress" {
		t.Fatalf("dangling entry = %#v", entry)
	}
	if len(entry.Expected) != 3 {
		t.Fatalf("dangling expected kinds = %v", entry.Expected)
	}
	if len(graph.Cycles) != 0 {
		t.Fatalf("dangling reference must not be a cycle: %v", graph.Cycles)
	}
}

func TestGroupsWithMissingOrNoMembersAreInvalid(t *testing.T) {
	graph := Build(Snapshot{Documents: []Document{
		document(KindProxyGroup, "empty", map[string]any{"id": "empty"}),
		document(KindProxyGroup, "dangling", map[string]any{"id": "dangling", "members": []any{"node-gone"}}),
		document(KindProxyGroup, "healthy", map[string]any{"id": "healthy", "members": []any{"node-1"}}),
		document(KindProxyNode, "node-1", map[string]any{"id": "node-1"}),
	}})
	want := map[string]string{"empty": "empty_members", "dangling": "dangling_members", "healthy": ""}
	for id, reason := range want {
		node, found := graph.Node(NodeRef{Type: KindProxyGroup, ID: id})
		if !found {
			t.Fatalf("group %s missing from graph", id)
		}
		if node.Invalid != reason {
			t.Fatalf("group %s invalid = %q, want %q", id, node.Invalid, reason)
		}
	}
}

func TestContentHashTracksStructuralChanges(t *testing.T) {
	first := buildBasic().ContentHash()
	second := buildBasic().ContentHash()
	if first == "" || first != second {
		t.Fatalf("content hash is not deterministic: %q vs %q", first, second)
	}
	changed := Build(Snapshot{
		Ports: []string{"eth2"},
		Documents: []Document{
			document(KindWANLink, "wan-main", map[string]any{"id": "wan-main", "interface_id": "eth2"}),
			document(KindRoutePolicy, "policy-1", map[string]any{"id": "policy-1", "egress": "wan-main"}),
		},
	}).ContentHash()
	if changed == first {
		t.Fatal("removing a dependent did not change the content hash")
	}
}

func TestInterfaceReferenceResolutionFoldsAliases(t *testing.T) {
	graph := Build(Snapshot{
		Ports: []string{"eth2"},
		Resolve: func(kind Kind, reference string) string {
			if kind == KindInterface && reference == "enp4s0" {
				return "eth2"
			}
			return reference
		},
		Documents: []Document{
			document(KindWANLink, "wan-main", map[string]any{"id": "wan-main", "interface_id": "enp4s0"}),
		},
	})
	if len(graph.Dangling) != 0 {
		t.Fatalf("kernel-name reference was not resolved: %#v", graph.Dangling)
	}
	wan := NodeRef{Type: KindWANLink, ID: "wan-main"}
	dependents := graph.Dependents(NodeRef{Type: KindInterface, ID: "eth2"})
	if len(dependents) != 1 || dependents[0].Referrer != wan {
		t.Fatalf("WAN line is not attached to the canonical port: %#v", dependents)
	}
}
