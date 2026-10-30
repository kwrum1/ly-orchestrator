package configgraph

import (
	"sort"
	"strings"
)

// topological order
//
// The graph stores edges as From (the object that names another object) ->
// To (the object it requires). An apply must create To first, so the sort is
// Kahn's algorithm over "objects with no unmet dependency", with a stable
// tie-break by kind rank and id. The delete order is exactly the reverse: a
// dependent object is removed before the object it names, otherwise a policy
// would briefly name a deleted WAN while the transaction is in flight.

func (b *builder) topoOrder() ([]NodeRef, [][]NodeRef) {
	refs := make([]NodeRef, 0, len(b.nodes))
	for ref := range b.nodes {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool { return lessRef(refs[left], refs[right]) })

	unmet := map[NodeRef]int{}
	dependents := map[NodeRef][]NodeRef{}
	dependencies := map[NodeRef][]NodeRef{}
	for _, edge := range b.edges {
		if _, exists := b.nodes[edge.To]; !exists {
			// Dangling references are reported separately and do not order
			// anything: there is no object to wait for.
			continue
		}
		unmet[edge.From]++
		dependents[edge.To] = append(dependents[edge.To], edge.From)
		dependencies[edge.From] = append(dependencies[edge.From], edge.To)
	}

	ready := make([]NodeRef, 0, len(refs))
	for _, ref := range refs {
		if unmet[ref] == 0 {
			ready = append(ready, ref)
		}
	}

	order := make([]NodeRef, 0, len(refs))
	for len(ready) > 0 {
		sort.Slice(ready, func(left, right int) bool { return lessRef(ready[left], ready[right]) })
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		for _, dependent := range dependents[next] {
			unmet[dependent]--
			if unmet[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}

	if len(order) == len(refs) {
		return order, nil
	}

	remaining := make(map[NodeRef]bool, len(refs)-len(order))
	placed := make(map[NodeRef]bool, len(order))
	for _, ref := range order {
		placed[ref] = true
	}
	for _, ref := range refs {
		if !placed[ref] {
			remaining[ref] = true
		}
	}
	return order, findCycles(remaining, dependencies)
}

// findCycles returns dependency cycles inside the unresolved subgraph. Each
// path is written in dependency order and repeats the entry node at the end,
// so an operator can follow "a depends on b depends on a" without guessing.
func findCycles(remaining map[NodeRef]bool, dependencies map[NodeRef][]NodeRef) [][]NodeRef {
	if len(remaining) == 0 {
		return nil
	}
	const (
		white = iota
		gray
		black
	)
	color := make(map[NodeRef]int, len(remaining))
	cycles := [][]NodeRef{}
	seen := map[string]bool{}
	path := []NodeRef{}

	var visit func(NodeRef)
	visit = func(node NodeRef) {
		color[node] = gray
		path = append(path, node)
		neighbours := append([]NodeRef(nil), dependencies[node]...)
		sort.Slice(neighbours, func(left, right int) bool { return lessRef(neighbours[left], neighbours[right]) })
		for _, neighbour := range neighbours {
			if !remaining[neighbour] {
				continue
			}
			switch color[neighbour] {
			case white:
				visit(neighbour)
			case gray:
				start := 0
				for index, entry := range path {
					if entry == neighbour {
						start = index
						break
					}
				}
				cycle := append([]NodeRef(nil), path[start:]...)
				cycle = append(cycle, neighbour)
				key := cycleKey(cycle)
				if !seen[key] {
					seen[key] = true
					cycles = append(cycles, cycle)
				}
			}
			if len(cycles) >= maxReportedCycles {
				break
			}
		}
		path = path[:len(path)-1]
		color[node] = black
	}

	refs := make([]NodeRef, 0, len(remaining))
	for ref := range remaining {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool { return lessRef(refs[left], refs[right]) })
	for _, ref := range refs {
		if color[ref] == white {
			visit(ref)
		}
		if len(cycles) >= maxReportedCycles {
			break
		}
	}
	if len(cycles) == 0 {
		// Unreachable in theory: a Kahn leftover without a cycle would mean a
		// dependency on an object that is itself placed, which cannot happen.
		// Report the leftovers rather than pretending the graph is ordered.
		leftover := append([]NodeRef(nil), refs...)
		return [][]NodeRef{leftover}
	}
	return cycles
}

const maxReportedCycles = 8

func cycleKey(cycle []NodeRef) string {
	if len(cycle) == 0 {
		return ""
	}
	body := cycle[:len(cycle)-1]
	best := 0
	for index := range body {
		if lessRef(body[index], body[best]) {
			best = index
		}
	}
	rotated := make([]string, 0, len(body))
	for offset := range body {
		rotated = append(rotated, body[(best+offset)%len(body)].String())
	}
	return strings.Join(rotated, "|")
}

// phases groups the create order by dependency depth. The order slice is
// authoritative; phases exist so the UI and the apply plan can show progress
// ("interfaces", "WAN", "policies") without re-implementing the sort.
func (b *builder) phases(order []NodeRef, cycles [][]NodeRef) ([]Phase, [][]NodeRef) {
	if len(cycles) > 0 {
		return nil, cycles
	}
	level := make(map[NodeRef]int, len(order))
	for _, ref := range order {
		deepest := -1
		for _, edge := range b.edges {
			if edge.From != ref {
				continue
			}
			if _, exists := b.nodes[edge.To]; !exists {
				continue
			}
			if dependencyLevel, ok := level[edge.To]; ok && dependencyLevel > deepest {
				deepest = dependencyLevel
			}
		}
		level[ref] = deepest + 1
	}
	byLevel := map[int][]NodeRef{}
	highest := 0
	for _, ref := range order {
		byLevel[level[ref]] = append(byLevel[level[ref]], ref)
		if level[ref] > highest {
			highest = level[ref]
		}
	}
	phases := make([]Phase, 0, highest+1)
	for index := 0; index <= highest; index++ {
		members := byLevel[index]
		if len(members) == 0 {
			continue
		}
		sort.Slice(members, func(left, right int) bool { return lessRef(members[left], members[right]) })
		kinds := []string{}
		seen := map[Kind]bool{}
		for _, ref := range members {
			if seen[ref.Type] {
				continue
			}
			seen[ref.Type] = true
			kinds = append(kinds, string(ref.Type))
		}
		phases = append(phases, Phase{Index: index, Name: phaseName(seen), Kinds: kinds, Nodes: members})
	}
	return phases, nil
}

func phaseName(kinds map[Kind]bool) string {
	highest := 0
	for kind := range kinds {
		if rank(kind) > highest {
			highest = rank(kind)
		}
	}
	switch {
	case highest <= rank(KindInterface):
		return "interfaces"
	case highest <= rank(KindWANLink):
		return "lines"
	case highest <= rank(KindProxyNode):
		return "groups"
	case highest <= rank(KindProxyGroup):
		return "proxy"
	case highest <= rank(KindProxyEgress):
		return "egress"
	case highest <= rank(KindNATStatic):
		return "routing"
	default:
		return "policies"
	}
}
