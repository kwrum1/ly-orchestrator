package httpapi

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/vpp"
)

const (
	proxyIngressHostPrefix = "lypxhin"
	proxyEgressHostPrefix  = "lypxhout"
	proxyIngressVPPPrefix  = "lypxin"
	proxyEgressVPPPrefix   = "lypxout"
)

// resolveLiveProxyServiceNetwork prefers the proxy service TAP names that
// actually exist right now over the deterministic names frozen into a plan.
//
// A VPP restart or an egress rebuild can leave the persisted plan pointing at a
// ly_px TAP that no longer exists while a differently-suffixed TAP is live. The
// nftables capture rules and the ABF path must name the live interface or the
// steering is silently dead. Resolution is deliberately conservative: the
// planned name wins when it is live, exactly one candidate is required, and an
// empty/ambiguous inventory leaves the plan untouched.
func (server *Server) resolveLiveProxyServiceNetwork(ctx context.Context, network proxy.ProxyServiceNetwork) proxy.ProxyServiceNetwork {
	if strings.TrimSpace(network.EgressID) == "" {
		return network
	}
	if live, ok := resolveLiveHostInterface(network.IngressHostInterface, proxyIngressHostPrefix); ok {
		network.IngressHostInterface = live
	}
	if live, ok := resolveLiveHostInterface(network.EgressHostInterface, proxyEgressHostPrefix); ok {
		network.EgressHostInterface = live
	}
	if names, err := liveVPPInterfaceNames(ctx); err == nil {
		if live, ok := resolveLiveVPPInterface(names, network.IngressVPPInterface, proxyIngressVPPPrefix); ok {
			network.IngressVPPInterface = live
		}
		if live, ok := resolveLiveVPPInterface(names, network.EgressVPPInterface, proxyEgressVPPPrefix); ok {
			network.EgressVPPInterface = live
		}
	}
	return network
}

func liveVPPInterfaceNames(ctx context.Context) (map[string]bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, liveVPPBinary(), "show", "interface").Output()
	if err != nil {
		return nil, err
	}
	return vpp.LiveInterfaceNames(string(output)), nil
}

func resolveLiveHostInterface(planned, prefix string) (string, bool) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return planned, false
	}
	candidates := make([]string, 0, 2)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			candidates = append(candidates, name)
		}
	}
	return chooseLiveInterface(planned, candidates)
}

func resolveLiveVPPInterface(names map[string]bool, planned, prefix string) (string, bool) {
	candidates := make([]string, 0, 2)
	for name := range names {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			candidates = append(candidates, name)
		}
	}
	return chooseLiveInterface(planned, candidates)
}

func chooseLiveInterface(planned string, candidates []string) (string, bool) {
	planned = strings.TrimSpace(planned)
	sort.Strings(candidates)
	for _, candidate := range candidates {
		if candidate == planned {
			return planned, false
		}
	}
	if len(candidates) != 1 {
		return planned, false
	}
	return candidates[0], true
}
