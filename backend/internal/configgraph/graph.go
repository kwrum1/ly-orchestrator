// Package configgraph builds the declarative dependency graph of the
// persisted desired state.
//
// The control plane stores one document per resource (LAN line, WAN line,
// proxy node, route policy, ...). Those documents reference each other by id:
// a port map names a WAN line, a route policy names an egress, a proxy egress
// names a group that names nodes, a DNS policy names an upstream and a proxy
// egress. Nothing in the per-document write path can see those edges, so an
// operator could delete the WAN a policy still routes through, clear a LAN port
// a DHCP service still serves, or turn a port from LAN to WAN while the line
// that owns it is still stored.
//
// This package derives the graph from a snapshot of the desired documents. It
// is pure: no clock, no IO, no store. The HTTP layer feeds it a snapshot and
// uses the result for referential-integrity checks, the dependencies API and
// the apply ordering.
package configgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind is a desired-state resource type. The value matches the resource_type
// used by the persistence store.
type Kind string

const (
	KindInterface           Kind = "interface"
	KindInterfaceBond       Kind = "interface_bond"
	KindManagementNetwork   Kind = "management_network"
	KindWANLink             Kind = "wan_link"
	KindWANGroup            Kind = "wan_group"
	KindObjectGroup         Kind = "object_group"
	KindPortMap             Kind = "port_map"
	KindNATStatic           Kind = "nat_static"
	KindRoutePolicy         Kind = "route_policy"
	KindProxyNode           Kind = "proxy_node"
	KindProxyGroup          Kind = "proxy_group"
	KindProxySubscription   Kind = "proxy_subscription"
	KindProxyEgress         Kind = "proxy_egress"
	KindDNSUpstream         Kind = "dns_upstream"
	KindDNSPolicy           Kind = "dns_policy"
	KindDomainIPSet         Kind = "domain_ip_set"
	KindDHCPServer          Kind = "dhcp_server"
	KindDHCPStaticBinding   Kind = "dhcp_static_binding"
	KindSecurityACL         Kind = "security_acl"
	KindSecurityIPMAC       Kind = "security_ip_mac_binding"
	KindSecurityThreatIntel Kind = "security_threat_intel"
	KindSecurityAttackRule  Kind = "security_attack_rule"
	KindTrafficControl      Kind = "traffic_control"
)

// allKinds lists every graph kind in dependency-friendly order. The HTTP layer
// loads documents in this order so a caller that logs or diffs the snapshot
// sees a stable sequence.
func allKinds() []Kind {
	return []Kind{
		KindInterface,
		KindInterfaceBond,
		KindManagementNetwork,
		KindWANLink,
		KindWANGroup,
		KindObjectGroup,
		KindDomainIPSet,
		KindProxySubscription,
		KindProxyNode,
		KindProxyGroup,
		KindProxyEgress,
		KindPortMap,
		KindNATStatic,
		KindRoutePolicy,
		KindDNSUpstream,
		KindDNSPolicy,
		KindDHCPServer,
		KindDHCPStaticBinding,
		KindSecurityACL,
		KindSecurityIPMAC,
		KindSecurityThreatIntel,
		KindSecurityAttackRule,
		KindTrafficControl,
	}
}

// Kinds returns every kind the graph understands.
func Kinds() []Kind {
	return allKinds()
}

// kindRank orders kinds for deterministic topological tie-breaking and for the
// advisory phase labels. It is the dataplane's build order: ports first, then
// lines, then objects that name lines, then policies that name objects.
var kindRank = map[Kind]int{
	KindInterface:           10,
	KindInterfaceBond:       20,
	KindManagementNetwork:   25,
	KindObjectGroup:         20,
	KindDomainIPSet:         20,
	KindWANLink:             30,
	KindDNSUpstream:         30,
	KindProxySubscription:   30,
	KindWANGroup:            40,
	KindProxyNode:           40,
	KindProxyGroup:          50,
	KindProxyEgress:         60,
	KindPortMap:             70,
	KindNATStatic:           70,
	KindRoutePolicy:         80,
	KindDNSPolicy:           80,
	KindDHCPServer:          80,
	KindDHCPStaticBinding:   82,
	KindSecurityACL:         80,
	KindSecurityIPMAC:       80,
	KindSecurityThreatIntel: 80,
	KindSecurityAttackRule:  80,
	KindTrafficControl:      80,
}

