package vpp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/proxy"
)

type SupplementalOwner string

const (
	SupplementalInterfaces SupplementalOwner = "interfaces"
	// SupplementalServiceNetworks owns DNS/proxy service handoffs. They depend
	// on the native underlay, LCP and PPPoE helper, but are independent of the
	// user route/ABF graph; keeping them separate lets the apply graph order
	// and heal their NAT44 role explicitly.
	SupplementalServiceNetworks SupplementalOwner = "service-networks"
	SupplementalRoutes          SupplementalOwner = "routes"
	SupplementalSecurity        SupplementalOwner = "security"
	SupplementalQoS             SupplementalOwner = "qos"
)

type SupplementalOperationReadback struct {
	Name        string                `json:"name"`
	Resource    string                `json:"resource"`
	PayloadHash string                `json:"payload_hash"`
	Shows       []VPPCTLCommandResult `json:"shows"`
}

func HasSupplementalOperations(plan Plan, owner SupplementalOwner) bool {
	operations, err := supplementalOperations(plan, owner)
	return err == nil && len(operations) > 0
}

func SupplementalOperationsEqual(left, right Plan, owner SupplementalOwner) (bool, error) {
	leftOperations, err := supplementalOperations(left, owner)
	if err != nil {
		return false, err
	}
	rightOperations, err := supplementalOperations(right, owner)
	if err != nil {
		// A persisted prior generation can no longer be turned back into an
		// operation list when its dataplane capability proof has expired (the
		// proof window is deliberately short). That is not evidence that the
		// prior supplemental objects are equal to the desired ones: without a
		// provable prior list the reconciler must run the idempotent cleanup.
		// Treat the prior as different instead of failing the whole rollback or
		// reconciliation, which would leave introduced objects behind.
		return false, nil
	}
	if len(leftOperations) != len(rightOperations) {
		return false, nil
	}
	for index := range leftOperations {
		leftOperation, rightOperation := leftOperations[index], rightOperations[index]
		if leftOperation.Name != rightOperation.Name || leftOperation.Resource != rightOperation.Resource {
			return false, nil
		}
		leftHash, err := supplementalOperationHash(leftOperation)
		if err != nil {
			return false, err
		}
		rightHash, err := supplementalOperationHash(rightOperation)
		if err != nil {
			return false, err
		}
		if leftHash != rightHash {
			return false, nil
		}
	}
	return true, nil
}

