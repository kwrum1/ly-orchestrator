package vpp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"ly-route/backend/internal/orchestrator"
)

func (channel vppctlChannel) doManagementLCPLifecycle(ctx context.Context, operation Operation, management ManagementLCP) (Reply, error) {
	results, err := channel.runServiceChainCommands(ctx, operation, "show lcp")
	if err != nil {
		return Reply{}, err
	}
	present := false
	for _, pair := range managementLCPPairs(resultStdout(results, "show lcp"), management.HostInterface) {
		if management.Enabled && pair == management.VPPInterface {
			present = true
			continue
		}
		removed, removeErr := channel.runServiceChainCommands(ctx, operation, fmt.Sprintf("lcp delete %s", pair))
		if removeErr != nil {
			return Reply{}, removeErr
		}
		results = append(results, removed...)
	}
	if management.Enabled {
		commands := []string{"lcp lcp-sync on"}
		if !present {
			commands = append(commands, fmt.Sprintf("lcp create %s host-if %s", management.VPPInterface, management.HostInterface))
		}
		if management.IPv4BroadcastLocal {
			commands = append(commands, "ip route add 255.255.255.255/32 via local", "show ip fib 255.255.255.255")
		}
		commands = append(commands, "show lcp")
		applied, applyErr := channel.runServiceChainCommands(ctx, operation, commands...)
		if applyErr != nil {
			return Reply{}, applyErr
		}
		results = append(results, applied...)
		if !managementLCPPresent(resultStdoutLast(results, "show lcp"), management.VPPInterface, management.HostInterface) {
			return Reply{}, snapshotDecodeError("Linux control-plane LCP pair %s/%s is absent from readback", management.VPPInterface, management.HostInterface)
		}
		if management.IPv4BroadcastLocal && !ipv4BroadcastLocalRoutePresent(resultStdoutLast(results, "show ip fib 255.255.255.255")) {
			return Reply{}, snapshotDecodeError("IPv4 limited broadcast route is absent from Linux control-plane readback")
		}
		if management.IPv4DHCPBroadcastBypass {
			// The limited-broadcast route only decides that the DISCOVER is
			// received locally. The per-port punt redirect is what carries it to
			// the host service tap; verify that leg on every apply and repair it
			// when the pair was re-created or the redirect went missing.
			delivery, deliveryErr := channel.ensureLANDHCPDelivery(ctx, operation, management)
			if deliveryErr != nil {
				return Reply{}, deliveryErr
			}
			results = append(results, delivery...)
		}
		// NAT44 roles are reconciled from live state, never by unconditional
		// deletes. The shared nat44RoleReconcileCommands emits only real
		// differences, so repeated applies produce no `set interface nat44:
		// del ... failed` lines. The role plan carries every LAN and WAN for
		// this generation, but only the first LAN operation executes it.
		if management.NAT44RolePlan != nil {
			state, stateResults, stateErr := readNAT44RuntimeStateOn(ctx, channel, operation.RequestID, management.NAT44RolePlan.Behavior)
			if stateErr == nil {
				results = append(results, stateResults...)
				roleCommands := nat44RoleReconcileCommands(management.NAT44RolePlan.LANInterfaces, management.NAT44RolePlan.OutsideInterfaces, management.NAT44RolePlan.Behavior, state)
				if len(roleCommands) > 0 {
					appliedRoles, roleErr := channel.runServiceChainCommands(ctx, operation, roleCommands...)
					if roleErr != nil {
						return Reply{}, roleErr
					}
					results = append(results, appliedRoles...)
				}
			}
		}
		// A LAN ingress that still carries the NAT44 outside role drops every
		// DHCP broadcast before the LCP punt. The reconcile above removes that
		// residue; this readback makes a swallowed delete fail the apply instead
		// of leaving LAN DHCP silently dead.
		if management.IPv4DHCPBroadcastBypass {
			nat44Readback, nat44Err := channel.verifyLANIsNotNAT44Outside(ctx, operation, management.VPPInterface)
			if nat44Err != nil {
				return Reply{}, nat44Err
			}
			results = append(results, nat44Readback...)
		}
		if management.IPv4DHCPBroadcastBypass {
			bypass := lanDHCPBroadcastBypassPolicyFor(management.VPPInterface, management.DHCPBypassPolicyID)
			bypassOperation := Operation{
				Name:      "vpp.lan-dhcp-bypass",
				RequestID: operation.RequestID,
				Resource:  operation.Resource,
				Payload:   bypass,
			}
			bypassReply, bypassErr := channel.doServiceChainApply(ctx, bypassOperation, bypass)
			if bypassErr != nil {
				return Reply{}, bypassErr
			}
			if payload, ok := bypassReply.Payload.(VPPCTLReplyPayload); ok {
				results = append(results, payload.CommandResults...)
			}
		}
		if management.DNSSteering {
			steering, steerErr := channel.applyLANDNSSteering(ctx, operation, management)
			if steerErr != nil {
				return Reply{}, steerErr
			}
			results = append(results, steering...)
		}
		return routePolicyLifecycleReply(operation, results), nil
	}
	if management.IPv4DHCPBroadcastBypass {
		bypass := lanDHCPBroadcastBypassPolicyFor(management.VPPInterface, management.DHCPBypassPolicyID)
		bypassOperation := Operation{
			Name:      "vpp.lan-dhcp-bypass.rollback-delete",
			RequestID: operation.RequestID,
			Resource:  operation.Resource,
			Payload:   bypass,
		}
		bypassReply, bypassErr := channel.doServiceChainDelete(ctx, bypassOperation, bypass)
		if bypassErr != nil {
			return Reply{}, bypassErr
		}
		if payload, ok := bypassReply.Payload.(VPPCTLReplyPayload); ok {
			results = append(results, payload.CommandResults...)
		}
	}
	if management.DNSSteering {
		steering, steerErr := channel.deleteLANDNSSteering(ctx, operation, management)
		if steerErr != nil {
			return Reply{}, steerErr
		}
		results = append(results, steering...)
	}
	verified, verifyErr := channel.runServiceChainCommands(ctx, operation, "show lcp")
	if verifyErr != nil {
		return Reply{}, verifyErr
	}
	results = append(results, verified...)
	if len(managementLCPPairs(resultStdoutLast(results, "show lcp"), management.HostInterface)) != 0 {
		return Reply{}, snapshotDecodeError("exclusive management left LCP host interface %s attached", management.HostInterface)
	}
	return routePolicyLifecycleReply(operation, results), nil
}

