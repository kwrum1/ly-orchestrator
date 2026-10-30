package proxy

import (
	"fmt"
	"strings"
)

// GroupStrategy is the operator-visible load policy of a node group.
//
//	latency     - one node at a time, the lowest observed RTT (xray leastPing)
//	loadbalance - spread connections across every member (xray roundRobin)
//
// A group with a single member is valid for both strategies; the data path
// then degenerates to that node and EnsureFailureFallback removes the
// health-gated balancer entirely.
type GroupStrategy string

const (
	GroupStrategyLatency     GroupStrategy = "latency"
	GroupStrategyLoadBalance GroupStrategy = "loadbalance"
)

type NodeGroup struct {
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Members  []string      `json:"members"`
	Strategy GroupStrategy `json:"strategy,omitempty"`
	// ProbeURL overrides the observatory probe target for this group. Empty
	// falls back to LY_ROUTE_PROXY_PROBE_URL, then the product default.
	ProbeURL string `json:"probe_url,omitempty"`
	// TopN is the number of members that participate in load balancing.
	// Zero (or a value >= len(Members)) means every member participates.
	// latency ignores it: that strategy always uses the single lowest-RTT node.
	TopN    int  `json:"top_n,omitempty"`
	Enabled bool `json:"enabled"`
}

// GroupBalancerTag is the xray balancer tag a compiled group selects.
func GroupBalancerTag(groupID string) string {
	var builder strings.Builder
	builder.WriteString("group-")
	for _, char := range strings.TrimSpace(groupID) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('-')
	}
	return builder.String()
}

// CompileNodeGroup turns a group into the outbound set and the balancer that
// selects among them. Member order is preserved so diagnostics and the fastest
// tie-break stay deterministic.
func CompileNodeGroup(group NodeGroup, nodes []Node) ([]XrayOutbound, XrayBalancer, error) {
	id := strings.TrimSpace(group.ID)
	if id == "" {
		return nil, XrayBalancer{}, fmt.Errorf("%w: node group id is required", ErrInvalidEgress)
	}
	if !group.Enabled {
		return nil, XrayBalancer{}, fmt.Errorf("%w: node group %q is disabled", ErrInvalidEgress, id)
	}
	strategy := GroupStrategy(strings.TrimSpace(string(group.Strategy)))
	if strategy == "" {
		strategy = GroupStrategyLatency
	}
	var balancerType string
	switch strategy {
	case GroupStrategyLatency:
		balancerType = "leastPing"
	case GroupStrategyLoadBalance:
		balancerType = "roundRobin"
	default:
		return nil, XrayBalancer{}, fmt.Errorf("%w: node group %q uses unsupported strategy %q", ErrInvalidEgress, id, strategy)
	}

	byID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		byID[strings.TrimSpace(node.ID)] = node
	}
	seen := make(map[string]struct{}, len(group.Members))
	members := make([]Node, 0, len(group.Members))
	for _, rawID := range group.Members {
		memberID := strings.TrimSpace(rawID)
		if memberID == "" {
			continue
		}
		if _, duplicate := seen[memberID]; duplicate {
			continue
		}
		seen[memberID] = struct{}{}
		node, exists := byID[memberID]
		if !exists {
			// A subscription refresh can retire a node document at any time.
			// Keep serving the remaining members instead of failing the whole
			// egress; an empty group is still an error below.
			continue
		}
		members = append(members, node)
	}
	if len(members) == 0 {
		return nil, XrayBalancer{}, fmt.Errorf("%w: node group %q has no usable member nodes", ErrInvalidEgress, id)
	}

	outbounds := make([]XrayOutbound, 0, len(members))
	tags := make([]string, 0, len(members))
	for _, node := range members {
		outbound, err := CompileNodeOutbound(node)
		if err != nil {
			return nil, XrayBalancer{}, fmt.Errorf("%w: node group %q member %q: %v", ErrInvalidEgress, id, node.ID, err)
		}
		outbounds = append(outbounds, outbound)
		tags = append(tags, outbound.Tag)
	}

	return outbounds, XrayBalancer{
		Tag:      GroupBalancerTag(id),
		Selector: tags,
		Strategy: XrayBalancerStrategy{Type: balancerType, Settings: map[string]any{}},
	}, nil
}