func (a Adapter) ApplySupplemental(ctx context.Context, plan Plan, owner SupplementalOwner) ([]SupplementalOperationReadback, error) {
	operations, err := supplementalOperations(plan, owner)
	if err != nil || len(operations) == 0 {
		return nil, err
	}
	if a.Client == nil {
		return nil, fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: open supplemental channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	readbacks := make([]SupplementalOperationReadback, 0, len(operations))
	for _, operation := range operations {
		reply, operationErr := doOperation(ctx, channel, operation)
		if operationErr != nil {
			return nil, operationErr
		}
		envelope, ok := reply.Payload.(VPPCTLReplyPayload)
		if !ok {
			return nil, fmt.Errorf("%w: supplemental operation %q returned untyped payload %T", ErrSnapshotIncomplete, operation.Name, reply.Payload)
		}
		readback := SupplementalOperationReadback{Name: operation.Name, Resource: operation.Resource}
		for _, result := range envelope.CommandResults {
			if strings.HasPrefix(result.Command, "show ") {
				// Lifecycle operations deliberately start by proving that an object is
				// absent.  An empty initial inventory is therefore valid evidence; the
				// operation-specific verifier below requires the final semantic state.
				readback.Shows = append(readback.Shows, result)
			}
		}
		if len(readback.Shows) == 0 {
			return nil, fmt.Errorf("%w: supplemental operation %q has no typed show readback", ErrSnapshotIncomplete, operation.Name)
		}
		if network, ok := operation.Payload.(DNSServiceNetwork); ok {
			if err := ensureDNSServiceNetworkNAT(ctx, channel, network, plan.NAT.Behavior); err != nil {
				return nil, err
			}
			// The role may have been repaired after the operation's first show;
			// append a fresh observation so evidence reflects the committed state.
			show := dnsNATShowCommand(plan.NAT.Behavior)
			poolShow := nat44PoolShowCommand(plan.NAT.Behavior)
			verifyReply, verifyErr := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-readback", RequestID: operation.RequestID, VPPCtlCommands: []string{show, poolShow}})
			if verifyErr != nil {
				return nil, verifyErr
			}
			if verifyEnvelope, ok := verifyReply.Payload.(VPPCTLReplyPayload); ok {
				envelope.CommandResults = append(envelope.CommandResults, verifyEnvelope.CommandResults...)
				for _, result := range verifyEnvelope.CommandResults {
					if strings.HasPrefix(result.Command, "show ") {
						readback.Shows = append(readback.Shows, result)
					}
				}
			}
		}
		verifyOperation := operation
		if envelope.ProxyServiceNetwork != nil {
			verifyOperation = proxyServiceNetworkVerificationOperation(operation, *envelope.ProxyServiceNetwork)
		}
		if err := verifySupplementalOperation(verifyOperation, envelope.CommandResults); err != nil {
			return nil, err
		}
		readback.PayloadHash, err = supplementalOperationHash(operation)
		if err != nil {
			return nil, err
		}
		readbacks = append(readbacks, readback)
	}
	return readbacks, nil
}

// ReadbackSupplemental proves an unchanged supplemental owner from live show
// commands only. Repeat applies use it to avoid re-creating TAPs/NAT roles.
func (a Adapter) ReadbackSupplemental(ctx context.Context, plan Plan, owner SupplementalOwner) ([]SupplementalOperationReadback, error) {
	operations, err := supplementalOperations(plan, owner)
	if err != nil || len(operations) == 0 {
		return nil, err
	}
	if a.Client == nil {
		return nil, fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: open supplemental readback channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	readbacks := make([]SupplementalOperationReadback, 0, len(operations))
	for _, operation := range operations {
		readbackOperation, err := readbackProxyServiceNetworkOperation(ctx, channel, operation)
		if err != nil {
			return nil, err
		}
		shows := make([]string, 0, len(readbackOperation.VPPCtlCommands))
		for _, command := range readbackOperation.VPPCtlCommands {
			command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?"))
			if strings.HasPrefix(command, "show ") {
				shows = append(shows, command)
			}
		}
		if len(shows) == 0 {
			return nil, fmt.Errorf("%w: supplemental operation %q has no show commands", ErrSnapshotIncomplete, operation.Name)
		}
		reply, err := doOperation(ctx, channel, Operation{Name: operation.Name + ".readback", RequestID: operation.RequestID, Resource: operation.Resource, VPPCtlCommands: shows})
		if err != nil {
			return nil, err
		}
		envelope, ok := reply.Payload.(VPPCTLReplyPayload)
		if !ok {
			return nil, fmt.Errorf("%w: supplemental readback %q returned untyped payload", ErrSnapshotIncomplete, operation.Name)
		}
		if err := verifySupplementalOperation(readbackOperation, envelope.CommandResults); err != nil {
			return nil, err
		}
		// The evidence hash always belongs to the plan operation: a live rename
		// is topology, not a desired-state change.
		hash, err := supplementalOperationHash(operation)
		if err != nil {
			return nil, err
		}
		readbacks = append(readbacks, SupplementalOperationReadback{Name: operation.Name, Resource: operation.Resource, PayloadHash: hash, Shows: envelope.CommandResults})
	}
	return readbacks, nil
}

func supplementalOperations(plan Plan, owner SupplementalOwner) ([]Operation, error) {
	operations, err := BuildOperations(plan)
	if err != nil {
		return nil, err
	}
	selected := make([]Operation, 0)
	for _, operation := range operations {
		if target, direct := operation.Payload.(flow.Target); direct && targetOwnedByGroup(plan.Flow.VPPGroups, target) {
			continue
		}
		if supplementalOperationOwner(operation) == owner {
			selected = append(selected, operation)
		}
	}
	return selected, nil
}

// readbackProxyServiceNetworkOperation resolves the service TAP names of a
// proxy-service readback against the live topology. The VPPCTL channel skips
// the proxy lifecycle hook for readback-only operations, so without this the
// repeat apply would `show interface address` the frozen plan name.
func readbackProxyServiceNetworkOperation(ctx context.Context, channel Channel, operation Operation) (Operation, error) {
	resolver, ok := channel.(proxyServiceNetworkInterfaceResolver)
	if !ok {
		return operation, nil
	}
	steering, isProxy := operation.Payload.(proxy.VPPSteeringInstruction)
	if !isProxy || steering.TargetKind != "vpp.proxy-service.network" {
		return operation, nil
	}
	network := steering.ServiceNetwork
	if strings.TrimSpace(network.EgressID) == "" {
		network = proxy.ServiceNetworkForEgressID(steering.EgressID)
	}
	resolved, presence, err := resolver.resolveProxyServiceNetworkInterfaces(ctx, network)
	if err != nil {
		return operation, err
	}
	if !presence.complete() {
		return operation, fmt.Errorf("proxy service network %q is absent from the current VPP topology: %s; the committed generation was not re-applied", network.EgressID, describeProxyServiceNetworkPresence(network, presence))
	}
	return proxyServiceNetworkOperationWithInterfaces(operation, network, resolved, true), nil
}

// activeSupplementalReadback drops evidence rows that belong to a retired
// feature. A gateway upgraded from firmware that applied built-in Smart QoS
// still carries those rows in its committed plan, while arch-v1 no longer
// builds the operation; ignoring them keeps restart evidence valid instead of
// surfacing a false snapshot_mismatch.
func activeSupplementalReadback(items []SupplementalOperationReadback) []SupplementalOperationReadback {
	active := make([]SupplementalOperationReadback, 0, len(items))
	for _, item := range items {
		if strings.HasPrefix(strings.TrimSpace(item.Name), "vpp.smart-qos") {
			continue
		}
		active = append(active, item)
	}
	return active
}

func targetOwnedByGroup(groups []flow.VPPObjectGroup, target flow.Target) bool {
	for _, group := range groups {
		if group.Kind != target.Kind {
			continue
		}
		for _, object := range group.Objects {
			if object.RuleID == target.RuleID {
				return true
			}
		}
	}
	return false
}

// ValidateCommittedSupplementalReadback validates the persisted evidence of an
// already-committed transaction without rebuilding payload hashes from live
// capability proofs. The payload hashes and show output were validated when
// the transaction committed; after a restart the proof window may legitimately
// have expired, which must not turn a healthy committed gateway into stale
// degraded state. Missing or malformed evidence still fails closed.
func ValidateCommittedSupplementalReadback(plan Plan, owner SupplementalOwner, actual []SupplementalOperationReadback, referenceTime time.Time) error {
	plan.NativePath.Now = referenceTime
	actual = activeSupplementalReadback(actual)
	operations, err := supplementalOperations(plan, owner)
	if err != nil {
		return err
	}
	if len(operations) != len(actual) {
		return fmt.Errorf("%w: supplemental %s evidence count is %d, want %d", ErrSnapshotIncomplete, owner, len(actual), len(operations))
	}
	for index, operation := range operations {
		item := actual[index]
		if item.Name == "" || item.Resource == "" || item.PayloadHash == "" || len(item.Shows) == 0 {
			return fmt.Errorf("%w: supplemental %s committed evidence is incomplete", ErrSnapshotIncomplete, owner)
		}
		if item.Name != operation.Name || item.Resource != operation.Resource {
			return fmt.Errorf("%w: supplemental %s committed evidence does not match operation %q", ErrSnapshotIncomplete, owner, operation.Name)
		}
	}
	return nil
}

func ValidateSupplementalReadback(plan Plan, owner SupplementalOwner, actual []SupplementalOperationReadback, referenceTime time.Time) error {
	evidencePlan := plan
	evidencePlan.NativePath.Now = referenceTime
	actual = activeSupplementalReadback(actual)
	operations, err := supplementalOperations(evidencePlan, owner)
	if err != nil {
		return err
	}
	if len(operations) != len(actual) {
		return fmt.Errorf("%w: supplemental %s evidence count is %d, want %d", ErrSnapshotIncomplete, owner, len(actual), len(operations))
	}
	for index, operation := range operations {
		hash, hashErr := supplementalOperationHash(operation)
		if hashErr != nil {
			return hashErr
		}
		if actual[index].Name != operation.Name || actual[index].Resource != operation.Resource || actual[index].PayloadHash != hash {
			return fmt.Errorf("%w: supplemental %s evidence does not match desired operation %q", ErrSnapshotIncomplete, owner, operation.Name)
		}
		if verifyErr := verifySupplementalOperation(operation, actual[index].Shows); verifyErr != nil {
			return verifyErr
		}
	}
	return nil
}

func supplementalOperationOwner(operation Operation) SupplementalOwner {
	switch operation.Name {
	case "vpp.dataplane.attach", "vpp.lan-control-lcp", "vpp.management-lcp":
		return SupplementalInterfaces
	case "vpp.proxy-service.network", "vpp.dns-service.network":
		return SupplementalServiceNetworks
	case "vpp.abf.policy", "vpp.pbr.policy", "vpp.service-chain.egress-binding", "vpp.dns-transparent-interception":
		return SupplementalRoutes
	case "vpp.smart-qos":
		return SupplementalQoS
	case "vpp.security-generation":
		return SupplementalSecurity
	case "vpp.acl.drop", "vpp.behavior.rate", "vpp.qos.classify", "vpp.qos.record", "vpp.qos.store", "vpp.qos.egress-map", "vpp.qos.mark", "vpp.policer":
		if _, direct := operation.Payload.(flow.Target); direct {
			return SupplementalQoS
		}
	}
	return ""
}

func SupplementalCleanupOperations(plan Plan, owner SupplementalOwner) ([]Operation, error) {
	operations, err := supplementalOperations(plan, owner)
	if err != nil {
		return nil, err
	}
	return supplementalCleanupOperationsFor(plan, operations)
}

// SupplementalIntroducedCleanupOperations tears down only supplemental objects
// that the desired plan introduces and the previous generation did not own.
// Rollback must never delete a pre-existing prerequisite such as a LAN LCP
// pair: doing so removes the Linux address that rollback is about to preserve.
// Objects that exist in both plans are restored in place by RollbackRestore.
func SupplementalIntroducedCleanupOperations(prior, desired Plan, owner SupplementalOwner) ([]Operation, error) {
	desiredOperations, err := supplementalOperations(desired, owner)
	if err != nil {
		// The desired plan is normally compilable (it was just applied), but a
		// rollback must not fail or guess which live prerequisite is
		// disposable when it is not. Keep every live object.
		return nil, nil
	}
	priorIdentities, err := supplementalOperationIdentities(prior, owner)
	if err != nil {
		// The previous generation cannot be compiled into operations (for
		// example its capability proof expired). Identity comparison is then
		// unavailable; keeping live prerequisites is safer than tearing down a
		// working LAN to repair a failed apply.
		return nil, nil
	}
	introduced := make([]Operation, 0, len(desiredOperations))
	for _, operation := range desiredOperations {
		if _, existed := priorIdentities[supplementalOperationObjectKey(operation)]; existed {
			continue
		}
		introduced = append(introduced, operation)
	}
	return supplementalCleanupOperationsFor(desired, introduced)
}

func supplementalOperationIdentities(plan Plan, owner SupplementalOwner) (map[string]struct{}, error) {
	operations, err := supplementalOperations(plan, owner)
	if err != nil {
		return nil, err
	}
	identities := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		identities[supplementalOperationObjectKey(operation)] = struct{}{}
	}
	return identities, nil
}

func supplementalOperationObjectKey(operation Operation) string {
	return strings.TrimSpace(operation.Name) + "\x00" + strings.TrimSpace(operation.Resource)
}

func supplementalCleanupOperationsFor(plan Plan, operations []Operation) ([]Operation, error) {
	// Rollback must tear down dependants before their prerequisites.  For the
	// interfaces owner that means detaching LCP/ABF and removing the reserved
	// DHCP bypass policy before the native attachment (and therefore the VPP
	// interface) is deleted; otherwise `show abf attach` fails with an unknown
	// interface and policy 65000 survives the rollback.  Cleanup therefore
	// reverses the apply order for every owner.
	cleanup := make([]Operation, 0, len(operations))
	for index := len(operations) - 1; index >= 0; index-- {
		operation := operations[index]
		if attachment, ok := operation.Payload.(NativeAttachment); ok && attachment.Tier == DataplaneTierDPDK {
			// PCI/VFIO restoration belongs to the privileged dataplane transaction,
			// never to an invented VPP CLI detach command.
			continue
		}
		commands, commandErr := supplementalCleanupCommands(operation)
		if commandErr != nil {
			return nil, commandErr
		}
		payload := operation.Payload
		if management, ok := payload.(ManagementLCP); ok {
			management.Enabled = false
			payload = management
		}
		cleanup = append(cleanup, Operation{Name: operation.Name + ".rollback-delete", RequestID: plan.RequestID, Resource: operation.Resource, Payload: payload, VPPCtlCommands: commands})
	}
	return cleanup, nil
}

// SupplementalReconciliationCleanupOperations returns cleanup operations for
// DNS and proxy service networks that existed in the persisted plan but are
// absent or materially changed in the desired plan. These resources are
// declarative plan data, so reconciliation must not rebuild the prior native
// path: its capability proofs can legitimately be expired by the next apply.
func SupplementalReconciliationCleanupOperations(prior, desired Plan, owner SupplementalOwner) ([]Operation, error) {
	priorOperations, err := supplementalReconciliationOperations(prior, owner)
	if err != nil {
		return nil, err
	}
	desiredOperations, err := supplementalReconciliationOperations(desired, owner)
	if err != nil {
		return nil, err
	}
	desiredIdentities := make(map[string]struct{}, len(desiredOperations))
	for _, operation := range desiredOperations {
		identity, identityErr := supplementalReconciliationIdentity(operation, owner)
		if identityErr != nil {
			return nil, identityErr
		}
		desiredIdentities[identity] = struct{}{}
	}
	cleanup := make([]Operation, 0)
	for _, operation := range priorOperations {
		identity, identityErr := supplementalReconciliationIdentity(operation, owner)
		if identityErr != nil {
			return nil, identityErr
		}
		if _, retained := desiredIdentities[identity]; retained {
			continue
		}
		if attachment, ok := operation.Payload.(NativeAttachment); ok && attachment.Tier == DataplaneTierDPDK {
			continue
		}
		commands, commandErr := supplementalCleanupCommands(operation)
		if commandErr != nil {
			return nil, commandErr
		}
		payload := operation.Payload
		if management, ok := payload.(ManagementLCP); ok {
			management.Enabled = false
			payload = management
		}
		cleanup = append(cleanup, Operation{
			Name:           operation.Name + ".reconcile-delete",
			RequestID:      desired.RequestID,
			Resource:       operation.Resource,
			Payload:        payload,
			VPPCtlCommands: commands,
		})
	}
	return cleanup, nil
}

// supplementalReconciliationOperations selects the objects whose absence from
// the next generation must be actively cleaned up.
//
// Service networks keep their full declarative operation set. For the
// interfaces owner only the LAN/management LCP pairs are reconciled: a
// cleared role must delete its lylan-* pair, but a role edit must never detach
// an otherwise healthy native attachment (that would turn a direction change
// into a takeover-mode change).
func supplementalReconciliationOperations(plan Plan, owner SupplementalOwner) ([]Operation, error) {
	if owner == SupplementalServiceNetworks {
		return supplementalServiceNetworkOperations(plan, owner)
	}
	if owner != SupplementalInterfaces {
		return nil, nil
	}
	return interfaceLCPReconciliationOperations(plan), nil
}

// interfaceLCPReconciliationOperations reconstructs only the LAN/management LCP
// identities. It deliberately avoids BuildOperations: a persisted prior
// generation can have an expired native proof, and role cleanup must not fail
// (or detach a healthy native attachment) just because that proof expired.
func interfaceLCPReconciliationOperations(plan Plan) []Operation {
	operations := make([]Operation, 0, len(plan.AddressAssignments)+1)
	seen := make(map[string]bool, len(plan.AddressAssignments))
	for _, assignment := range plan.AddressAssignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		vppInterface := strings.TrimSpace(assignment.VPPInterface)
		if vppInterface == "" || seen[vppInterface] {
			continue
		}
		seen[vppInterface] = true
		lanAddress := lanIPv4AddressForInterface(plan.AddressAssignments, vppInterface)
		operations = append(operations, Operation{
			Name:     "vpp.lan-control-lcp",
			Resource: strings.TrimSpace(assignment.ID),
			Payload: ManagementLCP{
				Enabled:      true,
				VPPInterface: vppInterface,
				HostInterface: LANControlPlaneHostInterface(
					assignment.LinuxInterface,
				),
				DNSSteering: plan.DNSABFSteering,
				LANAddress:  lanAddress,
			},
		})
	}
	if strings.TrimSpace(plan.NativePath.ManagementInterface) != "" {
		management, _ := managementLCPOperation(plan)
		operations = append(operations, Operation{Name: "vpp.management-lcp", Resource: "management-network", Payload: management})
	}
	return operations
}