const lanDHCPBroadcastBypassPolicyID = 65000

// lanDHCPBroadcastBypassPolicyFor returns the per-ingress DHCP bypass policy.
// ABF policies are single-ingress objects; reusing one reserved policy ID for
// several LAN ports forces a delete/recreate cycle on every apply, which VPP
// 25.10 abf_plugin does not survive (SIGSEGV in fib_path_list_contribute_forwarding).
func lanDHCPBroadcastBypassPolicyFor(ingress string, policyID int) ServiceChainPolicy {
	policy := lanDHCPBroadcastBypassPolicy(ingress)
	if policyID != 0 && policyID != lanDHCPBroadcastBypassPolicyID {
		policy.PolicyID = policyID
		policy.ChainID = policy.ChainID + "-" + safeTag(ingress)
	}
	return policy
}

// lanDHCPBroadcastBypassPolicy is a reserved, receive-only ABF policy.  A
// DHCP client sends its initial discover to 255.255.255.255 before it has an
// address or a route.  User route policies must never capture that packet;
// the local ABF path hands it to the Linux LCP where Dnsmasq can answer it.
func lanDHCPBroadcastBypassPolicy(ingress string) ServiceChainPolicy {
	return ServiceChainPolicy{
		ChainID:          "lan-dhcp-broadcast",
		Direction:        orchestrator.ServiceChainForward,
		Position:         0,
		Group:            "lan-control",
		PolicyID:         lanDHCPBroadcastBypassPolicyID,
		Priority:         1,
		AddressFamily:    "ip4",
		IngressInterface: strings.TrimSpace(ingress),
		NextHop:          "local",
		Match: orchestrator.FlowTuple{
			SourceIP:        "0.0.0.0",
			DestinationIP:   "255.255.255.255",
			Protocol:        orchestrator.ProtocolUDP,
			DestinationPort: 67,
		},
	}
}

