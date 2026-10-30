package vpp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// securityACLAttachment is one (interface, direction) pair an ACL must be
// attached to. A rule selected for several LANs has one pair per LAN.
type securityACLAttachment struct {
	Interface string
	Direction string
}

func (channel vppctlChannel) doSecurityACLLifecycle(ctx context.Context, operation Operation, acl trafficpolicy.SecurityACL, deleting bool) (Reply, error) {
	targets := channel.securityACLTargets(operation, acl.Match.Direction)
	tag := "ly-route-" + safeTag(acl.ID)
	results, err := channel.removeSecurityACLState(ctx, operation, tag)
	if err != nil || deleting {
		return routePolicyLifecycleReply(operation, results), err
	}
	if acl.ID == builtInWANDenyACLID {
		// Persisted plans written before this fix may still carry an
		// attachment. removeSecurityACLState has detached it above; never
		// re-attach a catch-all deny (LAN blackhole, or NAT44 return-path
		// drop at WAN ingress).
		return routePolicyLifecycleReply(operation, results), nil
	}
	stableACLID := stableID("security-acl:"+acl.ID, 50000, 49999)
	create := ""
	for _, raw := range operation.VPPCtlCommands {
		command := strings.TrimSpace(strings.TrimPrefix(raw, "?"))
		if strings.HasPrefix(command, "set acl-plugin acl ") {
			create = strings.Replace(command, fmt.Sprintf(" index %d", stableACLID), "", 1)
			break
		}
	}
	if create == "" {
		return Reply{}, snapshotDecodeError("security ACL %q has no creation command", acl.ID)
	}
	created, err := channel.runServiceChainCommands(ctx, operation, create)
	if err != nil {
		return Reply{}, err
	}
	actualACLID, err := allocatedServiceChainACLID(created)
	if err != nil {
		return Reply{}, err
	}
	attachCommands := make([]string, 0, len(targets)+3)
	interfaces := make([]string, 0, len(targets))
	seenInterfaces := map[string]struct{}{}
	for _, target := range targets {
		attachCommands = append(attachCommands, fmt.Sprintf("set acl-plugin interface %s %s acl %d", target.Interface, target.Direction, actualACLID))
		if _, duplicate := seenInterfaces[target.Interface]; duplicate {
			continue
		}
		seenInterfaces[target.Interface] = struct{}{}
		interfaces = append(interfaces, target.Interface)
	}
	attachCommands = append(attachCommands, fmt.Sprintf("show acl-plugin acl index %d", actualACLID), "show acl-plugin interface")
	for _, interfaceName := range interfaces {
		attachCommands = append(attachCommands, "show interface "+interfaceName)
	}
	applied, err := channel.runServiceChainCommands(ctx, operation, attachCommands...)
	if err != nil {
		return Reply{}, err
	}
	results = append(results, created...)
	results = append(results, applied...)
	aclOutput := resultStdoutLast(results, fmt.Sprintf("show acl-plugin acl index %d", actualACLID))
	if err := verifyACLOutput(aclOutput, aclCandidateProof{numericID: actualACLID, id: acl.ID, action: acl.Action, match: acl.Match}); err != nil {
		return Reply{}, fmt.Errorf("%w: ACL output %q", err, aclOutput)
	}
	interfaceOutput := resultStdoutLast(results, "show acl-plugin interface")
	for _, target := range targets {
		identityOutput := resultStdoutLast(results, "show interface "+target.Interface)
		if !securityACLInterfaceAttached(interfaceOutput, identityOutput, target.Interface, target.Direction, actualACLID) {
			return Reply{}, snapshotDecodeError("security ACL %q %s attachment on %s is missing: %q", acl.ID, target.Direction, target.Interface, interfaceOutput)
		}
	}
	return routePolicyLifecycleReply(operation, results), nil
}