// supplementalReconciliationIdentity uses the object identity for interface
// LCPs so an address/DHCP-policy update is applied in place instead of
// deleting and recreating the Linux control plane. Service networks retain the
// payload hash because any material change requires a full NAT/TAP rebuild.
func supplementalReconciliationIdentity(operation Operation, owner SupplementalOwner) (string, error) {
	if owner == SupplementalInterfaces {
		return strings.TrimSpace(operation.Name) + "\x00" + strings.TrimSpace(operation.Resource), nil
	}
	return supplementalOperationIdentity(operation)
}

func supplementalServiceNetworkOperations(plan Plan, owner SupplementalOwner) ([]Operation, error) {
	if owner != SupplementalServiceNetworks {
		return nil, nil
	}
	operations := make([]Operation, 0, len(plan.DNSServiceNetworks)+len(plan.Proxy.VPPSteering))
	poolAddresses, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		return nil, err
	}
	for index, network := range plan.DNSServiceNetworks {
		if err := validateDNSServiceNetwork(network); err != nil {
			return nil, err
		}
		network.NATPoolAddress = poolAddresses[index]
		operations = append(operations, Operation{
			Name:           "vpp.dns-service.network",
			RequestID:      plan.RequestID,
			Resource:       network.UpstreamID,
			Payload:        network,
			VPPCtlCommands: dnsServiceNetworkCommands(network, plan.NAT.Behavior),
		})
	}
	for _, steering := range plan.Proxy.VPPSteering {
		commands := proxySteeringCommands(steering, plan.NAT.Behavior)
		if steering.TargetKind == "vpp.proxy-service.network" && len(commands) == 0 {
			return nil, fmt.Errorf("proxy egress %q has no resolved VPP underlay route for its service network", steering.EgressID)
		}
		operations = append(operations, Operation{
			Name:           steering.TargetKind,
			RequestID:      plan.RequestID,
			Resource:       steering.EgressID,
			Payload:        steering,
			VPPCtlCommands: commands,
		})
	}
	return operations, nil
}