func ipv4BroadcastLocalRoutePresent(output string) bool {
	return strings.Contains(output, "255.255.255.255/32") && strings.Contains(output, "dpo-receive") && strings.Contains(output, "local0")
}

func managementLCPPairs(output, hostInterface string) []string {
	pairs := []string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "itf-pair:" {
			continue
		}
		for _, field := range fields[2:] {
			if field == hostInterface {
				pairs = append(pairs, fields[2])
				break
			}
		}
	}
	return pairs
}

func managementLCPPresent(output, vppInterface, hostInterface string) bool {
	for _, pair := range managementLCPPairs(output, hostInterface) {
		if pair == vppInterface {
			return true
		}
	}
	return false
}

// LAN DNS ABF steering is the multi-LAN replacement for the single-interface
// VPP dns-intercept plugin. Two reserved policy families are installed per
// LAN: a local receive policy for the gateway address (dnsmasq on the LCP) and
// a lookup policy that steers every other :53 flow into the VPP DNS service
// table (table 100) where the native adapter owns it.
const lanDNSVPPTableID = 100

func lanDNSABFPolicies(m ManagementLCP) []ServiceChainPolicy {
	ingress := strings.TrimSpace(m.VPPInterface)
	if ingress == "" {
		return nil
	}
	tag := safeTag(ingress)
	policies := make([]ServiceChainPolicy, 0, 4)
	add := func(chain string, id int, priority int, dst string, proto orchestrator.Protocol, nextHop string) {
		policies = append(policies, ServiceChainPolicy{
			ChainID:          chain,
			Direction:        orchestrator.ServiceChainForward,
			Position:         0,
			Group:            "lan-dns",
			PolicyID:         id,
			Priority:         priority,
			AddressFamily:    "ip4",
			IngressInterface: ingress,
			NextHop:          nextHop,
			Match: orchestrator.FlowTuple{
				SourceIP:        "0.0.0.0/0",
				DestinationIP:   dst,
				Protocol:        proto,
				DestinationPort: 53,
			},
		})
	}
	if gateway := strings.TrimSpace(m.LANAddress); gateway != "" {
		add("lan-dns-local-"+tag+"-udp", stableID("lan-dns-local-udp:"+ingress, 64000, 100), 2, gateway, orchestrator.ProtocolUDP, "local")
		add("lan-dns-local-"+tag+"-tcp", stableID("lan-dns-local-tcp:"+ingress, 64100, 100), 3, gateway, orchestrator.ProtocolTCP, "local")
	}
	add("lan-dns-proxy-"+tag+"-udp", stableID("lan-dns-proxy-udp:"+ingress, 64200, 100), 6, "0.0.0.0/0", orchestrator.ProtocolUDP, fmt.Sprintf("ip4-lookup-in-table %d", lanDNSVPPTableID))
	add("lan-dns-proxy-"+tag+"-tcp", stableID("lan-dns-proxy-tcp:"+ingress, 64300, 100), 7, "0.0.0.0/0", orchestrator.ProtocolTCP, fmt.Sprintf("ip4-lookup-in-table %d", lanDNSVPPTableID))
	return policies
}

