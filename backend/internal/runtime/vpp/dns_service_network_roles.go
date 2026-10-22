package vpp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/nat"
)

// VerifyDNSServiceNetworks is the graph edge predicate that must hold before
// route/ABF nodes consume a DNS service handoff: the TAP exists and it owns a
// NAT44 input role on its resolved underlay.
func (adapter Adapter) VerifyDNSServiceNetworks(ctx context.Context, plan Plan) error {
	if len(plan.DNSServiceNetworks) == 0 {
		return nil
	}
	if adapter.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open service-network verify channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	for _, network := range plan.DNSServiceNetworks {
		if err := waitDNSServiceNetworkNAT(ctx, channel, network, plan.NAT.Behavior); err != nil {
			return err
		}
	}
	return nil
}

// VerifyNAT44Ready is the graph edge predicate between nat44 and port-maps.
func (adapter Adapter) VerifyNAT44Ready(ctx context.Context, plan Plan) error {
	if len(plan.NAT.StaticMappings) == 0 && len(plan.NAT.PortMappings) == 0 {
		return nil
	}
	if adapter.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open NAT44 verify channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	command := "show nat44 interfaces"
	if plan.NAT.Behavior == nat.BehaviorFullCone {
		command = "show nat44 ei interfaces"
	}
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.nat44.verify", RequestID: plan.RequestID, VPPCtlCommands: []string{command}})
	if err != nil {
		return fmt.Errorf("NAT44 is not initialized before port mappings: %w", err)
	}
	if payload, ok := reply.Payload.(VPPCTLReplyPayload); ok {
		for _, result := range payload.CommandResults {
			if result.Command == command && strings.TrimSpace(result.Stdout) != "" {
				return nil
			}
		}
	}
	return fmt.Errorf("NAT44 is not initialized before port mappings: %s returned no interfaces", command)
}

// ensureDNSServiceNetworkNAT repairs the only non-optional dependency inside a
// vpp.dns-service.network operation.  VPP can accept the TAP/table/route
// commands while the underlay session interface is still being created; the
// bind can then fail silently because the operation marked it optional.  The
// apply graph verifies the role and retries the bind here instead of reporting
// a "snapshot incomplete" error later.
func ensureDNSServiceNetworkNAT(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	if err := waitDNSServiceNetworkNAT(ctx, channel, network, behavior); err == nil {
		return nil
	}
	underlay := dnsServiceNetworkUnderlayInterface(network)
	if strings.HasPrefix(strings.TrimSpace(underlay), "pppoe-runtime:") {
		resolved, err := resolveRuntimePPPoEInterface(underlay)
		if err != nil {
			return fmt.Errorf("DNS service interface %q cannot attach NAT44: %w", network.VPPInterface, err)
		}
		underlay = resolved
	}
	if strings.TrimSpace(underlay) == "" {
		return fmt.Errorf("DNS service interface %q has no underlay for its NAT44 input role", network.VPPInterface)
	}
	commands := []string{}
	if behavior == nat.BehaviorFullCone {
		commands = append(commands,
			fmt.Sprintf("?set interface nat44 ei in %s out %s output-feature del", network.VPPInterface, underlay),
			fmt.Sprintf("set interface nat44 ei in %s out %s", network.VPPInterface, underlay),
			fmt.Sprintf("?nat44 ei add interface address %s", underlay),
		)
	} else {
		commands = append(commands,
			fmt.Sprintf("?set interface nat44 in %s out %s output-feature del", network.VPPInterface, underlay),
			fmt.Sprintf("set interface nat44 in %s out %s", network.VPPInterface, underlay),
		)
	}
	if _, err := doOperation(ctx, channel, Operation{
		Name:           "vpp.dns-service.network.nat44-role",
		RequestID:      network.UpstreamID,
		Resource:       network.UpstreamID,
		Payload:        network,
		VPPCtlCommands: commands,
	}); err != nil {
		return fmt.Errorf("DNS service interface %q NAT44 input role attach failed on underlay %q: %w", network.VPPInterface, underlay, err)
	}
	if err := waitDNSServiceNetworkNAT(ctx, channel, network, behavior); err != nil {
		return fmt.Errorf("DNS service interface %q is missing its NAT44 input role on underlay %q after attach: %w", network.VPPInterface, underlay, err)
	}
	return nil
}

func waitDNSServiceNetworkNAT(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	command := dnsNATShowCommand(behavior)
	attempts := 25
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_NAT_ROLE_ATTEMPTS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attempts = parsed
		}
	}
	interval := 200 * time.Millisecond
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_NAT_ROLE_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	var last string
	for attempt := 1; attempt <= attempts; attempt++ {
		reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-readback", RequestID: network.UpstreamID, VPPCtlCommands: []string{command}})
		if err == nil {
			if payload, ok := reply.Payload.(VPPCTLReplyPayload); ok {
				for _, result := range payload.CommandResults {
					if result.Command != command {
						continue
					}
					last = strings.TrimSpace(result.Stdout)
					if natRolePresent(last, network.VPPInterface) {
						return nil
					}
				}
			}
		} else {
			last = err.Error()
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("NAT44 input role for %q was not observed in %s (%s)", network.VPPInterface, command, strings.TrimSpace(last))
}

func dnsNATShowCommand(behavior nat.Behavior) string {
	if behavior == nat.BehaviorFullCone {
		return "show nat44 ei interfaces"
	}
	return "show nat44 interfaces"
}

func natRolePresent(output, vppInterface string) bool {
	vppInterface = strings.TrimSpace(vppInterface)
	if vppInterface == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == vppInterface && fields[1] == "in" {
			return true
		}
	}
	return false
}