func supplementalOperationIdentity(operation Operation) (string, error) {
	hash, err := supplementalOperationHash(operation)
	if err != nil {
		return "", err
	}
	return operation.Name + "\x00" + operation.Resource + "\x00" + hash, nil
}

func supplementalCleanupCommands(operation Operation) ([]string, error) {
	switch payload := operation.Payload.(type) {
	case NativeAttachment:
		switch payload.Hook {
		case NativeHookAFPacket:
			return []string{fmt.Sprintf("?delete host-interface name %s", payload.LinuxInterface), "show interface"}, nil
		case NativeHookAFXDP:
			return []string{fmt.Sprintf("?delete interface af_xdp %s", payload.VPPInterface), "show interface"}, nil
		case NativeHookRDMA:
			return []string{fmt.Sprintf("?delete interface rdma %s", payload.VPPInterface), "show interface"}, nil
		default:
			return nil, &UnsupportedOperationError{Name: operation.Name, Resource: operation.Resource}
		}
	case proxy.VPPSteeringInstruction:
		return proxySteeringDeleteCommands(payload), nil
	case DNSServiceNetwork:
		return dnsServiceNetworkDeleteCommands(payload), nil
	case flow.Target:
		return flowTargetDeleteCommands(payload), nil
	case SmartQoSInterface:
		return []string{
			fmt.Sprintf("set ly-route smart-qos interface %s disable", payload.VPPInterface),
			"show ly-route smart-qos",
		}, nil
	case SecurityGeneration:
		return []string{"?show acl-plugin acl", "?show acl-plugin interface", "?show acl-plugin macip acl", "?show acl-plugin macip interface", "?show policer"}, nil
	case ManagementLCP:
		return managementLCPCleanupCommands(), nil
	case DNSTransparentInterception:
		return dnsTransparentCleanupCommands(payload), nil
	default:
		return nil, &UnsupportedOperationError{Name: operation.Name, Resource: operation.Resource}
	}
}