// LANDNSABFPolicyIDs returns the reserved ABF policy ids the multi-LAN DNS
// steering path installs on one LAN ingress. The desired-dataplane health
// probe derives the same ids so it verifies every LAN, not only the first.
func LANDNSABFPolicyIDs(lanInterface, lanAddress string) []int {
	policies := lanDNSABFPolicies(ManagementLCP{VPPInterface: lanInterface, LANAddress: lanAddress, DNSSteering: true})
	ids := make([]int, 0, len(policies))
	for _, policy := range policies {
		ids = append(ids, policy.PolicyID)
	}
	return ids
}

func (channel vppctlChannel) applyLANDNSSteering(ctx context.Context, operation Operation, management ManagementLCP) ([]VPPCTLCommandResult, error) {
	table, err := channel.runServiceChainCommands(ctx, operation,
		// The ABF path replaces the single-slot dns-intercept plugin. Clear a
		// stale plugin attachment left by an earlier single-LAN generation so
		// only the per-LAN ABF policies steer :53.
		"?set ly-route dns-intercept disable",
		fmt.Sprintf("?ip table add %d", lanDNSVPPTableID),
		fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via local", lanDNSVPPTableID),
		fmt.Sprintf("show ip fib table %d", lanDNSVPPTableID),
	)
	if err != nil {
		return nil, err
	}
	results := append([]VPPCTLCommandResult(nil), table...)
	for _, policy := range lanDNSABFPolicies(management) {
		applied, applyErr := channel.applyLANABFPolicy(ctx, operation, policy)
		if applyErr != nil {
			return nil, applyErr
		}
		results = append(results, applied...)
	}
	return results, nil
}

// applyLANABFPolicy is a dedicated lifecycle for the LAN DNS steering
// policies. It cannot use doServiceChainApply: VPP renders a lookup-DPO ABF
// path as a `deag:` path with a `fib-index:` line, which the service-chain
// parser (correctly) rejects for user service chains. Here the readback is
// scoped to what the steering contract needs: the ACL tag exists and the
// policy is attached to the ingress with the requested next hop.
func (channel vppctlChannel) applyLANABFPolicy(ctx context.Context, operation Operation, policy ServiceChainPolicy) ([]VPPCTLCommandResult, error) {
	results := []VPPCTLCommandResult{}
	listed, err := channel.runServiceChainCommands(ctx, operation, "show acl-plugin acl")
	if err != nil {
		return nil, err
	}
	results = append(results, listed...)
	// Failed generations can leave several ACLs carrying the same tag. Remove
	// every one of them: keeping a stale duplicate makes the freshly created
	// ACL ambiguous and the policy/attach readback then resolves to the wrong
	// index (observed as "DNS ABF policy 64224 is not attached").
	oldACLs := taggedACLIndexes(resultStdoutLast(results, "show acl-plugin acl"), serviceChainACLTag(policy))
	cleanup := []string{
		fmt.Sprintf("?abf attach ip4 del policy %d %s", policy.PolicyID, policy.IngressInterface),
		fmt.Sprintf("?abf policy del id %d", policy.PolicyID),
	}
	for _, index := range oldACLs {
		cleanup = append(cleanup, fmt.Sprintf("?delete acl-plugin acl index %d", index))
	}
	removed, err := channel.runServiceChainCommands(ctx, operation, cleanup...)
	if err != nil {
		return nil, err
	}
	results = append(results, removed...)
	created, err := channel.runServiceChainCommands(ctx, operation, serviceChainACLCommand(policy, false))
	if err != nil {
		return nil, err
	}
	results = append(results, created...)
	policy.ACLID, err = allocatedServiceChainACLID(created)
	if err != nil {
		return nil, err
	}
	applied, err := channel.runServiceChainCommands(ctx, operation,
		fmt.Sprintf("abf policy add id %d acl %d via %s", policy.PolicyID, policy.ACLID, policy.NextHop),
		fmt.Sprintf("abf attach ip4 policy %d priority %d %s", policy.PolicyID, policy.Priority, policy.IngressInterface),
		"show abf attach "+policy.IngressInterface,
		fmt.Sprintf("show abf policy %d", policy.PolicyID),
		fmt.Sprintf("show acl-plugin acl index %d", policy.ACLID),
	)
	if err != nil {
		return nil, err
	}
	results = append(results, applied...)
	attach := resultStdoutLast(results, "show abf attach "+policy.IngressInterface)
	if !strings.Contains(attach, fmt.Sprintf("policy:%d", policy.PolicyID)) {
		// VPP has been seen accepting an attach command while the policy is
		// not yet visible in the interface attachment table (desktop run: the
		// fourth policy of a LAN family). Re-attach once and re-read before
		// treating it as a hard failure.
		repaired, repairErr := channel.runServiceChainCommands(ctx, operation,
			fmt.Sprintf("abf attach ip4 policy %d priority %d %s", policy.PolicyID, policy.Priority, policy.IngressInterface),
			"show abf attach "+policy.IngressInterface,
		)
		if repairErr != nil {
			return nil, repairErr
		}
		results = append(results, repaired...)
		attach = resultStdoutLast(results, "show abf attach "+policy.IngressInterface)
	}
	if !strings.Contains(attach, fmt.Sprintf("policy:%d", policy.PolicyID)) {
		return nil, serviceChainDecodeError("DNS ABF policy %d is not attached to %s (attach retried once)", policy.PolicyID, policy.IngressInterface)
	}
	if strings.HasPrefix(strings.TrimSpace(policy.NextHop), "ip4-lookup-in-table ") {
		if !strings.Contains(attach, fmt.Sprintf("ipv4-VRF:%d", lanDNSVPPTableID)) && !strings.Contains(attach, fmt.Sprintf("lookup in ipv4-VRF:%d", lanDNSVPPTableID)) {
			return nil, serviceChainDecodeError("DNS ABF policy %d is not attached to table %d", policy.PolicyID, lanDNSVPPTableID)
		}
	} else if !strings.Contains(attach, "dpo-receive") {
		return nil, serviceChainDecodeError("DNS ABF policy %d is not attached to the local receive path", policy.PolicyID)
	}
	acl := resultStdoutLast(results, fmt.Sprintf("show acl-plugin acl index %d", policy.ACLID))
	if !strings.Contains(acl, serviceChainACLTag(policy)) {
		return nil, serviceChainDecodeError("DNS ABF ACL %d tag is missing", policy.ACLID)
	}
	return results, nil
}