func rank(kind Kind) int {
	if value, ok := kindRank[kind]; ok {
		return value
	}
	return 100
}

// Label is the operator-facing noun for a kind. Error messages and UI badges
// use it instead of the storage spelling.
func Label(kind Kind) string {
	switch kind {
	case KindInterface:
		return "LAN line"
	case KindInterfaceBond:
		return "interface bond"
	case KindManagementNetwork:
		return "management network"
	case KindWANLink:
		return "WAN line"
	case KindWANGroup:
		return "WAN group"
	case KindObjectGroup:
		return "IP group"
	case KindPortMap:
		return "port map"
	case KindNATStatic:
		return "static NAT mapping"
	case KindRoutePolicy:
		return "route policy"
	case KindProxyNode:
		return "proxy node"
	case KindProxyGroup:
		return "proxy group"
	case KindProxySubscription:
		return "proxy subscription"
	case KindProxyEgress:
		return "proxy egress"
	case KindDNSUpstream:
		return "DNS upstream"
	case KindDNSPolicy:
		return "DNS policy"
	case KindDomainIPSet:
		return "domain IP set"
	case KindDHCPServer:
		return "DHCP service"
	case KindDHCPStaticBinding:
		return "DHCP static binding"
	case KindSecurityACL:
		return "security ACL"
	case KindSecurityIPMAC:
		return "IP/MAC binding"
	case KindSecurityThreatIntel:
		return "threat intel feed"
	case KindSecurityAttackRule:
		return "attack rule"
	case KindTrafficControl:
		return "traffic control policy"
	default:
		return string(kind)
	}
}

// NodeRef identifies one graph node.
type NodeRef struct {
	Type Kind   `json:"type"`
	ID   string `json:"id"`
}

func (ref NodeRef) String() string {
	return string(ref.Type) + ":" + ref.ID
}