// securityACLTargets lists every interface/direction pair named by the
// operation. Older plans carry a single placeholder attachment, which resolves
// to the channel LAN; a multi-LAN plan carries one attachment per LAN.
func (channel vppctlChannel) securityACLTargets(operation Operation, fallbackDirection string) []securityACLAttachment {
	targets := make([]securityACLAttachment, 0, 2)
	seen := map[string]struct{}{}
	add := func(interfaceName, direction string) {
		interfaceName = strings.TrimSpace(interfaceName)
		direction = strings.ToLower(strings.TrimSpace(direction))
		if interfaceName == "" || (direction != "input" && direction != "output") {
			return
		}
		key := interfaceName + "\x00" + direction
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, securityACLAttachment{Interface: interfaceName, Direction: direction})
	}
	for _, raw := range operation.VPPCtlCommands {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(raw, "?")))
		if len(fields) < 6 || fields[0] != "set" || fields[1] != "interface" || fields[3] != "acl" || fields[4] != "intfc" {
			continue
		}
		add(fields[5], fields[2])
	}
	if len(targets) > 0 {
		return targets
	}
	interfaceName := strings.TrimSpace(channel.lanVPPInterface)
	if interfaceName == "" {
		interfaceName = "lyroute-$LY_ROUTE_LAN_INTERFACE"
	}
	for _, direction := range securityDirections(fallbackDirection) {
		add(interfaceName, direction)
	}
	return targets
}

// removeSecurityACLState detaches and deletes an ACL by tag on every VPP
// interface. Cleanup must not depend on the LAN inventory that was current
// when the rule was created: a rule attached to a LAN that was later removed
// would otherwise survive the delete and keep filtering traffic.
func (channel vppctlChannel) removeSecurityACLState(ctx context.Context, operation Operation, tag string) ([]VPPCTLCommandResult, error) {
	results, err := channel.runServiceChainCommands(ctx, operation, "show interface", "show acl-plugin acl", "show acl-plugin interface")
	if err != nil {
		return nil, err
	}
	interfaces := securityGenerationInterfaceNames(resultStdout(results, "show interface"))
	owned := taggedServiceChainACLIDs(resultStdout(results, "show acl-plugin acl"), tag)
	if len(owned) > 0 && len(interfaces) == 0 {
		return nil, snapshotDecodeError("security ACL tag %q cleanup cannot map VPP interfaces", tag)
	}
	for _, id := range owned {
		removeCommands := make([]string, 0, 2*len(interfaces)+1)
		for _, interfaceName := range interfaces {
			for _, attachDirection := range []string{"input", "output"} {
				removeCommands = append(removeCommands, fmt.Sprintf("?set acl-plugin interface %s %s acl %d del", interfaceName, attachDirection, id))
			}
		}
		removeCommands = append(removeCommands, fmt.Sprintf("delete acl-plugin acl index %d", id))
		removed, removeErr := channel.runServiceChainCommands(ctx, operation, removeCommands...)
		if removeErr != nil {
			return nil, removeErr
		}
		results = append(results, removed...)
	}
	verified, err := channel.runServiceChainCommands(ctx, operation, "show acl-plugin acl", "show acl-plugin interface")
	if err != nil {
		return nil, err
	}
	results = append(results, verified...)
	if len(taggedServiceChainACLIDs(resultStdoutLast(results, "show acl-plugin acl"), tag)) != 0 {
		return nil, snapshotDecodeError("security ACL tag %q remains after deletion", tag)
	}
	return results, nil
}

func securityACLInterfaceAttached(output, identityOutput, interfaceName, direction string, aclID int) bool {
	for _, attachedID := range securityACLInterfaceIDs(output, identityOutput, interfaceName, direction) {
		if attachedID == aclID {
			return true
		}
	}
	return false
}

func securityACLInterfaceIDs(output, identityOutput, interfaceName, direction string) []int {
	if lan := strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_INTERFACE")); lan != "" {
		interfaceName = strings.ReplaceAll(interfaceName, "$LY_ROUTE_LAN_INTERFACE", lan)
	}
	interfaceIndex := ""
	for _, line := range nonBlankLines(identityOutput) {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == interfaceName {
			if _, err := strconv.Atoi(fields[1]); err == nil {
				interfaceIndex = fields[1]
				break
			}
		}
	}
	if interfaceIndex == "" {
		return nil
	}
	currentInterface := ""
	result := []int{}
	for _, line := range nonBlankLines(output) {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "sw_if_index ") {
			currentInterface = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(lower, "sw_if_index ")), ":")
			continue
		}
		if currentInterface != interfaceIndex || !strings.Contains(lower, direction) {
			continue
		}
		normalized := strings.NewReplacer("[", " ", "]", " ", ":", " ", ",", " ").Replace(line)
		for _, field := range strings.Fields(normalized) {
			if id, err := strconv.Atoi(field); err == nil {
				result = append(result, id)
			}
		}
	}
	return result
}