func taggedACLIndexes(output, tag string) []int {
	indexes := []int{}
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "tag {"+tag+"}") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "acl-index" {
			continue
		}
		if index, err := strconv.Atoi(fields[1]); err == nil {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func taggedACLIndex(output, tag string) int {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "tag {"+tag+"}") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "acl-index" {
			continue
		}
		if index, err := strconv.Atoi(fields[1]); err == nil {
			return index
		}
	}
	return 0
}

func (channel vppctlChannel) deleteLANDNSSteering(ctx context.Context, operation Operation, management ManagementLCP) ([]VPPCTLCommandResult, error) {
	results := []VPPCTLCommandResult{}
	listed, err := channel.runServiceChainCommands(ctx, operation, "show acl-plugin acl")
	if err != nil {
		return nil, err
	}
	results = append(results, listed...)
	for _, policy := range lanDNSABFPolicies(management) {
		commands := []string{
			fmt.Sprintf("?abf attach ip4 del policy %d %s", policy.PolicyID, policy.IngressInterface),
			fmt.Sprintf("?abf policy del id %d", policy.PolicyID),
		}
		if acl := taggedACLIndex(resultStdoutLast(results, "show acl-plugin acl"), serviceChainACLTag(policy)); acl > 0 {
			commands = append(commands, fmt.Sprintf("?delete acl-plugin acl index %d", acl))
		}
		removed, removeErr := channel.runServiceChainCommands(ctx, operation, commands...)
		if removeErr != nil {
			return nil, removeErr
		}
		results = append(results, removed...)
	}
	return results, nil
}