// Node is one desired object. Virtual nodes are ports that are present in the
// hardware inventory but have no desired document yet; they exist so a
// reference to a real port is not reported as dangling.
type Node struct {
	Type    Kind   `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
	Virtual bool   `json:"virtual,omitempty"`
	// Invalid carries a machine-readable reason when the node can no longer be
	// applied as written (for example a proxy group whose members are gone).
	Invalid string `json:"invalid,omitempty"`
}

func (node Node) Ref() NodeRef {
	return NodeRef{Type: node.Type, ID: node.ID}
}

// Edge points from the dependent object to the object it requires. An apply
// creates To before From; a delete removes From before To.
type Edge struct {
	From  NodeRef `json:"from"`
	To    NodeRef `json:"to"`
	Field string  `json:"field"`
}

// Dangling is a reference that names no node of any expected kind. It is
// reported but never fatal: a LAN line can be written before the port
// inventory is refreshed. Apply-time compilation owns the hard failure.
type Dangling struct {
	From      NodeRef `json:"from"`
	Field     string  `json:"field"`
	Reference string  `json:"reference"`
	Expected  []Kind  `json:"expected_types"`
}

// Phase groups nodes that can be created together. Index 0 contains objects
// with no dependencies; each later phase depends only on earlier phases.
type Phase struct {
	Index int       `json:"index"`
	Name  string    `json:"name"`
	Kinds []string  `json:"kinds"`
	Nodes []NodeRef `json:"nodes"`
}

// Snapshot is the input to Build.
type Snapshot struct {
	// Documents holds every persisted desired object. Tombstoned documents
	// (deleted=true) must already be removed by the caller.
	Documents []Document
	// Ports lists physical product interface ids (eth3) known to exist. They
	// become virtual nodes so a WAN/LAN line that names a real port is not
	// reported as dangling.
	Ports []string
	// Resolve optionally canonicalizes a reference before lookup. The HTTP
	// layer uses it to fold kernel names, MACs and PCI addresses onto the
	// product id while the stored document keeps its own spelling.
	Resolve func(kind Kind, reference string) string
}

// Document is one desired object.
type Document struct {
	Kind    Kind
	ID      string
	Name    string
	Enabled bool
	Fields  map[string]any
}

// Graph is the derived dependency view.
type Graph struct {
	Nodes       []Node      `json:"nodes"`
	Edges       []Edge      `json:"edges"`
	Dangling    []Dangling  `json:"dangling"`
	CreateOrder []NodeRef   `json:"create_order"`
	DeleteOrder []NodeRef   `json:"delete_order"`
	Phases      []Phase     `json:"phases"`
	Cycles      [][]NodeRef `json:"cycles,omitempty"`
}

type builder struct {
	snapshot Snapshot
	nodes    map[NodeRef]*Node
	order    []NodeRef
	edges    []Edge
	edgeSeen map[string]struct{}
	dangling []Dangling
	lookup   map[Kind]map[string]NodeRef
	fields   map[NodeRef]map[string]any
}

// Build derives nodes, edges, dangling references, the topological create
// order, the reverse delete order, phase grouping and any cycles.
func Build(snapshot Snapshot) *Graph {
	graph := &Graph{}
	b := &builder{
		snapshot: snapshot,
		nodes:    map[NodeRef]*Node{},
		edgeSeen: map[string]struct{}{},
		lookup:   map[Kind]map[string]NodeRef{},
		fields:   map[NodeRef]map[string]any{},
	}
	for _, document := range snapshot.Documents {
		if strings.TrimSpace(document.ID) == "" {
			continue
		}
		ref := NodeRef{Type: document.Kind, ID: strings.TrimSpace(document.ID)}
		b.fields[ref] = document.Fields
		if _, exists := b.nodes[ref]; exists {
			continue
		}
		b.addNode(Node{
			Type:    ref.Type,
			ID:      ref.ID,
			Name:    document.Name,
			Enabled: document.Enabled,
		})
	}
	for _, port := range snapshot.Ports {
		port = strings.TrimSpace(port)
		if port == "" {
			continue
		}
		ref := NodeRef{Type: KindInterface, ID: port}
		if _, exists := b.nodes[ref]; exists {
			continue
		}
		b.addNode(Node{Type: KindInterface, ID: port, Enabled: true, Virtual: true})
	}
	for _, document := range snapshot.Documents {
		if strings.TrimSpace(document.ID) == "" {
			continue
		}
		b.scan(Document{
			Kind:    document.Kind,
			ID:      strings.TrimSpace(document.ID),
			Enabled: document.Enabled,
			Fields:  document.Fields,
		})
	}
	b.markInvalidGroups()
	graph.Nodes = b.sortedNodes()
	graph.Edges = b.sortedEdges()
	graph.Dangling = b.sortedDangling()
	graph.CreateOrder, graph.Cycles = b.topoOrder()
	graph.DeleteOrder = reverse(graph.CreateOrder)
	graph.Phases, graph.Cycles = b.phases(graph.CreateOrder, graph.Cycles)
	if graph.CreateOrder == nil {
		graph.CreateOrder = []NodeRef{}
	}
	if graph.DeleteOrder == nil {
		graph.DeleteOrder = []NodeRef{}
	}
	if graph.Phases == nil {
		graph.Phases = []Phase{}
	}
	return graph
}

func (b *builder) addNode(node Node) {
	ref := node.Ref()
	b.nodes[ref] = &node
	b.order = append(b.order, ref)
	if b.lookup[node.Type] == nil {
		b.lookup[node.Type] = map[string]NodeRef{}
	}
	b.lookup[node.Type][node.ID] = ref
}

// Node returns a node by reference.
func (g *Graph) Node(ref NodeRef) (Node, bool) {
	for _, node := range g.Nodes {
		if node.Type == ref.Type && node.ID == ref.ID {
			return node, true
		}
	}
	return Node{}, false
}

func (b *builder) resolve(reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}
	if b.snapshot.Resolve != nil {
		return strings.TrimSpace(b.snapshot.Resolve(KindInterface, reference))
	}
	return reference
}

// add records one reference. It resolves the value, finds the first expected
// kind that owns a matching node and records an edge; if no node matches it
// records a dangling reference.
func (b *builder) add(from NodeRef, field, reference string, expected ...Kind) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return
	}
	resolved := reference
	for _, kind := range expected {
		if kind != KindInterface {
			continue
		}
		resolved = b.resolve(reference)
		break
	}
	for _, kind := range expected {
		candidates := b.lookup[kind]
		for _, id := range []string{resolved, reference} {
			if ref, exists := candidates[id]; exists {
				if ref == from {
					return
				}
				b.addEdge(from, ref, field)
				return
			}
		}
	}
	b.dangling = append(b.dangling, Dangling{From: from, Field: field, Reference: resolved, Expected: expected})
}

func (b *builder) addEdge(from, to NodeRef, field string) {
	key := from.String() + "\x00" + to.String() + "\x00" + field
	if _, exists := b.edgeSeen[key]; exists {
		return
	}
	b.edgeSeen[key] = struct{}{}
	b.edges = append(b.edges, Edge{From: from, To: to, Field: field})
}

func (b *builder) scan(document Document) {
	ref := NodeRef{Type: document.Kind, ID: document.ID}
	fields := document.Fields
	switch document.Kind {
	case KindInterface:
		// A LAN line document may be keyed by its logical name and point at the
		// port it owns; a bridge lists its member ports.
		if kind := strings.ToLower(firstString(fields, "kind", "type")); kind == "lan_bridge" {
			for _, member := range stringList(value(fields, "bridge_members", "members")) {
				b.add(ref, "members", member, KindInterface)
			}
		} else if parent := firstString(fields, "interface_id", "parent_interface", "vlan_parent"); parent != "" {
			b.add(ref, "interface_id", parent, KindInterface)
		}
	case KindInterfaceBond:
		for _, member := range stringList(value(fields, "members")) {
			b.add(ref, "members", member, KindInterface)
		}
	case KindManagementNetwork:
		b.add(ref, "interface_id", firstString(fields, "interface_id"), KindInterface)
	case KindWANLink:
		b.add(ref, "interface_id", firstString(fields, "interface_id", "system_name"), KindInterface)
	case KindWANGroup:
		for _, field := range []string{"wan_members", "members"} {
			for _, member := range stringList(fields[field]) {
				b.add(ref, field, member, KindWANLink)
			}
		}
		for _, field := range []string{"primary_member", "primary_wan", "primary", "backup_member", "backup_wan", "backup"} {
			b.add(ref, field, firstString(fields, field), KindWANLink)
		}
		if loadBalance, ok := fields["load_balance"].(map[string]any); ok {
			for _, field := range []string{"primary_member", "primary_wan", "primary", "backup_member", "backup_wan", "backup"} {
				b.add(ref, "load_balance."+field, firstString(loadBalance, field), KindWANLink)
			}
		}
		for _, field := range []string{"weights", "member_weights"} {
			if weights, ok := fields[field].(map[string]any); ok {
				for member := range weights {
					b.add(ref, field, member, KindWANLink)
				}
			}
			for _, entry := range slice(fields[field]) {
				if object, ok := entry.(map[string]any); ok {
					b.add(ref, field, firstString(object, "id", "wan_id", "member_id", "egress_id"), KindWANLink)
				}
			}
		}
	case KindObjectGroup:
		for _, reference := range stringList(fields["references"]) {
			b.add(ref, "references", reference, KindObjectGroup)
		}
	case KindPortMap, KindNATStatic:
		for _, field := range []string{"wan_interface", "wan_link", "wan_link_id", "interface_id", "egress"} {
			b.add(ref, field, firstString(fields, field), KindWANLink, KindWANGroup, KindInterface)
		}
	case KindRoutePolicy:
		b.add(ref, "egress", firstString(fields, "egress", "target_line", "wan_group", "wan_link"), KindWANGroup, KindWANLink, KindProxyEgress)
		for _, field := range []string{"ingress_interface", "lan_interface"} {
			b.add(ref, field, firstString(fields, field), KindInterface)
		}
		b.addObjectGroups(ref, "match", fields["match"])
	case KindProxyNode:
		b.add(ref, "subscription_id", firstString(fields, "subscription_id"), KindProxySubscription)
	case KindProxyGroup:
		for _, member := range stringList(fields["members"]) {
			b.add(ref, "members", member, KindProxyNode)
		}
	case KindProxyEgress:
		b.add(ref, "node_id", firstString(fields, "node_id"), KindProxyNode)
		b.add(ref, "group_id", firstString(fields, "group_id"), KindProxyGroup)
		b.add(ref, "subscription_id", firstString(fields, "subscription_id"), KindProxySubscription)
		b.add(ref, "underlay_wan_id", firstString(fields, "underlay_wan_id", "underlay_wan"), KindWANLink)
	case KindDNSUpstream:
		b.add(ref, "interface_id", firstString(fields, "interface_id", "resolve_interface"), KindInterface)
	case KindDNSPolicy:
		b.scanDNSPolicy(ref, fields)
	case KindDHCPServer, KindDHCPStaticBinding:
		b.add(ref, "interface_id", firstString(fields, "interface_id", "lan_interface"), KindInterface)
	case KindSecurityACL, KindTrafficControl:
		for _, field := range []string{"ingress_interface", "lan_interface"} {
			b.add(ref, field, firstString(fields, field), KindInterface)
		}
		b.addObjectGroups(ref, "match", fields["match"])
		for index, rule := range slice(fields["rules"]) {
			if object, ok := rule.(map[string]any); ok {
				b.addObjectGroups(ref, fmt.Sprintf("rules[%d].match", index), value(object, "match"))
			}
		}
	case KindSecurityIPMAC:
		b.add(ref, "interface_id", firstString(fields, "interface_id"), KindInterface)
	}
}

// scanDNSPolicy understands both stored shapes: the policy handler persists
// the dns.Policy document itself, while the desired-state default nests it
// under "policy".
func (b *builder) scanDNSPolicy(ref NodeRef, fields map[string]any) {
	policy := fields
	if nested, ok := fields["policy"].(map[string]any); ok {
		policy = nested
	}
	b.addOutcome(ref, "miss", value(policy, "miss"))
	for index, rule := range slice(policy["rules"]) {
		object, ok := rule.(map[string]any)
		if !ok {
			continue
		}
		prefix := fmt.Sprintf("rules[%d]", index)
		outcome := value(object, "outcome")
		if outcome == nil {
			outcome = object
		}
		b.addOutcome(ref, prefix+".outcome", outcome)
		for _, field := range []string{"source_prefixes", "sources", "src_ip"} {
			b.addGroupTokens(ref, prefix+"."+field, value(object, field))
		}
		for _, setID := range stringList(value(object, "domain_set_ids")) {
			b.add(ref, prefix+".domain_set_ids", setID, KindDomainIPSet)
		}
	}
	if domainSet, ok := fields["domain_set"].(string); ok {
		b.add(ref, "domain_set", domainSet, KindDomainIPSet)
	}
}

func (b *builder) addOutcome(ref NodeRef, field string, value any) {
	outcome, ok := value.(map[string]any)
	if !ok {
		return
	}
	b.add(ref, field+".upstream_id", firstString(outcome, "upstream_id"), KindDNSUpstream)
	b.add(ref, field+".fallback_upstream_id", firstString(outcome, "fallback_upstream_id"), KindDNSUpstream)
	b.add(ref, field+".proxy_egress_id", firstString(outcome, "proxy_egress_id"), KindProxyEgress)
	b.add(ref, field+".wan_egress_id", firstString(outcome, "wan_egress_id"), KindWANLink, KindWANGroup)
}

// addObjectGroups records every token inside a match/selector object that
// names an existing IP group. Addresses, protocols and ports never collide
// with a group id, so no key allow-list is required.
func (b *builder) addObjectGroups(ref NodeRef, field string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			b.addObjectGroups(ref, field+"."+key, child)
		}
	case []any:
		for _, child := range typed {
			b.addObjectGroups(ref, field, child)
		}
	case string:
		b.addGroupTokens(ref, field, typed)
	}
}

func (b *builder) addGroupTokens(ref NodeRef, field string, value any) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			b.addGroupTokens(ref, field, child)
		}
		return
	case []string:
		for _, child := range typed {
			b.addGroupTokens(ref, field, child)
		}
		return
	case string:
		for _, token := range strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if group, ok := b.lookup[KindObjectGroup][token]; ok && group != ref {
				b.addEdge(ref, group, field)
			}
		}
	}
}

func (b *builder) markInvalidGroups() {
	for index := range b.order {
		node := b.nodes[b.order[index]]
		switch node.Type {
		case KindProxyGroup, KindWANGroup:
			fields := b.fields[node.Ref()]
			declared := len(stringList(fields["members"])) + len(stringList(fields["wan_members"]))
			resolved := 0
			for _, edge := range b.edges {
				if edge.From != node.Ref() {
					continue
				}
				if _, exists := b.nodes[edge.To]; exists {
					resolved++
				}
			}
			if declared == 0 {
				node.Invalid = "empty_members"
			} else if resolved < declared {
				node.Invalid = "dangling_members"
			}
		}
	}
}

func (b *builder) sortedNodes() []Node {
	nodes := make([]Node, 0, len(b.nodes))
	for _, ref := range b.order {
		nodes = append(nodes, *b.nodes[ref])
	}
	sort.SliceStable(nodes, func(left, right int) bool {
		return lessRef(NodeRef{Type: nodes[left].Type, ID: nodes[left].ID}, NodeRef{Type: nodes[right].Type, ID: nodes[right].ID})
	})
	return nodes
}

func (b *builder) sortedEdges() []Edge {
	edges := append([]Edge(nil), b.edges...)
	sort.SliceStable(edges, func(left, right int) bool {
		if edges[left].From != edges[right].From {
			return lessRef(edges[left].From, edges[right].From)
		}
		if edges[left].To != edges[right].To {
			return lessRef(edges[left].To, edges[right].To)
		}
		return edges[left].Field < edges[right].Field
	})
	return edges
}

func (b *builder) sortedDangling() []Dangling {
	dangling := append([]Dangling(nil), b.dangling...)
	sort.SliceStable(dangling, func(left, right int) bool {
		if dangling[left].From != dangling[right].From {
			return lessRef(dangling[left].From, dangling[right].From)
		}
		if dangling[left].Field != dangling[right].Field {
			return dangling[left].Field < dangling[right].Field
		}
		return dangling[left].Reference < dangling[right].Reference
	})
	return dangling
}

func lessRef(left, right NodeRef) bool {
	if rank(left.Type) != rank(right.Type) {
		return rank(left.Type) < rank(right.Type)
	}
	if left.Type != right.Type {
		return left.Type < right.Type
	}
	return left.ID < right.ID
}

func reverse(refs []NodeRef) []NodeRef {
	reversed := make([]NodeRef, 0, len(refs))
	for index := len(refs) - 1; index >= 0; index-- {
		reversed = append(reversed, refs[index])
	}
	return reversed
}

// ContentHash is a stable fingerprint of the graph. It changes when any node,
// edge or dangling reference changes, independent of document ordering or
// runtime fields such as timestamps.
func (g *Graph) ContentHash() string {
	payload := struct {
		Nodes    []Node     `json:"nodes"`
		Edges    []Edge     `json:"edges"`
		Dangling []Dangling `json:"dangling"`
	}{Nodes: g.Nodes, Edges: g.Edges, Dangling: g.Dangling}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ShortHash is the first 16 hex characters of ContentHash.
func (g *Graph) ShortHash() string {
	hash := g.ContentHash()
	if len(hash) > 16 {
		return hash[:16]
	}
	return hash
}

// Dependent is one object that (directly or transitively) requires a node.
// Path starts at the queried node and ends at Referrer, following the
// dependency direction (each element depends on the next).
type Dependent struct {
	Referrer NodeRef   `json:"referrer"`
	Field    string    `json:"field"`
	Depth    int       `json:"depth"`
	Path     []NodeRef `json:"path"`
}

// Dependents returns the direct dependents of ref.
func (g *Graph) Dependents(ref NodeRef) []Dependent {
	return g.collectDependents(ref, 1)
}

// TransitiveDependents returns every object that depends on ref, directly or
// through another object. The operators need the whole chain: deleting a WAN
// breaks a WAN group, which breaks a route policy that names the group.
func (g *Graph) TransitiveDependents(ref NodeRef) []Dependent {
	return g.collectDependents(ref, 0)
}

func (g *Graph) collectDependents(ref NodeRef, maxDepth int) []Dependent {
	results := []Dependent{}
	reported := map[string]bool{}
	expanded := map[NodeRef]bool{}
	type queued struct {
		ref   NodeRef
		path  []NodeRef
		depth int
	}
	queue := []queued{{ref: ref, path: []NodeRef{ref}}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if expanded[current.ref] {
			continue
		}
		expanded[current.ref] = true
		for _, edge := range g.Edges {
			if edge.To != current.ref {
				continue
			}
			depth := current.depth + 1
			if maxDepth > 0 && depth > maxDepth {
				continue
			}
			key := edge.From.String() + "\x00" + edge.Field
			if reported[key] {
				continue
			}
			reported[key] = true
			path := append(append([]NodeRef(nil), current.path...), edge.From)
			results = append(results, Dependent{Referrer: edge.From, Field: edge.Field, Depth: depth, Path: path})
			queue = append(queue, queued{ref: edge.From, path: path, depth: depth})
		}
	}
	sort.SliceStable(results, func(left, right int) bool {
		if results[left].Depth != results[right].Depth {
			return results[left].Depth < results[right].Depth
		}
		if results[left].Referrer != results[right].Referrer {
			return lessRef(results[left].Referrer, results[right].Referrer)
		}
		return results[left].Field < results[right].Field
	})
	return results
}

// EnabledDependents returns dependents whose document is enabled. Disabled
// consumers do not block a delete: they cannot be applied anyway.
func (g *Graph) EnabledDependents(ref NodeRef, transitive bool) []Dependent {
	var candidates []Dependent
	if transitive {
		candidates = g.TransitiveDependents(ref)
	} else {
		candidates = g.Dependents(ref)
	}
	enabled := make([]Dependent, 0, len(candidates))
	for _, candidate := range candidates {
		node, exists := g.Node(candidate.Referrer)
		if !exists || !node.Enabled {
			continue
		}
		enabled = append(enabled, candidate)
	}
	return enabled
}

// BuildError formats a cycle-aware refusal. The apply path uses it before any
// VPP command is generated, so a cycle can never abort halfway through a
// transaction.
func (g *Graph) BuildError() error {
	if len(g.Cycles) == 0 {
		return nil
	}
	paths := make([]string, 0, len(g.Cycles))
	for _, cycle := range g.Cycles {
		parts := make([]string, 0, len(cycle))
		for _, ref := range cycle {
			parts = append(parts, ref.String())
		}
		paths = append(paths, strings.Join(parts, " -> "))
	}
	return fmt.Errorf("configuration dependency cycle: %s", strings.Join(paths, "; "))
}

func value(fields map[string]any, keys ...string) any {
	for _, key := range keys {
		if result, ok := fields[key]; ok && result != nil {
			return result
		}
	}
	return nil
}

func firstString(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := fields[key].(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func slice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, item)
		}
		return result
	}
	return nil
}

func stringList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	case []any:
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				if trimmed := strings.TrimSpace(entry); trimmed != "" {
					result = append(result, trimmed)
				}
			case map[string]any:
				if id := firstString(entry, "id", "wan_id", "member_id", "egress_id", "node_id"); id != "" {
					result = append(result, id)
				}
			}
		}
	}
	return result
}
