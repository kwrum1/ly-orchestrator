package vpp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

type Plan struct {
	RequestID          string
	NativePath         NativePathRequest
	AddressAssignments []AddressAssignment
	// SmartQoSAssignments and SmartQoSEnabled are retained for wire/API
	// compatibility with pre-arch-v1 plans, receipts and evidence. They never
	// produce VPP operations: built-in Smart QoS is retired and its plugin is no
	// longer shipped.
	SmartQoSAssignments []AddressAssignment `json:"smart_qos_assignments,omitempty"`
	SmartQoSEnabled     bool                `json:"smart_qos_enabled,omitempty"`
	Interfaces          []InterfaceState
	Bonds               []BondState
	Proxy               proxy.CompiledEgress
	Flow                flow.CompiledIntent
	NAT                 nat.CompiledConfig
	Policy              trafficpolicy.Config
	// RetiredRoutePolicyIDs keeps disabled policy identities available to
	// reconciliation when an older runtime snapshot has already lost them.
	RetiredRoutePolicyIDs []string `json:"retired_route_policy_ids,omitempty"`
	Security              SecurityGeneration
	DNSInterception       bool
	// DNSABFSteering replaces the single-interface dns-intercept plugin with
	// per-ingress ABF policies when a gateway has several LAN interfaces.
	DNSABFSteering bool
	// LANVPPInterfaces lists every configured LAN ingress. NAT44 inside and
	// NAT return-guard attachments must cover all of them, not just the first.
	LANVPPInterfaces   []string            `json:"lan_vpp_interfaces,omitempty"`
	DNSServiceNetworks []DNSServiceNetwork `json:"dns_service_networks,omitempty"`
	// Mirror is the desired port-mirroring (SPAN) generation. It is applied
	// through a supplemental interface operation so a VPP restart replays it
	// from the durable operations file.
	Mirror            *MirrorConfig `json:"mirror,omitempty"`
	DataplanePrepared bool          `json:"-"`
	// AllowUnprovenOperations lets reconciliation derive the operation graph
	// from declarative plan data even when the live dataplane capability proof
	// is no longer valid. Apply paths must leave it false so an unprovable
	// tier still blocks mutation.
	AllowUnprovenOperations bool `json:"-"`
	// SkipQoS excludes QoS objects from live reconciliation. It is set when
	// the running VPP build has no QoS plugin: the dataplane cannot represent
	// those objects, and planning must not create, verify or delete them.
	SkipQoS bool `json:"skip_qos,omitempty"`
}

// DNSInterceptRoute binds one LAN gateway prefix to the VPP interface that
// owns it. The intercept FIB must return every service reply through the LAN
// the query arrived on; pointing every prefix at the first LAN silently
// breaks DNS for all other VLANs.
type DNSInterceptRoute struct {
	Prefix    string `json:"prefix"`
	Interface string `json:"interface"`
}

type DNSTransparentInterception struct {
	LANInterface  string              `json:"lan_interface"`
	LANInterfaces []string            `json:"lan_interfaces,omitempty"`
	IPv4Prefixes  []string            `json:"ipv4_prefixes,omitempty"`
	IPv4Routes    []DNSInterceptRoute `json:"ipv4_routes,omitempty"`
	IPv6Prefixes  []string            `json:"ipv6_prefixes,omitempty"`
}

type ManagementLCP struct {
	Enabled                 bool   `json:"enabled"`
	VPPInterface            string `json:"vpp_interface"`
	HostInterface           string `json:"host_interface"`
	IPv4BroadcastLocal      bool   `json:"ipv4_broadcast_local,omitempty"`
	IPv4DHCPBroadcastBypass bool   `json:"ipv4_dhcp_broadcast_bypass,omitempty"`
	DHCPBypassPolicyID      int    `json:"dhcp_bypass_policy_id,omitempty"`
	// DNSSteering installs per-ingress ABF policies that steer UDP/TCP :53
	// into the VPP DNS service table (or the local LCP for the gateway
	// address). It is the multi-LAN replacement for the single-interface
	// dns-intercept plugin.
	DNSSteering bool   `json:"dns_steering,omitempty"`
	LANAddress  string `json:"lan_address,omitempty"`
	// NAT44RolePlan, when present, asks the LAN control-plane lifecycle to
	// reconcile NAT44 roles against the live state with the same shared diff as
	// the NAT44 graph. Only the first LAN operation of a generation carries it.
	NAT44RolePlan *NAT44RolePlan `json:"nat44_role_plan,omitempty"`
}

type AddressAssignment struct {
	ID             string   `json:"id"`
	LinuxInterface string   `json:"linux_interface"`
	VPPInterface   string   `json:"vpp_interface"`
	CIDR           string   `json:"cidr"`
	Mode           string   `json:"mode,omitempty"`
	RemoveCIDRs    []string `json:"remove_cidrs,omitempty"`
	Role           string   `json:"role,omitempty"`
	BandwidthKbps  uint64   `json:"bandwidth_kbps,omitempty"`
}

type SmartQoSInterface struct {
	VPPInterface  string `json:"vpp_interface"`
	Role          string `json:"role"`
	RateKbps      uint64 `json:"rate_kbps"`
	BurstBytes    uint64 `json:"burst_bytes,omitempty"`
	HostIsolation string `json:"host_isolation"`
	Direction     string `json:"direction,omitempty"`
}

type Operation struct {
	Name           string
	RequestID      string
	Resource       string
	Payload        any
	VPPCtlCommands []string `json:"vppctl_commands,omitempty"`
	// ConfigPhase is the configuration dependency phase that owns this
	// operation (interfaces, lines, routing, proxy, policies, services).
	// WS-K writes it into operations.json so the persisted vpp-apply plan
	// shows the configuration-level order. vpp-apply ignores the field; the
	// executable order inside a phase is still its own staging logic.
	ConfigPhase string `json:"config_phase,omitempty"`
}

type Reply struct {
	Operation   string
	Retval      int32
	Done        bool
	ControlPing bool
	Payload     any
}

type Receipt struct {
	RequestID  string      `json:"request_id"`
	Operations []Operation `json:"operations"`
}

type Health struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type Snapshot struct {
	RequestID     string                      `json:"request_id"`
	TransactionID string                      `json:"transaction_id"`
	ReadbackAt    time.Time                   `json:"readback_at"`
	Interfaces    []InterfaceState            `json:"interfaces,omitempty"`
	Bonds         []BondState                 `json:"bonds,omitempty"`
	RoutePolicies []trafficpolicy.RoutePolicy `json:"route_policies,omitempty"`
	// PreNATClassifier is the previous generation's global pre-NAT header.
	// Rollback restores it verbatim so a failed policy change cannot leave the
	// classifier pointing at the wrong LAN.
	PreNATClassifier *PreNATClassifierState      `json:"pre_nat_classifier,omitempty"`
	WANGroups        []trafficpolicy.WANGroup    `json:"wan_groups,omitempty"`
	ACLs             []trafficpolicy.SecurityACL `json:"acls,omitempty"`
	QoS              []flow.VPPObjectGroup       `json:"qos,omitempty"`
	NAT              nat.CompiledConfig          `json:"nat,omitempty"`
	Hash             string                      `json:"hash"`
}

type Client interface {
	OpenChannel(context.Context) (Channel, error)
}

type Channel interface {
	Do(context.Context, Operation) (Reply, error)
	Close() error
}

type MultipartStream interface {
	Recv(context.Context) (Reply, error)
}

type Adapter struct {
	Client Client
}

type UnsupportedOperationError struct {
	Name     string
	Resource string
}

func (err *UnsupportedOperationError) Error() string {
	return fmt.Sprintf("unsupported production VPP operation %q for resource %q", err.Name, err.Resource)
}

func (err *UnsupportedOperationError) UnsupportedOperation() string { return err.Name }

type VPPError struct {
	Operation string
	RequestID string
	Retval    int32
	Err       error
}

func (e VPPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("vpp operation %s request %s failed: %v", e.Operation, e.RequestID, e.Err)
	}
	return fmt.Sprintf("vpp operation %s request %s returned retval %d", e.Operation, e.RequestID, e.Retval)
}

func (e VPPError) Unwrap() error { return e.Err }

func (a Adapter) Apply(ctx context.Context, plan Plan) (Receipt, error) {
	operations, err := BuildOperations(plan)
	if err != nil {
		return Receipt{}, err
	}
	if len(operations) == 0 {
		return Receipt{RequestID: plan.RequestID, Operations: operations}, nil
	}
	if a.Client == nil {
		return Receipt{}, VPPError{Operation: "connect", RequestID: plan.RequestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return Receipt{}, VPPError{Operation: "open_channel", RequestID: plan.RequestID, Err: err}
	}
	defer channel.Close()

	for _, operation := range operations {
		reply, err := channel.Do(ctx, operation)
		if err != nil {
			return Receipt{}, VPPError{Operation: operation.Name, RequestID: operation.RequestID, Err: err}
		}
		if err := NormalizeRetval(operation.Name, operation.RequestID, reply.Retval); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{RequestID: plan.RequestID, Operations: operations}, nil
}

func (a Adapter) ExecuteOperations(ctx context.Context, operations []Operation) error {
	if len(operations) == 0 {
		return nil
	}
	if a.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open operation channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	var operationErrors []error
	for _, operation := range operations {
		if _, err := doOperation(ctx, channel, operation); err != nil {
			operationErrors = append(operationErrors, err)
		}
	}
	return errors.Join(operationErrors...)
}

func (a Adapter) HealthCheck(context.Context) (Health, error) {
	if a.Client == nil {
		return Health{Available: false, Reason: "vpp client is not configured"}, nil
	}
	return Health{Available: true}, nil
}

func (a Adapter) Snapshot(ctx context.Context, request SnapshotRequest) (Snapshot, error) {
	return a.snapshot(ctx, request)
}

func (a Adapter) Rollback(context.Context, Snapshot) error {
	return errors.New("vpp live rollback is not implemented in this foundation slice")
}

// proxySteeringEgressIDs lists the egress IDs a compiled plan hands to the
// proxy service. The egress identity and the resolved steering instructions
// are both accepted so validation also covers a plan built from an older
// desired state.
func proxySteeringEgressIDs(gateway Plan) []string {
	ids := make([]string, 0, len(gateway.Proxy.VPPSteering)+1)
	seen := make(map[string]struct{}, len(gateway.Proxy.VPPSteering)+1)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, found := seen[id]; found {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(gateway.Proxy.ID)
	for _, steering := range gateway.Proxy.VPPSteering {
		add(steering.EgressID)
	}
	return ids
}

func BuildOperations(plan Plan) ([]Operation, error) {
	operations := []Operation{}
	flowGroups := plan.Flow.VPPGroups
	if plan.SkipQoS {
		flowGroups = WithoutQoSPluginGroups(flowGroups)
	}
	needsDataplane := plan.DNSInterception || plan.Mirror != nil || len(plan.DNSServiceNetworks) > 0 || len(plan.NativePath.Assignments) > 0 || len(plan.AddressAssignments) > 0 || len(plan.Proxy.VPPSteering) > 0 || len(flowGroups) > 0 || len(plan.Flow.Targets) > 0 || len(plan.NAT.StaticMappings) > 0 || len(plan.NAT.PortMappings) > 0 || len(plan.Policy.WANGroups) > 0 || len(plan.Policy.RoutePolicies) > 0 || len(plan.Policy.SecurityACLs) > 0 || len(plan.Security.ACLs) > 0 || len(plan.Security.MACIP) > 0 || len(plan.Security.AttackRules) > 0
	if !needsDataplane {
		return operations, nil
	}
	nativePath, err := SelectNativePath(plan.NativePath)
	if err != nil {
		var locked *DataplaneLockedError
		var afRequired *AFFallbackRequiredError
		if !plan.AllowUnprovenOperations || !(errors.As(err, &locked) || errors.As(err, &afRequired)) {
			return nil, err
		}
		// Reconciliation of a desired or persisted plan must still be able to
		// derive cleanup commands after the live tier proof expired, or when a
		// persisted pre-consent plan only carries an AF_PACKET proof. This is
		// never used for an apply: only reconciliation helpers set the flag.
		declarative := plan.NativePath
		declarative.AllowUnproven = true
		nativePath, err = SelectNativePath(declarative)
		if err != nil {
			return nil, err
		}
		plan.DataplanePrepared = true
	}
	if nativePath.Tier == DataplaneTierDPDK && !plan.DataplanePrepared {
		prerequisites := append([]PrerequisiteResult(nil), nativePath.Prerequisites...)
		prerequisites = append(prerequisites, prerequisite(
			"dpdk_runtime_adapter_ready", "", false,
			"DPDK was selected as the common tier but PCI/VFIO bind, VPP restart, readback, and rollback are not yet transactionally integrated",
		))
		return nil, &DataplaneLockedError{Prerequisites: prerequisites, Candidates: nativePath.Candidates}
	}
	attachments := make(map[string]NativeAttachment, len(nativePath.Attachments))
	attachmentsByVPP := make(map[string]NativeAttachment, len(nativePath.Attachments))
	for _, attachment := range nativePath.Attachments {
		attachments[strings.TrimSpace(attachment.LinuxInterface)] = attachment
		attachmentsByVPP[strings.TrimSpace(attachment.VPPInterface)] = attachment
	}
	bondMembers := make(map[string][]string, len(plan.Bonds))
	for _, bond := range plan.Bonds {
		bondMembers[strings.TrimSpace(bond.Name)] = bond.Members
	}
	prerequisites := append([]PrerequisiteResult(nil), nativePath.Prerequisites...)
	management := strings.TrimSpace(plan.NativePath.ManagementInterface)
	for _, assignment := range plan.AddressAssignments {
		linuxInterface := strings.TrimSpace(assignment.LinuxInterface)
		vppInterface := strings.TrimSpace(assignment.VPPInterface)
		attachment, proven := attachments[linuxInterface]
		isBond := false
		if members, ok := bondMembers[linuxInterface]; ok {
			// A bond is a synthetic interface: it has no PCI/native proof of its
			// own. It inherits the proof of every member; a member without a
			// selected native attachment keeps the assignment locked with that
			// member's name in the evidence.
			isBond = true
			proven = len(members) > 0
			for _, member := range members {
				if _, ok := attachmentsByVPP[strings.TrimSpace(member)]; !ok {
					proven = false
					break
				}
			}
		}
		vppMatches := proven && vppInterface == attachment.VPPInterface
		if isBond {
			vppMatches = proven && vppInterface == linuxInterface
		}
		prerequisites = append(prerequisites,
			prerequisite("address_assignment_management_excluded", linuxInterface, management != "" && (plan.NativePath.ManagementShared || linuxInterface != management), "address assignment references the management interface"),
			prerequisite("address_assignment_proven", linuxInterface, proven, "address assignment has no selected runtime-proven native attachment (bond inherits its members)"),
			prerequisite("address_assignment_vpp_interface_matches", linuxInterface, vppMatches, "address assignment VPP interface does not match the selected native attachment"),
		)
	}
	for _, result := range prerequisites {
		if !result.Passed {
			return nil, &DataplaneLockedError{Prerequisites: prerequisites}
		}
	}
	for _, attachment := range nativePath.Attachments {
		operations = append(operations, DataplaneAttachOperation(plan.RequestID, attachment))
	}
	if mirrorOperation, err := mirrorSpanOperation(plan, nativePath.Attachments); err != nil {
		return nil, err
	} else if mirrorOperation != nil {
		operations = append(operations, *mirrorOperation)
	}
	seenLANControlPlanes := map[string]bool{}
	// VPP keeps one local path for 255.255.255.255. Re-adding the broadcast
	// route once per LAN LCP corrupts the FIB path list and crashes
	// abf_plugin (observed with three LAN interfaces). Emit it exactly once.
	// The DHCP broadcast ABF bypass is also a per-ingress object: the first LAN
	// keeps the reserved 65000 ID, later LANs get a stable derived ID so each
	// policy is created once instead of being torn down and recreated.
	broadcastRouteEmitted := false
	bypassPoliciesEmitted := 0
	nat44RolePlanEmitted := false
	// A bond has no Linux host interface, and creating an LCP/ABF pair for it
	// segfaults VPP 26.06's abf_plugin. Bond L3 is applied by the bond lifecycle;
	// LAN services that require an LCP stay unavailable on a bond.
	bondNames := make(map[string]bool, len(plan.Bonds))
	for _, bond := range plan.Bonds {
		bondNames[strings.TrimSpace(bond.Name)] = true
	}
	for _, assignment := range plan.AddressAssignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		if bondNames[strings.TrimSpace(assignment.LinuxInterface)] {
			continue
		}
		vppInterface := strings.TrimSpace(assignment.VPPInterface)
		if vppInterface == "" || seenLANControlPlanes[vppInterface] {
			continue
		}
		seenLANControlPlanes[vppInterface] = true
		bypassPolicyID := lanDHCPBroadcastBypassPolicyID
		if bypassPoliciesEmitted > 0 {
			bypassPolicyID = stableID("lan-dhcp-bypass:"+vppInterface, lanDHCPBroadcastBypassPolicyID+1, 499)
		}
		bypassPoliciesEmitted++
		lanAddress := lanIPv4AddressForInterface(plan.AddressAssignments, vppInterface)
		controlPlane := ManagementLCP{
			Enabled:                 true,
			VPPInterface:            vppInterface,
			HostInterface:           LANControlPlaneHostInterface(assignment.LinuxInterface),
			IPv4BroadcastLocal:      !broadcastRouteEmitted,
			IPv4DHCPBroadcastBypass: true,
			DHCPBypassPolicyID:      bypassPolicyID,
			DNSSteering:             plan.DNSABFSteering,
			LANAddress:              lanAddress,
		}
		if !nat44RolePlanEmitted {
			// One role plan per generation, carrying every LAN and WAN, so a
			// later LAN operation cannot independently flip another LAN's role.
			controlPlane.NAT44RolePlan = &NAT44RolePlan{
				Behavior:          plan.NAT.Behavior,
				LANInterfaces:     lanVPPInterfacesForPlan(plan),
				OutsideInterfaces: natOutsideInterfaces(plan),
			}
			nat44RolePlanEmitted = true
		}
		broadcastRouteEmitted = true
		operations = append(operations, Operation{
			Name:           "vpp.lan-control-lcp",
			RequestID:      plan.RequestID,
			Resource:       assignment.ID,
			Payload:        controlPlane,
			VPPCtlCommands: managementLCPCommands(controlPlane),
		})
	}
	for _, assignment := range plan.AddressAssignments {
		operation := Operation{Name: "vpp.interface.address", RequestID: plan.RequestID, Resource: assignment.ID, Payload: assignment}
		operation.VPPCtlCommands = interfaceAddressCommands(assignment)
		operations = append(operations, operation)
	}
	managementLCP, lcpReady := managementLCPOperation(plan)
	if plan.NativePath.ManagementShared && !lcpReady {
		return nil, &DataplaneLockedError{Prerequisites: append(prerequisites, prerequisite("shared_management_lan_binding", management, false, "shared management requires the management interface to be the configured LAN"))}
	}
	if management != "" {
		operations = append(operations, Operation{Name: "vpp.management-lcp", RequestID: plan.RequestID, Resource: "management-network", Payload: managementLCP, VPPCtlCommands: managementLCPCommands(managementLCP)})
	}
	if plan.DNSInterception {
		interception, ok := dnsTransparentInterception(plan.AddressAssignments)
		if !ok {
			return nil, &DataplaneLockedError{Prerequisites: append(prerequisites, prerequisite("dns_lan_interface_present", "", false, "transparent DNS interception requires one configured LAN interface"))}
		}
		operations = append(operations, Operation{Name: "vpp.dns-transparent-interception", RequestID: plan.RequestID, Resource: "gateway-dns", Payload: interception, VPPCtlCommands: dnsTransparentCommands(interception)})
	}
	wanGroups := map[string]trafficpolicy.WANGroup{}
	for _, group := range plan.Policy.WANGroups {
		wanGroups[group.ID] = group
	}
	for _, group := range flowGroups {
		if len(group.Objects) == 0 {
			continue
		}
		operation := Operation{Name: group.Kind, RequestID: plan.RequestID, Resource: group.Kind, Payload: group}
		operation.VPPCtlCommands = flowGroupCommands(group)
		operations = append(operations, operation)
	}
	for _, target := range plan.Flow.Targets {
		if plan.SkipQoS && IsQoSPluginGroup(target.Kind) {
			continue
		}
		if targetOwnedByGroup(flowGroups, target) {
			continue
		}
		resource := strings.TrimSpace(target.RuleID)
		if resource == "" {
			resource = target.Kind
		}
		operations = append(operations, Operation{Name: target.Kind, RequestID: plan.RequestID, Resource: resource, Payload: target, VPPCtlCommands: flowTargetCommands(target)})
	}
	// A current LAN port needs its NAT44 role reconciled even when the plan has
	// no WAN. A port that used to be a WAN keeps its nat44 outside attachment
	// across the role change; nat44-ed-out2in then drops the DHCP broadcast
	// ("no translation") before the ABF DHCP policy or the Linux LCP sees it,
	// so the client gets no OFFER. With no outside interface nothing can be
	// translated: detach the LAN from NAT. With NAT required, always remove the
	// stale outside role before binding the LAN inside.
	if planRequiresNAT(plan) {
		initializeCommands := natInitializeCommands(plan.NAT.Behavior)
		lan := gatewayLANVPPInterface(plan)
		if plan.NAT.Behavior != nat.BehaviorFullCone && lan != "" {
			// This is the persisted boot-replay initialization, not the live
			// role reconcile. Keep it additive so a replay on an already
			// initialized VPP cannot emit `del ... failed`; the live apply uses
			// the shared state-driven nat44RoleReconcileCommands instead.
			initializeCommands = append(initializeCommands, "?set interface nat44 in "+lan)
			outputFeature := proxyUnderlayInterfaces(plan)
			for _, wan := range natOutsideInterfaces(plan) {
				if nat44InterfaceListed(outputFeature, wan) {
					// A proxy egress leaves through this WAN, so the replayed
					// role must already be the output-feature form.
					initializeCommands = append(initializeCommands, "?set interface nat44 out "+wan+" output-feature")
					continue
				}
				initializeCommands = append(initializeCommands, "?set interface nat44 out "+wan)
			}
		}
		// NAT44 requires the outside interface address in the translation
		// pool. VPP selects the outside address from the packet's TX interface
		// only when the pool address belongs to the packet's inside FIB
		// (snat_address_t.fib_index == rx_fib_index). An address added with
		// "nat44 add interface address" keeps fib_index ~0, so with more than
		// one WAN VPP ignored the routed WAN and picked a pool entry by hash
		// order. Bind static WAN addresses to the inside (default) VRF so the
		// WAN-group active member really selects its own address; keep the
		// interface form for runtime-addressed WANs (DHCP/PPPoE).
		staticPool := nat44PoolAddressMap(nat44PoolAddresses(plan.AddressAssignments))
		for _, wan := range natOutsideInterfaces(plan) {
			if plan.NAT.Behavior == nat.BehaviorFullCone {
				initializeCommands = append(initializeCommands, "?nat44 ei add interface address "+wan)
				continue
			}
			if address, ok := staticPool[wan]; ok {
				initializeCommands = append(initializeCommands,
					"?nat44 add interface address "+wan+" del",
					"?nat44 add address "+address+" tenant-vrf 0",
				)
				continue
			}
			initializeCommands = append(initializeCommands, "?nat44 add interface address "+wan)
		}
		if plan.NAT.Behavior != nat.BehaviorFullCone {
			initializeCommands = append(initializeCommands, "show nat44 addresses")
		}
		operations = append(operations, Operation{
			Name:           "vpp.nat44.initialize",
			RequestID:      plan.RequestID,
			Resource:       string(plan.NAT.Behavior),
			Payload:        plan.NAT.Behavior,
			VPPCtlCommands: initializeCommands,
		})
	}
	// VPPGroups are the executable, deduplicated representation of Targets.
	// Executing both creates the same ACL/policer twice for matched rules.
	for _, mapping := range plan.NAT.StaticMappings {
		operation := Operation{Name: "vpp.nat44-ed.static-mapping", RequestID: plan.RequestID, Resource: mapping.ID, Payload: mapping}
		operation.VPPCtlCommands = natStaticMappingCommandsForBehavior(plan.NAT.Behavior, mapping)
		operation.VPPCtlCommands = tokenizePersistedWANAddress(operation.VPPCtlCommands, mapping.WANInterface, mapping.ExternalAddress, false)
		operations = append(operations, operation)
	}
	for _, mapping := range plan.NAT.PortMappings {
		operation := Operation{Name: "vpp.nat44-ed.static-mapping", RequestID: plan.RequestID, Resource: mapping.ID, Payload: mapping}
		operation.VPPCtlCommands = natPortMappingCommandsForBehavior(plan.NAT.Behavior, mapping)
		// The durable operations file is replayed after a cold VPP start.  A
		// plain NAT44 static mapping is not sufficient: the return-path ACL,
		// ABF policy/attachment, and pre-NAT bypass are separate VPP objects and
		// are normally created by the live ApplyNAT44 adapter.  Persist them here
		// with stable identities so the standalone replay service can rebuild
		// the complete forwarding contract before status is read.
		operation.VPPCtlCommands = append(operation.VPPCtlCommands, natReturnGuardPersistentCommands(mapping, gatewayLANPrefix(plan))...)
		operation.VPPCtlCommands = tokenizePersistedWANAddress(operation.VPPCtlCommands, mapping.WANInterface, mapping.ExternalAddress, mapping.ExternalAddressExplicit)
		operations = append(operations, operation)
	}
	for _, group := range plan.Policy.WANGroups {
		operation := Operation{Name: "vpp.pbr.next-hop-group", RequestID: plan.RequestID, Resource: group.ID, Payload: group}
		operation.VPPCtlCommands = wanGroupCommands(group)
		operations = append(operations, operation)
	}
	servicePoolAddresses, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		return nil, err
	}
	for index, network := range plan.DNSServiceNetworks {
		if err := validateDNSServiceNetwork(network); err != nil {
			return nil, err
		}
		if !vppCommandTokensSafe(network.UnderlayRoute) {
			return nil, fmt.Errorf("DNS service network %q has no safe VPP underlay route", network.UpstreamID)
		}
		network.NATPoolAddress = servicePoolAddresses[index]
		operation := Operation{Name: "vpp.dns-service.network", RequestID: plan.RequestID, Resource: network.UpstreamID, Payload: network}
		operation.VPPCtlCommands = dnsServiceNetworkCommands(network, plan.NAT.Behavior)
		operations = append(operations, operation)
	}
	// A proxy service network uses the selected WAN group table as its
	// underlay, so create its VPP handoff after WAN groups are present.
	for _, steering := range plan.Proxy.VPPSteering {
		operation := Operation{Name: steering.TargetKind, RequestID: plan.RequestID, Resource: steering.EgressID, Payload: steering}
		operation.VPPCtlCommands = proxySteeringCommands(steering, plan.NAT.Behavior)
		if steering.TargetKind == "vpp.proxy-service.network" && len(operation.VPPCtlCommands) == 0 {
			return nil, fmt.Errorf("proxy egress %q has no resolved VPP underlay route for its service network", steering.EgressID)
		}
		operations = append(operations, operation)
	}
	routePolicies := orderedRoutePoliciesForVPP(plan.Policy.RoutePolicies, wanGroups)
	routeOptions := buildRoutePolicyCommandOptions(routePolicies, wanGroups)
	addRoutePolicyLocalDestinations(routeOptions, routePolicies, plan.AddressAssignments)
	if err := ValidateRoutePolicyLANBindings(routePolicies, plan.AddressAssignments); err != nil {
		return nil, err
	}
	// A 1:1 static NAT mapping rewrites the source of its inside host before
	// the steered packet can reach the proxy TAP. Refuse to compile that dead
	// combination even when the desired state reached the plan from an older
	// build that did not reject the save.
	if err := ValidateRoutePolicyStaticNAT(routePolicies, plan.NAT.StaticMappings, proxySteeringEgressIDs(plan)); err != nil {
		return nil, err
	}
	if err := validateLargeRoutePolicyFallback(routePolicies, routeOptions, wanGroups); err != nil {
		return nil, err
	}
	for _, policy := range routePolicies {
		options := routeOptions[policy.ID]
		operation := Operation{Name: "vpp.route-policy", RequestID: plan.RequestID, Resource: policy.ID, Payload: policy}
		operation.VPPCtlCommands = routePolicyCommandsWithOptions(policy, wanGroups, options)
		operations = append(operations, operation)
	}
	for _, acl := range plan.Policy.SecurityACLs {
		// The seeded catch-all WAN deny is not deployable on any interface:
		// on the LAN it locks out every client and on WAN ingress it drops
		// NAT44 return and port-forward traffic before translation. NAT44-ED
		// is stateful, so the built-in rule stays uninstalled.
		if acl.ID == builtInWANDenyACLID {
			continue
		}
		operation := Operation{Name: "vpp.security-acl", RequestID: plan.RequestID, Resource: acl.ID, Payload: acl}
		operation.VPPCtlCommands = securityACLCommands(acl)
		operations = append(operations, operation)
	}
	// The security generation owns every tagged ACL, MACIP ACL, policer and
	// attack guard. Emitting the operation even for an empty desired set lets
	// the lifecycle retire objects whose last referencing rule was deleted;
	// otherwise a removed guard keeps filtering traffic in the dataplane.
	securityCommands, err := securityGenerationCommands(plan.Security)
	if err != nil {
		return nil, err
	}
	operations = append(operations, Operation{Name: "vpp.security-generation", RequestID: plan.RequestID, Resource: securityGenerationResource(plan.Security), Payload: plan.Security, VPPCtlCommands: securityCommands})
	// Route/ACL/ABF operations attach to the configured LAN dataplane
	// interface.  The first native attachment can be the management or WAN
	// port, so deriving the ingress from attachment order silently produced an
	// unresolved `lyroute-$LY_ROUTE_LAN_INTERFACE` in production.
	lanVPPInterface := ""
	for _, assignment := range plan.AddressAssignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			lanVPPInterface = strings.TrimSpace(assignment.VPPInterface)
			if lanVPPInterface != "" {
				break
			}
		}
	}
	if lanVPPInterface == "" && len(nativePath.Attachments) > 0 {
		lanVPPInterface = nativePath.Attachments[0].VPPInterface
	}
	if lanVPPInterface != "" {
		// A route policy must be classified on the LAN that owns its source:
		// the pre-NAT plugin binds one ingress interface, and a policy for
		// 192.168.10.10/32 attached to another LAN never sees the packet.
		operations = rewriteRoutePolicyIngress(operations, plan.AddressAssignments)
		// A security ACL is attached to every LAN its selectors describe, not
		// only to the first LAN in the inventory. Expanding before the generic
		// placeholder rewrite keeps a multi-LAN rule from being collapsed onto
		// the default ingress.
		operations = expandSecurityACLInterfaces(operations, plan.AddressAssignments)
		operations = rewriteOperationsInterface(operations, lanVPPInterface)
	}
	for _, operation := range operations {
		if len(operation.VPPCtlCommands) == 0 {
			return nil, &UnsupportedOperationError{Name: operation.Name, Resource: operation.Resource}
		}
		for _, command := range operation.VPPCtlCommands {
			if commandReferencesInterface(command, management) && !plan.NativePath.ManagementShared {
				prerequisites = append(prerequisites, prerequisite("management_excluded_from_operations", management, false, "operation "+operation.Name+" references the management interface"))
			}
		}
	}
	for _, result := range prerequisites {
		if !result.Passed {
			return nil, &DataplaneLockedError{Prerequisites: prerequisites}
		}
	}
	operations = expandLANInterfaceCommands(operations, plan.LANVPPInterfaces)
	return operations, nil
}

func expandLANInterfaceCommands(operations []Operation, lans []string) []Operation {
	if len(lans) <= 1 {
		return operations
	}
	for index := range operations {
		expanded := make([]string, 0, len(operations[index].VPPCtlCommands)+len(lans))
		for _, command := range operations[index].VPPCtlCommands {
			switch {
			case strings.Contains(command, "set interface nat44 in lyroute-$LY_ROUTE_LAN_INTERFACE"):
				for _, lan := range lans {
					expanded = append(expanded, strings.ReplaceAll(command, "lyroute-$LY_ROUTE_LAN_INTERFACE", lan))
				}
			case strings.Contains(command, "abf attach ip4 policy") && strings.Contains(command, "lyroute-$LY_ROUTE_LAN_INTERFACE"):
				for _, lan := range lans {
					expanded = append(expanded, strings.ReplaceAll(command, "lyroute-$LY_ROUTE_LAN_INTERFACE", lan))
				}
			default:
				expanded = append(expanded, command)
			}
		}
		operations[index].VPPCtlCommands = expanded
	}
	return operations
}

func managementLCPOperation(plan Plan) (ManagementLCP, bool) {
	management := strings.TrimSpace(plan.NativePath.ManagementInterface)
	result := ManagementLCP{Enabled: plan.NativePath.ManagementShared, VPPInterface: "lyroute-" + management, HostInterface: "lymgmt0"}
	if !plan.NativePath.ManagementShared {
		return result, management != ""
	}
	for _, assignment := range plan.AddressAssignments {
		if strings.TrimSpace(assignment.LinuxInterface) != management || !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		if name := strings.TrimSpace(assignment.VPPInterface); name != "" {
			result.VPPInterface = name
		}
		return result, result.VPPInterface != ""
	}
	return result, false
}

func managementLCPCommands(lcp ManagementLCP) []string {
	commands := []string{"lcp lcp-sync on", "show lcp"}
	if lcp.Enabled {
		commands = append(commands, fmt.Sprintf("lcp create %s host-if %s", lcp.VPPInterface, lcp.HostInterface))
		if lcp.IPv4BroadcastLocal {
			commands = append(commands, "ip route add 255.255.255.255/32 via local", "show ip fib 255.255.255.255")
		}
		// Read the DHCP delivery leg back with the pair: the limited broadcast
		// route is only half of it, the per-port punt redirect to the pair's tap
		// is what actually hands the DISCOVER to the host service. The dynamic
		// lifecycle (doManagementLCPLifecycle) repairs a missing redirect; this
		// list is the fallback for callers without the dynamic channel.
		if lcp.IPv4DHCPBroadcastBypass {
			commands = append(commands, "show ip punt redirect")
		}
		commands = append(commands, "show lcp")
	}
	return commands
}

// LANControlPlaneHostInterface returns the deterministic Linux interface paired
// with a VPP-owned LAN. The name is short enough for Linux IFNAMSIZ and stable
// across reboots so host services such as Dnsmasq and SmartDNS can bind safely.
func LANControlPlaneHostInterface(linuxInterface string) string {
	linuxInterface = strings.TrimSpace(strings.ToLower(linuxInterface))
	var suffix strings.Builder
	lossy := false
	for _, value := range linuxInterface {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
			suffix.WriteRune(value)
			continue
		}
		if value > unicode.MaxASCII {
			lossy = true
		}
		if suffix.Len() > 0 && !strings.HasSuffix(suffix.String(), "-") {
			suffix.WriteByte('-')
		}
	}
	cleaned := strings.Trim(suffix.String(), "-")
	if !lossy && cleaned != "" && len("lylan-"+cleaned) <= 15 {
		return "lylan-" + cleaned
	}
	digest := sha256.Sum256([]byte(linuxInterface))
	return fmt.Sprintf("lylan-%x", digest[:4])
}

// lanIPv4AddressForInterface returns the IPv4 gateway of a LAN ingress. A
// dual-stack LAN has separate IPv4 and IPv6 assignments; the ABF steering
// policies are IPv4-only, so an IPv6 address must never be used as a /32 match.
func lanIPv4AddressForInterface(assignments []AddressAssignment, vppInterface string) string {
	vppInterface = strings.TrimSpace(vppInterface)
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") || strings.TrimSpace(assignment.VPPInterface) != vppInterface {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if err == nil && prefix.Addr().Is4() {
			return prefix.Addr().String()
		}
	}
	return ""
}

func dnsTransparentInterception(assignments []AddressAssignment) (DNSTransparentInterception, bool) {
	result := DNSTransparentInterception{}
	seenPrefixes := map[string]bool{}
	seenLANs := map[string]bool{}
	for _, assignment := range assignments {
		if strings.ToLower(strings.TrimSpace(assignment.Role)) != "lan" {
			continue
		}
		name := strings.TrimSpace(assignment.VPPInterface)
		if name == "" {
			name = nativeLANInterface(strings.TrimSpace(assignment.LinuxInterface))
		}
		if name == "" {
			continue
		}
		if result.LANInterface == "" {
			result.LANInterface = name
		}
		if !seenLANs[name] {
			seenLANs[name] = true
			result.LANInterfaces = append(result.LANInterfaces, name)
		}
		cidr := strings.TrimSpace(assignment.CIDR)
		if cidr == "" || seenPrefixes[cidr] {
			continue
		}
		seenPrefixes[cidr] = true
		if strings.Contains(cidr, ":") {
			result.IPv6Prefixes = append(result.IPv6Prefixes, cidr)
		} else {
			result.IPv4Prefixes = append(result.IPv4Prefixes, cidr)
			result.IPv4Routes = append(result.IPv4Routes, DNSInterceptRoute{Prefix: cidr, Interface: name})
		}
	}
	return result, len(result.LANInterfaces) > 0
}

func dnsTransparentCommands(interception DNSTransparentInterception) []string {
	commands := []string{
		"?set ly-route dns-intercept disable",
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", dnsIPv4TableID),
		fmt.Sprintf("?ip table add %d", dnsIPv4TableID),
		fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via local", dnsIPv4TableID),
	}
	routes := interception.IPv4Routes
	if len(routes) == 0 {
		// Plans persisted before per-prefix routing only carried the prefix
		// list; keep them reconcilable with the previous single-LAN shape.
		for _, prefix := range interception.IPv4Prefixes {
			routes = append(routes, DNSInterceptRoute{Prefix: prefix, Interface: interception.LANInterface})
		}
	}
	for _, route := range routes {
		routeInterface := strings.TrimSpace(route.Interface)
		if routeInterface == "" {
			routeInterface = interception.LANInterface
		}
		commands = append(commands, fmt.Sprintf("?ip route add table %d %s via %s", dnsIPv4TableID, route.Prefix, routeInterface))
	}
	interfaces := interception.LANInterfaces
	if len(interfaces) == 0 && strings.TrimSpace(interception.LANInterface) != "" {
		interfaces = []string{interception.LANInterface}
	}
	for _, name := range interfaces {
		commands = append(commands, fmt.Sprintf("set ly-route dns-intercept interface %s table %d", name, dnsIPv4TableID))
	}
	return append(commands,
		"show ly-route dns-intercept",
		fmt.Sprintf("show ip fib table %d", dnsIPv4TableID))
}

func commandReferencesInterface(command, interfaceName string) bool {
	if interfaceName == "" {
		return false
	}
	for _, field := range strings.Fields(command) {
		token := strings.Trim(field, "?[](),;")
		if token == interfaceName || token == "lyroute-"+interfaceName || token == "host-"+interfaceName {
			return true
		}
	}
	return false
}

func rewriteOperationsInterface(operations []Operation, vppInterface string) []Operation {
	if strings.TrimSpace(vppInterface) == "" {
		return operations
	}
	for index := range operations {
		for commandIndex, command := range operations[index].VPPCtlCommands {
			command = strings.ReplaceAll(command, "lyroute-$LY_ROUTE_LAN_INTERFACE", vppInterface)
			command = strings.ReplaceAll(command, "host-$LY_ROUTE_LAN_INTERFACE", vppInterface)
			operations[index].VPPCtlCommands[commandIndex] = command
		}
	}
	return operations
}

// expandSecurityACLInterfaces rewrites each standalone ACL attachment into one
// command per LAN that the rule describes. The generic LAN placeholder rewrite
// would otherwise collapse every ACL onto the first LAN, which silently makes
// a rule for another LAN a no-op.
func expandSecurityACLInterfaces(operations []Operation, assignments []AddressAssignment) []Operation {
	lans := securityLANBindings(assignments)
	if len(lans) == 0 {
		return operations
	}
	for index := range operations {
		if operations[index].Name != "vpp.security-acl" {
			continue
		}
		acl, ok := operations[index].Payload.(trafficpolicy.SecurityACL)
		if !ok {
			continue
		}
		expanded := make([]string, 0, len(operations[index].VPPCtlCommands)+len(lans))
		shown := make(map[string]struct{}, len(lans))
		for _, command := range operations[index].VPPCtlCommands {
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(command, "?")))
			if len(fields) >= 6 && fields[0] == "set" && fields[1] == "interface" && fields[3] == "acl" && fields[4] == "intfc" && fields[5] == "lyroute-$LY_ROUTE_LAN_INTERFACE" {
				direction := fields[2]
				for _, interfaceName := range securityACLLANInterfaces(acl.Match, direction, lans, "") {
					expanded = append(expanded, strings.ReplaceAll(command, "lyroute-$LY_ROUTE_LAN_INTERFACE", interfaceName))
				}
				continue
			}
			if strings.TrimSpace(command) == "show interface lyroute-$LY_ROUTE_LAN_INTERFACE" {
				for _, interfaceName := range securityAllLANInterfaces(lans) {
					if _, duplicate := shown[interfaceName]; duplicate {
						continue
					}
					shown[interfaceName] = struct{}{}
					expanded = append(expanded, "show interface "+interfaceName)
				}
				continue
			}
			expanded = append(expanded, command)
		}
		operations[index].VPPCtlCommands = expanded
	}
	return operations
}

func interfaceAddressCommands(assignment AddressAssignment) []string {
	name := strings.TrimSpace(assignment.VPPInterface)
	if name == "" {
		name = nativeLANInterface(strings.TrimSpace(assignment.LinuxInterface))
	}
	if strings.ToLower(strings.TrimSpace(assignment.Mode)) == "dhcp4" {
		if name == "" {
			return nil
		}
		commands := []string{fmt.Sprintf("set interface state %s up", name)}
		for _, cidr := range assignment.RemoveCIDRs {
			cidr = strings.TrimSpace(cidr)
			if cidr != "" {
				commands = append(commands, fmt.Sprintf("?set interface ip address del %s %s", name, cidr))
			}
		}
		commands = append(commands,
			fmt.Sprintf("?set dhcp client del intfc %s", name),
			fmt.Sprintf("set dhcp client intfc %s", name),
			"show dhcp client",
			fmt.Sprintf("show interface address %s", name),
		)
		return commands
	}
	cidr := strings.TrimSpace(assignment.CIDR)
	if name == "" || cidr == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("set interface state %s up", name),
		fmt.Sprintf("?set interface ip address %s %s", name, cidr),
		fmt.Sprintf("show interface address %s", name),
	}
}

// mirrorSpanOperation renders the disable-first SPAN generation. It is emitted
// for every plan that owns at least one VPP attachment: an empty desired mirror
// must actively clear a session left behind by the previous generation, which
// is also what makes "remove the mirror port" converge instead of leaving the
// old session in VPP.
func mirrorSpanOperation(plan Plan, attachments []NativeAttachment) (*Operation, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	takenOver := make(map[string]struct{}, len(attachments))
	cleanup := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		name := strings.TrimSpace(attachment.VPPInterface)
		if name == "" {
			continue
		}
		takenOver[name] = struct{}{}
		cleanup = append(cleanup, name)
	}
	payload := MirrorConfig{}
	if plan.Mirror != nil {
		payload = *plan.Mirror
		destination := strings.TrimSpace(payload.Destination)
		if destination == "" {
			return nil, fmt.Errorf("mirror generation has no destination interface")
		}
		if _, ok := takenOver[destination]; !ok {
			return nil, fmt.Errorf("mirror port %s is not taken over by the dataplane", nonEmptyMirrorName(payload.LinuxInterface, destination))
		}
		for _, source := range payload.Sources {
			if _, ok := takenOver[strings.TrimSpace(source.Interface)]; !ok {
				return nil, fmt.Errorf("mirror source %s is not taken over by the dataplane", nonEmptyMirrorName(source.LinuxInterface, source.Interface))
			}
		}
	}
	resource := "mirror"
	if payload.LinuxInterface != "" {
		resource = payload.LinuxInterface
	}
	return &Operation{
		Name:           "vpp.interface-span",
		RequestID:      plan.RequestID,
		Resource:       resource,
		Payload:        payload,
		VPPCtlCommands: MirrorSpanCommands(plan.Mirror, cleanup),
	}, nil
}

func nonEmptyMirrorName(preferred, fallback string) string {
	if strings.TrimSpace(preferred) != "" {
		return preferred
	}
	return fallback
}

func DataplaneAttachOperation(requestID string, attachment NativeAttachment) Operation {
	if attachment.Tier == DataplaneTierDPDK {
		commands := []string{}
		hardware := strings.TrimSpace(attachment.HardwareVPPInterface)
		product := strings.TrimSpace(attachment.VPPInterface)
		if hardware != "" && product != "" && hardware != product {
			// VPP names a DPDK port after the PMD/PCI path
			// (GigabitEthernet0/3/0). Rename it once to the product contract
			// so routes, FIB, NAT, status and the interface API all address
			// the same name. The rename is optional so a replay after the
			// old name disappeared still succeeds; the hard state/readback
			// commands below prove the product name exists.
			commands = append(commands, fmt.Sprintf("?set interface name %s %s", hardware, product))
		}
		commands = append(commands,
			// VPP restarts recreate DPDK devices with admin state down.
			// Explicitly bring every DPDK port back up so a
			// reboot/replay does not silently lose the WAN carrier.
			fmt.Sprintf("set interface state %s up", attachment.VPPInterface),
			fmt.Sprintf("show hardware-interfaces %s", attachment.VPPInterface),
			fmt.Sprintf("show interface %s", attachment.VPPInterface),
		)
		return Operation{
			Name:           "vpp.dataplane.attach",
			RequestID:      requestID,
			Resource:       attachment.LinuxInterface,
			Payload:        attachment,
			VPPCtlCommands: commands,
		}
	}
	commands := []string(nil)
	switch {
	case attachment.Hook == NativeHookAFPacket && attachment.Mode == NativeModeAFPacket:
		commands = []string{
			// A port has exactly one dataplane owner: CleanupDuplicateDataplaneTAPs
			// removes any installer TAP that still carries this product name
			// before the apply snapshot, so the rename below can claim it.
			// AF_PACKET is an acceptance-only fallback. Its virtual path does not
			// reliably complete checksum/GSO offloads, so packets must leave VPP
			// with their checksums already materialized.
			fmt.Sprintf("?create host-interface name %s cksum-gso-disable", attachment.LinuxInterface),
			fmt.Sprintf("?set interface name host-%s %s", attachment.LinuxInterface, attachment.VPPInterface),
		}
		// VPP assigns a locally administered address to AF_PACKET host interfaces.
		// Linux peers learn the physical NIC address, so their frames otherwise fail
		// VPP's L2 destination check. The probe persists that address in the
		// attachment, allowing replay to restore both LAN and WAN deterministically.
		if strings.TrimSpace(attachment.MACAddress) != "" {
			commands = append(commands, fmt.Sprintf("set interface mac address %s %s", attachment.VPPInterface, attachment.MACAddress))
		}
		commands = append(commands,
			fmt.Sprintf("set interface state %s up", attachment.VPPInterface),
			fmt.Sprintf("show hardware-interfaces %s", attachment.VPPInterface),
			fmt.Sprintf("show interface %s", attachment.VPPInterface),
		)
	case attachment.Hook == NativeHookTapBridge && attachment.Mode == NativeModeTapBridge:
		bridge, hostInterface := tapBridgeInterfaceNames(attachment.LinuxInterface)
		commands = []string{
			// A duplicate TAP with this product name is removed by
			// CleanupDuplicateDataplaneTAPs before the apply runs; deleting it
			// here instead would drop the live bridge TAP on every replay.
			fmt.Sprintf("?create tap if-name %s host-if-name %s host-bridge %s no-gso", attachment.VPPInterface, hostInterface, bridge),
			fmt.Sprintf("set interface state %s up", attachment.VPPInterface),
			fmt.Sprintf("show hardware-interfaces %s", attachment.VPPInterface),
			fmt.Sprintf("show interface %s", attachment.VPPInterface),
		}
	case attachment.Hook == NativeHookAFXDP && (attachment.Mode == NativeModeZeroCopy || attachment.Mode == NativeModeCopy):
		modeFlag := "zero-copy"
		if attachment.Mode == NativeModeCopy {
			modeFlag = "no-zero-copy"
		}
		commands = []string{
			fmt.Sprintf("?create interface af_xdp host-if %s name %s rx-queue-size 512 tx-queue-size 512 num-rx-queues all %s mac-reuse", attachment.LinuxInterface, attachment.VPPInterface, modeFlag),
			fmt.Sprintf("set interface state %s up", attachment.VPPInterface),
			fmt.Sprintf("show hardware-interfaces %s", attachment.VPPInterface),
			fmt.Sprintf("show interface %s", attachment.VPPInterface),
		}
	case attachment.Hook == NativeHookRDMA && attachment.Mode == NativeModeRDMADV:
		commands = []string{
			fmt.Sprintf("?create interface rdma host-if %s name %s mode dv", attachment.LinuxInterface, attachment.VPPInterface),
			fmt.Sprintf("set interface state %s up", attachment.VPPInterface),
			fmt.Sprintf("show hardware-interfaces %s", attachment.VPPInterface),
			fmt.Sprintf("show interface %s", attachment.VPPInterface),
		}
	}
	return Operation{
		Name:           "vpp.dataplane.attach",
		RequestID:      requestID,
		Resource:       attachment.LinuxInterface,
		Payload:        attachment,
		VPPCtlCommands: commands,
	}
}

// proxyServiceNetworkTAPCommands is the idempotent TAP/address block that makes
// the two proxy service peers exist. It is shared by plan generation and by the
// execution-time repair that runs before a readback, so a replayed plan can
// never show an interface the same operation has not already created.
func proxyServiceNetworkTAPCommands(network proxy.ProxyServiceNetwork) []string {
	return []string{
		fmt.Sprintf("?create tap id %d host-if-name %s no-gso", network.IngressTapID, network.IngressHostInterface),
		fmt.Sprintf("?set interface name tap%d %s", network.IngressTapID, network.IngressVPPInterface),
		fmt.Sprintf("?set interface state %s up", network.IngressVPPInterface),
		fmt.Sprintf("?set interface mtu packet %d %s", network.MTU, network.IngressVPPInterface),
		fmt.Sprintf("?set interface ip address %s %s", network.IngressVPPInterface, network.IngressCIDR),
		fmt.Sprintf("?create tap id %d host-if-name %s no-gso", network.EgressTapID, network.EgressHostInterface),
		fmt.Sprintf("?set interface name tap%d %s", network.EgressTapID, network.EgressVPPInterface),
		fmt.Sprintf("?set interface state %s up", network.EgressVPPInterface),
		fmt.Sprintf("?set interface mtu packet %d %s", network.MTU, network.EgressVPPInterface),
	}
}

func proxySteeringCommands(steering proxy.VPPSteeringInstruction, behavior nat.Behavior) []string {
	resource := steering.EgressID
	if strings.TrimSpace(resource) == "" {
		resource = string(steering.Handoff)
	}
	if steering.TargetKind == "vpp.proxy-service.network" {
		network := steering.ServiceNetwork
		if strings.TrimSpace(network.EgressID) == "" {
			network = proxy.ServiceNetworkForEgressID(resource)
		}
		underlay := strings.TrimSpace(steering.UnderlayRoute)
		if underlay == "" {
			underlay = strings.TrimSpace(network.UnderlayRoute)
		}
		underlayInterface := proxyUnderlayInterface(underlay)
		legacyUnbound := strings.TrimSpace(steering.UnderlayWANID) == "" && (underlay == "" || underlay == "local")
		if !legacyUnbound && (underlay == "" || underlay == "local" || underlayInterface == "") {
			// A proxy handoff with a configured underlay WAN but no resolved
			// route would install TAPs whose traffic has no VPP route, then
			// report committed while Xray silently cannot dial its node.
			return nil
		}
		if underlay == "local" {
			underlay = ""
		}
		tableID := network.OutboundTableID
		tenantTable := behavior != nat.BehaviorFullCone && tableID > 0 && strings.TrimSpace(network.NATPoolAddress) != ""
		routeTable := tableID > 0 && underlay != ""
		commands := proxyServiceNetworkTAPCommands(network)
		if routeTable {
			// Install the underlay WAN as an explicit proxy-egress route. When a
			// tenant pool also exists, the egress TAP is moved into this FIB so
			// traffic cannot fall back to a lucky main-FIB default.
			commands = append(commands, fmt.Sprintf("?ip table add %d", tableID))
		}
		if tenantTable {
			commands = append(commands, fmt.Sprintf("?set interface ip table %s %d", network.EgressVPPInterface, tableID))
		}
		commands = append(commands, fmt.Sprintf("?set interface ip address %s %s", network.EgressVPPInterface, network.EgressCIDR))
		if routeTable {
			commands = append(commands,
				fmt.Sprintf("?ip route del table %d 0.0.0.0/0", tableID),
				fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via %s", tableID, underlay),
				// Return path: WAN replies are translated back to the Linux
				// egress peer; the ingress peer is the TPROXY handoff. Both are
				// explicit routes in the same proxy FIB.
				fmt.Sprintf("?ip route add table %d %s/32 via %s", tableID, network.EgressHostAddress, network.EgressVPPInterface),
				fmt.Sprintf("?ip route add table %d %s/32 via %s", tableID, network.IngressHostAddress, network.IngressVPPInterface),
			)
			for _, lanRoute := range network.LANRoutes {
				lanRoute = strings.TrimSpace(lanRoute)
				if lanRoute == "" {
					continue
				}
				commands = append(commands, fmt.Sprintf("?ip route add table %d %s via %s", tableID, lanRoute, network.IngressVPPInterface))
			}
		}
		if behavior == nat.BehaviorFullCone {
			commands = append(commands, natInitializeCommands(behavior)...)
			commands = append(commands,
				fmt.Sprintf("?set interface nat44 ei in %s out %s output-feature del", network.EgressVPPInterface, underlayInterface),
				fmt.Sprintf("?set interface nat44 ei in %s out %s", network.EgressVPPInterface, underlayInterface),
				fmt.Sprintf("?nat44 ei add interface address %s", underlayInterface),
			)
		} else if tenantTable {
			commands = append(commands, natInitializeCommands(behavior)...)
			commands = append(commands,
				nat44PoolAddCommand(behavior, network.NATPoolAddress, tableID),
				// A tenant proxy FIB cannot use the in/out feature pair: the
				// inside FIB is 35554 while the WAN outside interface (and the
				// return packet) is in FIB 0, so the out2in session lookup
				// never matches. Output-feature runs after the tenant FIB has
				// routed the packet to the WAN; NAT chooses the tenant pool
				// from the packet's ingress FIB (2) and stores the outside FIB
				// as 0, which is exactly where replies arrive.
				fmt.Sprintf("?set interface nat44 in %s del", network.EgressVPPInterface),
				fmt.Sprintf("?set interface nat44 in %s output-feature del", network.EgressVPPInterface),
				fmt.Sprintf("?set interface nat44 out %s del", network.EgressVPPInterface),
				fmt.Sprintf("?set interface nat44 out %s output-feature del", network.EgressVPPInterface),
				// VPP 26.06 rejects `out <wan> output-feature` while the WAN
				// already owns the plain outside role (the CLI answers
				// "interface already configured" and exits 0), so the role
				// is replaced here. The live apply path additionally proves
				// the resulting role from `show nat44 interfaces`.
				fmt.Sprintf("?set interface nat44 out %s output-feature del", underlayInterface),
				fmt.Sprintf("?set interface nat44 out %s del", underlayInterface),
				fmt.Sprintf("set interface nat44 out %s output-feature", underlayInterface),
			)
		} else {
			commands = append(commands,
				fmt.Sprintf("?set interface nat44 in %s del", network.EgressVPPInterface),
			)
		}
		fibCommand := "show ip fib table 0"
		if tenantTable {
			fibCommand = fmt.Sprintf("show ip fib table %d", tableID)
		}
		commands = append(commands,
			fmt.Sprintf("show interface address %s", network.IngressVPPInterface),
			fmt.Sprintf("show interface address %s", network.EgressVPPInterface),
			fibCommand,
		)
		if behavior == nat.BehaviorFullCone {
			commands = append(commands, "show nat44 ei interfaces", "show nat44 ei addresses")
		} else {
			commands = append(commands, "show nat44 interfaces")
			if tenantTable {
				commands = append(commands, "show nat44 addresses")
			}
		}
		return append(commands,
			"show tap",
		)
	}
	interfaceName := "lyroute-$LY_ROUTE_LAN_INTERFACE"
	policyID := stableID("abf:"+resource, 1000, 8999)
	aclID := stableID("acl:"+resource, 1000, 8999)
	tableID := stableID("pbr:"+resource, 10000, 49999)
	priority := steering.Order
	if priority <= 0 {
		priority = 100
	}

	switch steering.TargetKind {
	case "vpp.abf.policy":
		return []string{
			fmt.Sprintf("?set acl-plugin acl index %d permit src 0.0.0.0/0 dst 0.0.0.0/0 tag ly-route-%s", aclID, safeTag(resource)),
			fmt.Sprintf("?abf policy add id %d acl %d via local", policyID, aclID),
			fmt.Sprintf("?abf attach ip4 policy %d priority %d %s", policyID, priority, interfaceName),
			fmt.Sprintf("show acl-plugin acl index %d", aclID),
			fmt.Sprintf("show abf policy %d", policyID),
			fmt.Sprintf("show abf attach %s", interfaceName),
		}
	case "vpp.pbr.policy":
		return []string{
			fmt.Sprintf("?ip table add %d", tableID),
			fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via local", tableID),
			fmt.Sprintf("?set ip flow-hash table %d src dst sport dport proto", tableID),
			fmt.Sprintf("show ip table %d", tableID),
			fmt.Sprintf("show ip fib table %d", tableID),
		}
	case "vpp.service-chain.egress-binding":
		return []string{
			fmt.Sprintf("show interface %s", interfaceName),
			fmt.Sprintf("show abf attach %s", interfaceName),
			fmt.Sprintf("show ip table %d", tableID),
		}
	default:
		return nil
	}
}

// proxyUnderlayInterface extracts the VPP interface from a resolved underlay
// path ("<next-hop> <interface>", a PPPoE token, or a WAN-group table lookup).
// It is only used to attach the NAT44 outside role; the route itself keeps the
// complete path so a WAN-group lookup is preserved.
func proxyUnderlayInterface(underlay string) string {
	fields := strings.Fields(strings.TrimSpace(underlay))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// proxyUnderlayInterfaces returns the VPP interfaces that carry proxy egress
// traffic. A proxy egress terminates on a service-network TAP and leaves the
// gateway through the selected underlay WAN, so that WAN keeps the NAT44
// output-feature role for as long as the egress exists. The NAT44 role
// reconciler consumes this list, which is what stops a later NAT44 graph pass
// from "normalizing" the role back to a plain outside attachment.
func proxyUnderlayInterfaces(plan Plan) []string {
	if len(plan.Proxy.VPPSteering) == 0 {
		return nil
	}
	result := make([]string, 0, len(plan.Proxy.VPPSteering))
	seen := make(map[string]struct{}, len(plan.Proxy.VPPSteering))
	for _, steering := range plan.Proxy.VPPSteering {
		if steering.TargetKind != "vpp.proxy-service.network" {
			continue
		}
		underlay := strings.TrimSpace(steering.UnderlayRoute)
		if underlay == "" {
			underlay = strings.TrimSpace(steering.ServiceNetwork.UnderlayRoute)
		}
		if underlay == "" || underlay == "local" {
			continue
		}
		name := proxyUnderlayInterface(underlay)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

// dnsServiceNetworkTAPCommands creates (or repairs) the VPP side of the DNS
// service handoff. The commands are idempotent and every mutation is optional:
// the caller must prove the TAP and the underlay exist before it issues a
// NAT44 role, and reports which side is missing when they do not.
func dnsServiceNetworkTAPCommands(network DNSServiceNetwork) []string {
	underlayPath := routePathVia("", "", network.UnderlayRoute)
	return []string{
		fmt.Sprintf("?create tap id %d host-if-name %s no-gso", network.TapID, network.HostInterface),
		fmt.Sprintf("?set interface name tap%d %s", network.TapID, network.VPPInterface),
		fmt.Sprintf("?set interface state %s up", network.VPPInterface),
		fmt.Sprintf("?set interface mtu packet %d %s", network.MTU, network.VPPInterface),
		fmt.Sprintf("?ip table add %d", network.TableID),
		fmt.Sprintf("?set interface ip table %s %d", network.VPPInterface, network.TableID),
		fmt.Sprintf("?set interface ip address %s %s", network.VPPInterface, network.CIDR),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", network.TableID),
		fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via %s", network.TableID, underlayPath),
		// Optional: a missing TAP must reach the semantic repair/error path in
		// ensureDNSServiceNetworkNAT instead of failing on a readback command.
		fmt.Sprintf("?show interface address %s", network.VPPInterface),
		fmt.Sprintf("?show ip fib table %d", network.TableID),
		"?show tap",
	}
}

func dnsServiceNetworkCommands(network DNSServiceNetwork, behavior nat.Behavior) []string {
	underlayInterface := dnsServiceNetworkUnderlayInterface(network)
	commands := dnsServiceNetworkTAPCommands(network)
	// The DNS handoff owns its NAT44 role, so it must also ensure the NAT44
	// plugin is enabled before the role command.  On a clean boot the
	// service-networks node runs before the nat44 node; without this the
	// interface role command is accepted-but-ignored while the plugin is
	// disabled, and the graph's role verification fails closed.
	commands = append(commands, natInitializeCommands(behavior)...)
	// NAT44 is tenant-scoped in VPP: the inside TAP lives in its own table, so
	// the outside address must be added to that same tenant VRF. The global
	// LAN->WAN pool is a different VRF and does not make this handoff
	// translatable. The command is optional because the address may already be
	// present on a repeat apply; ensureDNSServiceNetworkNAT reconciles the exact
	// entry and the readback fails closed if it is missing.
	if pool := strings.TrimSpace(network.NATPoolAddress); pool != "" {
		commands = append(commands, nat44PoolAddCommand(behavior, pool, network.TableID))
	}
	// The role is optional here on purpose: ApplySupplemental calls
	// ensureDNSServiceNetworkNAT immediately afterwards, which proves both
	// interfaces exist, retries the role and reports a precise missing side.
	// Making the raw command required would surface only VPP's generic
	// "interface couldn't be found" text.
	if behavior == nat.BehaviorFullCone {
		return append(commands,
			fmt.Sprintf("?set interface nat44 ei in %s out %s output-feature del", network.VPPInterface, underlayInterface),
			fmt.Sprintf("?set interface nat44 ei in %s out %s", network.VPPInterface, underlayInterface),
			fmt.Sprintf("?nat44 ei add interface address %s", underlayInterface),
			"show nat44 ei interfaces",
			"show nat44 ei addresses",
		)
	}
	return append(commands,
		fmt.Sprintf("?set interface nat44 in %s out %s output-feature del", network.VPPInterface, underlayInterface),
		fmt.Sprintf("?set interface nat44 in %s out %s", network.VPPInterface, underlayInterface),
		"show nat44 interfaces",
		"show nat44 addresses",
	)
}

func dnsServiceNetworkUnderlayInterface(network DNSServiceNetwork) string {
	fields := strings.Fields(network.UnderlayRoute)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func flowGroupCommands(group flow.VPPObjectGroup) []string {
	if len(group.Objects) == 0 {
		return nil
	}
	commands := make([]string, 0, len(group.Objects)*2)
	seen := map[string]struct{}{}
	for _, object := range group.Objects {
		target := flow.Target{Kind: group.Kind, RuleID: object.RuleID, Granularity: object.Granularity, Action: object.Action, Class: object.Class, DSCP: object.DSCP, RemarkBehavior: object.RemarkBehavior, Policer: object.Policer, Shaping: object.Shaping, Match: object.Match, Attachments: object.Attachments}
		for _, command := range flowTargetCommands(target) {
			if _, ok := seen[command]; ok {
				continue
			}
			seen[command] = struct{}{}
			commands = append(commands, command)
		}
	}
	return commands
}

func flowTargetCommands(target flow.Target) []string {
	interfaceName := "lyroute-$LY_ROUTE_LAN_INTERFACE"
	resource := target.RuleID
	if strings.TrimSpace(resource) == "" {
		resource = target.Kind
	}
	mapID := stableID("qos-map:"+resource, 1, 999)
	policerName := "ly_route_" + safeTag(resource)
	qosValue := qosClassValue(target)
	dscp := dscpValue(target.DSCP)
	policerRate, policerBurst := policerValues(target)

	switch target.Kind {
	case "vpp.acl.drop":
		aclID := stableID("flow-acl-drop:"+resource, 10000, 49999)
		commands := aclMatchCommands(aclID, resource, policyMatch(target.Match), "deny")
		for _, attachment := range target.Attachments {
			commands = append(commands, flowAttachACLCommand(aclID, attachment))
		}
		commands = append(commands, fmt.Sprintf("show acl-plugin acl index %d", aclID))
		return commands
	case "vpp.behavior.rate":
		commands := []string{}
		commands = append(commands, flowRateRuleCommands(target)...)
		if len(commands) == 0 {
			// A policy can define a rate/policer without an explicit ACL
			// attachment; keep a concrete policer create command so the
			// replacement lifecycle has something to apply.
			commands = append(commands, fmt.Sprintf("?policer add name %s type 1r2c cir %d cb %d rate kbps conform-action transmit exceed-action drop violate-action drop", policerName, policerRate, policerBurst))
		}
		commands = append(commands, "show ly-route flow-rate")
		return commands
	case "vpp.qos.classify":
		return []string{
			fmt.Sprintf("qos record ip %s", interfaceName),
			fmt.Sprintf("qos store ip %s value %d", interfaceName, qosValue),
			fmt.Sprintf("show qos record %s", interfaceName),
			fmt.Sprintf("show qos store %s", interfaceName),
		}
	case "vpp.qos.record":
		return []string{
			fmt.Sprintf("qos record ip %s", interfaceName),
			fmt.Sprintf("show qos record %s", interfaceName),
		}
	case "vpp.qos.store":
		return []string{
			fmt.Sprintf("qos store ip %s value %d", interfaceName, qosValue),
			fmt.Sprintf("show qos store %s", interfaceName),
		}
	case "vpp.qos.egress-map":
		return []string{
			fmt.Sprintf("qos egress map id %d [ip][%d]=%d", mapID, qosValue, dscp),
			fmt.Sprintf("show qos egress map id %d", mapID),
		}
	case "vpp.qos.mark":
		return []string{
			fmt.Sprintf("qos egress map id %d [ip][%d]=%d", mapID, qosValue, dscp),
			fmt.Sprintf("qos mark ip %s id %d", interfaceName, mapID),
			fmt.Sprintf("show qos egress map id %d", mapID),
			fmt.Sprintf("show qos mark %s", interfaceName),
		}
	case "vpp.policer":
		return []string{
			fmt.Sprintf("?policer add name %s type 1r2c cir %d cb %d rate kbps conform-action transmit exceed-action drop violate-action drop", policerName, policerRate, policerBurst),
			fmt.Sprintf("?policer input name %s %s", policerName, interfaceName),
			fmt.Sprintf("show policer name %s", policerName),
		}
	case "vpp.shape":
		return flowShapeCommands(target)
	default:
		return nil
	}
}

// flowShapeCommands compiles an interface-level fq-codel shaper. The plugin
// exposes one egress shaper per interface, so a match-based rule is rejected
// during intent validation instead of silently shaping the whole interface.
func flowShapeCommands(target flow.Target) []string {
	if target.Shaping == nil || len(target.Attachments) == 0 {
		return nil
	}
	shaping := *target.Shaping
	commands := make([]string, 0, len(target.Attachments)+1)
	seen := map[string]struct{}{}
	for _, attachment := range target.Attachments {
		interfaceName := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(attachment), "input:"), "output:")
		if interfaceName == "" {
			continue
		}
		rateKbps := (shaping.RateBPS + 999) / 1000
		command := fmt.Sprintf("set ly-route smart-qos interface %s rate %d", interfaceName, rateKbps)
		if shaping.BurstBPS != 0 {
			command += fmt.Sprintf(" burst-bytes %d", shaping.BurstBytes())
		}
		isolation := shaping.HostIsolation
		if isolation == "" {
			role := "lan"
			if shaping.Direction == "uplink" || strings.Contains(attachment, "LY_ROUTE_WAN_INTERFACE") {
				role = "wan"
			}
			isolation = shaping.InterfaceHostIsolation(role)
		}
		if isolation == flow.HostIsolationAuto || isolation == flow.HostIsolationOff {
			isolation = flow.HostIsolationDestination
		}
		command += " host-isolation " + string(isolation)
		if _, duplicate := seen[command]; duplicate {
			continue
		}
		seen[command] = struct{}{}
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return nil
	}
	return append(commands, "show ly-route smart-qos")
}

func flowACLMatch(match flow.Match) string {
	return fmt.Sprintf("src %s dst %s proto %s sport %s dport %s", firstOrAny(match.Sources), firstOrAny(match.Destinations), firstOrAny(match.Protocols), portRange(firstOrAny(match.SourcePorts)), portRange(firstOrAny(match.DestPorts)))
}

func flowAttachACLCommand(aclID int, attachment string) string {
	if strings.HasPrefix(attachment, "output:") {
		return fmt.Sprintf("?set acl-plugin interface %s output acl %d", nativeLANInterface(strings.TrimPrefix(attachment, "output:")), aclID)
	}
	return fmt.Sprintf("?set acl-plugin interface %s input acl %d", nativeLANInterface(strings.TrimPrefix(attachment, "input:")), aclID)
}

func flowAttachPolicerCommand(policerName, attachment string) string {
	if strings.HasPrefix(attachment, "output:") {
		return fmt.Sprintf("?policer output name %s %s", policerName, nativeLANInterface(strings.TrimPrefix(attachment, "output:")))
	}
	return fmt.Sprintf("?policer input name %s %s", policerName, nativeLANInterface(strings.TrimPrefix(attachment, "input:")))
}

func flowRateRuleCommands(target flow.Target) []string {
	commands := make([]string, 0)
	clause := 0
	rateKbps, burstBytes := policerValues(target)
	for _, attachment := range target.Attachments {
		direction := flowAttachmentDirection(attachment)
		interfaceName := flowAttachmentInterface(attachment)
		for _, source := range nonEmptyList(target.Match.Sources, "0.0.0.0/0") {
			for _, destination := range nonEmptyList(target.Match.Destinations, "0.0.0.0/0") {
				for _, protocol := range nonEmptyList(target.Match.Protocols, "any") {
					for _, sourcePort := range nonEmptyList(target.Match.SourcePorts, "any") {
						for _, destinationPort := range nonEmptyList(target.Match.DestPorts, "any") {
							clauseSource, clauseDestination := source, destination
							clauseSourcePort, clauseDestinationPort := sourcePort, destinationPort
							if direction == "output" {
								clauseSource, clauseDestination = clauseDestination, clauseSource
								clauseSourcePort, clauseDestinationPort = clauseDestinationPort, clauseSourcePort
							}
							clause++
							commands = append(commands, fmt.Sprintf(
								"set ly-route flow-rate rule %s_%d interface %s direction %s source %s destination %s protocol %s source-port %s destination-port %s rate-kbps %d burst-bytes %d",
								safeTag(target.RuleID), clause, interfaceName, direction,
								aclAddressValue(clauseSource), aclAddressValue(clauseDestination), flowRateProtocolName(protocol),
								portRange(clauseSourcePort), portRange(clauseDestinationPort), rateKbps, burstBytes,
							))
						}
					}
				}
			}
		}
	}
	return commands
}

func flowRateProtocolName(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "any", "0":
		return "any"
	case "tcp", "6":
		return "tcp"
	case "udp", "17":
		return "udp"
	case "icmp", "1":
		return "icmp"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func nativeLANInterface(name string) string {
	if strings.TrimSpace(name) == "host-$LY_ROUTE_LAN_INTERFACE" {
		return "lyroute-$LY_ROUTE_LAN_INTERFACE"
	}
	return name
}

func firstOrAny(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "any"
}

// PlanRequiresNAT reports whether the plan owns NAT44 objects. Port mappings,
// DNS service networks, NAT route actions and proxy steering all initialize
// NAT44 even when there is no static mapping, so evidence validation must use
// the same predicate as BuildOperations.
func PlanRequiresNAT(plan Plan) bool {
	return planRequiresNAT(plan)
}

func planRequiresNAT(plan Plan) bool {
	if len(plan.NAT.StaticMappings) > 0 || len(plan.NAT.PortMappings) > 0 || len(plan.DNSServiceNetworks) > 0 {
		return true
	}
	for _, policy := range plan.Policy.RoutePolicies {
		if strings.EqualFold(strings.TrimSpace(policy.Action), "nat") {
			return true
		}
	}
	for _, steering := range plan.Proxy.VPPSteering {
		if steering.TargetKind == "vpp.proxy-service.network" {
			return true
		}
	}
	// A plain routed gateway: one WAN outside interface plus a LAN ingress. A
	// gateway whose only NAT-worthy object is "a WAN line" still has to bind
	// NAT44 inside/outside, otherwise LAN clients have no SNAT: the gateway
	// forwards into the WAN but no reply ever comes back, and the operator sees
	// "the line is up but nothing goes out" until a port mapping is created.
	if planHasLANIngress(plan) && len(natOutsideInterfaces(plan)) > 0 {
		return true
	}
	return false
}

// planHasLANIngress reports whether the plan carries a LAN interface that
// NAT44 must treat as the inside domain.
func planHasLANIngress(plan Plan) bool {
	if len(plan.LANVPPInterfaces) > 0 {
		return true
	}
	for _, assignment := range plan.AddressAssignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			return true
		}
	}
	return false
}

// natOutsideInterfaces returns the deduplicated WAN interfaces that carry
// NAT44 translated traffic. Explicit mappings win; otherwise every configured
// WAN link is a candidate so a plain NAT gateway still gets a pool address.
func natOutsideInterfaces(plan Plan) []string {
	seen := map[string]bool{}
	result := make([]string, 0, 2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		result = append(result, name)
	}
	for _, mapping := range plan.NAT.StaticMappings {
		add(mapping.WANInterface)
	}
	for _, mapping := range plan.NAT.PortMappings {
		add(mapping.WANInterface)
	}
	// Every configured WAN is an outside interface, not only the WANs that
	// currently own a port mapping. A WAN-group backup must already have its
	// address in the translation pool when it becomes the active member.
	for _, assignment := range plan.AddressAssignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			add(assignment.VPPInterface)
		}
	}
	return result
}

func natInitializeCommands(behavior nat.Behavior) []string {
	if behavior == nat.BehaviorFullCone {
		return []string{
			"?nat44 plugin disable",
			"?nat44 ei plugin enable",
		}
	}
	return []string{
		"?nat44 ei plugin disable",
		"?nat44 plugin enable",
	}
}

func natStaticMappingCommands(mapping nat.StaticMapping) []string {
	return natStaticMappingCommandsForBehavior(nat.BehaviorEndpointDependent, mapping)
}

func natStaticMappingCommandsForBehavior(behavior nat.Behavior, mapping nat.StaticMapping) []string {
	if behavior == nat.BehaviorFullCone {
		commands := []string{}
		if mapping.WANInterface != "" {
			commands = append(commands,
				fmt.Sprintf("?set interface nat44 ei in %s output-feature del", mapping.WANInterface),
				fmt.Sprintf("?set interface nat44 ei in %s output-feature", mapping.WANInterface),
				fmt.Sprintf("?nat44 ei add interface address %s", mapping.WANInterface),
			)
		}
		commands = append(commands,
			fmt.Sprintf("?nat44 ei add static mapping local %s external %s", mapping.InternalAddress, mapping.ExternalAddress),
			"show nat44 ei static mappings",
			"show nat44 ei sessions",
		)
		return commands
	}
	commands := []string{}
	if mapping.WANInterface != "" {
		// NAT44-ED has one inside interface (LAN) and one outside interface
		// (WAN). Marking the WAN as an input/inside interface as well is wrong
		// and prevents hairpin traffic from being routed back out the LAN.
		commands = append(commands,
			// The combined "in <lan> out <wan>" form clears the
			// output-feature flag on every other outside WAN, so a
			// multi-WAN apply left only the last mapping able to translate
			// policy-routed traffic. Bind the LAN inside once and re-arm
			// this WAN's output-feature flag independently.
			fmt.Sprintf("?set interface nat44 in %s output-feature del", mapping.WANInterface),
			fmt.Sprintf("?set interface nat44 out %s del", mapping.WANInterface),
			fmt.Sprintf("?set interface nat44 out %s output-feature del", mapping.WANInterface),
			"?set interface nat44 in lyroute-$LY_ROUTE_LAN_INTERFACE",
			fmt.Sprintf("set interface nat44 out %s output-feature", mapping.WANInterface),
		)
	}
	commands = append(commands,
		fmt.Sprintf("?nat44 add static mapping local %s external %s", mapping.InternalAddress, mapping.ExternalAddress),
		"show nat44 static mappings",
		"show nat44 sessions",
	)
	return commands
}

func natPortMappingCommands(mapping nat.PortMapping) []string {
	return natPortMappingCommandsForBehavior(nat.BehaviorEndpointDependent, mapping)
}

func natPortMappingCommandsForBehavior(behavior nat.Behavior, mapping nat.PortMapping) []string {
	if behavior == nat.BehaviorFullCone {
		commands := []string{}
		if mapping.WANInterface != "" {
			commands = append(commands,
				fmt.Sprintf("?set interface nat44 ei in %s output-feature del", mapping.WANInterface),
				fmt.Sprintf("?set interface nat44 ei in %s output-feature", mapping.WANInterface),
				fmt.Sprintf("?nat44 ei add interface address %s", mapping.WANInterface),
			)
		}
		mappingCommand := fmt.Sprintf("nat44 ei add static mapping %s local %s %d external %s %d", mapping.Protocol, mapping.InternalHost, mapping.InternalPort, mapping.ExternalAddress, mapping.ExternalPort)
		commands = append(commands,
			"show nat44 ei static mappings",
			fmt.Sprintf("?%s del", mappingCommand),
			fmt.Sprintf("?%s", mappingCommand),
			"show nat44 ei static mappings",
			"show nat44 ei sessions",
		)
		return commands
	}
	commands := []string{}
	if mapping.WANInterface != "" {
		commands = append(commands,
			// The combined "in <lan> out <wan>" form clears the
			// output-feature flag on every other outside WAN, so a
			// multi-WAN apply left only the last mapping able to translate
			// policy-routed traffic. Bind the LAN inside once and re-arm
			// this WAN's output-feature flag independently.
			fmt.Sprintf("?set interface nat44 in %s output-feature del", mapping.WANInterface),
			fmt.Sprintf("?set interface nat44 out %s del", mapping.WANInterface),
			fmt.Sprintf("?set interface nat44 out %s output-feature del", mapping.WANInterface),
			"?set interface nat44 in lyroute-$LY_ROUTE_LAN_INTERFACE",
			fmt.Sprintf("set interface nat44 out %s output-feature", mapping.WANInterface),
		)
	}
	if mapping.Hairpin {
		// The LAN-to-WAN inside/outside binding is installed above for every
		// mapping. Hairpin additionally needs the external address in the NAT44
		// pool so the translated packet can be sent back through the LAN.
		commands = append(commands, fmt.Sprintf("?nat44 add address %s", mapping.ExternalAddress), "show nat44 addresses")
	}
	mappingCommand := fmt.Sprintf("nat44 add static mapping %s local %s %d external %s %d", mapping.Protocol, mapping.InternalHost, mapping.InternalPort, mapping.ExternalAddress, mapping.ExternalPort)
	commands = append(commands,
		"show nat44 static mappings",
		fmt.Sprintf("?%s del", mappingCommand),
		fmt.Sprintf("?%s", mappingCommand),
		"show nat44 static mappings",
		// The bounded replay executor passes commands as argv, so a shell-style
		// pipe is not guaranteed to be available. These are diagnostic filters,
		// not the authoritative readback (the typed snapshot does that); never
		// fail a whole boot replay merely because the filter produced no row.
		fmt.Sprintf("?show nat44 static mappings | include %s", mapping.ExternalAddress),
		fmt.Sprintf("?show nat44 static mappings | include %s", safeTag(mapping.ID)),
		"show nat44 sessions",
	)
	if mapping.Hairpin {
		commands = append(commands, "?show nat44 sessions | include hairpin")
	}
	return commands
}

// routePolicyCommandOptions describes the optional FIB-table implementation
// used for a destination-only route policy chain.  The legacy ACL path is
// deliberately retained for policies that contain source/port/protocol
// predicates; those predicates cannot be represented by a plain FIB lookup.
type routePolicyCommandOptions struct {
	optimizedIPv4     bool
	defaultVia        string
	localDestinations []string
}

// addRoutePolicyLocalDestinations keeps a catch-all policy from stealing
// traffic addressed to the gateway/LAN itself.  This is a routing invariant,
// not a proxy-specific exception: the LAN prefix must remain reachable by
// the normal local FIB even when the user deliberately selects an any-to-any
// egress policy.
func addRoutePolicyLocalDestinations(options map[string]routePolicyCommandOptions, policies []trafficpolicy.RoutePolicy, assignments []AddressAssignment) {
	addRoutePolicyLocalDestinationPrefixes(options, policies, routePolicyLocalDestinations(assignments))
	// The classifier binds one LAN and takes the first local destination as its
	// lan-prefix. Put each policy's own source LAN first so the installed header
	// and the route readback expectation describe the same LAN.
	bindings := routePolicyLANBindings(assignments)
	for _, policy := range policies {
		prioritizeRoutePolicyLANPrefix(options, policy, bindings)
	}
}

func addRoutePolicyLocalDestinationPrefixes(options map[string]routePolicyCommandOptions, policies []trafficpolicy.RoutePolicy, prefixes []string) {
	if len(prefixes) == 0 {
		return
	}
	for _, policy := range policies {
		id := strings.TrimSpace(policy.ID)
		if id == "" {
			continue
		}
		option := options[id]
		option.localDestinations = append([]string(nil), prefixes...)
		options[id] = option
	}
}

func routePolicyLocalDestinations(assignments []AddressAssignment) []string {
	seen := map[string]struct{}{}
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		seen[prefix.Masked().String()] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for prefix := range seen {
		result = append(result, prefix)
	}
	sort.Strings(result)
	return result
}

// orderedRoutePoliciesForVPP creates the lower-priority tables first.  A
// chained table can only install a lookup DPO after its next table exists.
// The policy priority in the generated ABF attachment remains unchanged, so
// this is only an implementation ordering detail.
func orderedRoutePoliciesForVPP(policies []trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup) []trafficpolicy.RoutePolicy {
	ordered := append([]trafficpolicy.RoutePolicy(nil), policies...)
	options := buildRoutePolicyCommandOptions(ordered, wanGroups)
	if len(options) == 0 || len(ordered) < 2 {
		return ordered
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority == ordered[j].Priority {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Priority > ordered[j].Priority
	})
	return ordered
}

// buildRoutePolicyCommandOptions recognizes the destination-only suffix of an
// ordered policy list. Earlier source/protocol exceptions are evaluated first
// by their normal ACL rules; once they miss, the suffix can safely continue in
// native FIB tables without changing policy precedence.
func buildRoutePolicyCommandOptions(policies []trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup) map[string]routePolicyCommandOptions {
	options := make(map[string]routePolicyCommandOptions)
	if len(policies) == 0 {
		return options
	}
	ordered := append([]trafficpolicy.RoutePolicy(nil), policies...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	chainStart := 0
	chainEnd := -1
	for index, policy := range ordered {
		if !routePolicyFIBChainEligible(policy) || len(routePolicyIPv4Destinations(policy.Match.Destinations)) == 0 {
			if chainEnd >= 0 {
				// A lower-precedence non-FIB rule cannot be reached after the
				// effective catch-all, but it must not invalidate the chain that
				// was already established above it.
				break
			}
			chainStart = index + 1
			continue
		}
		// The first destination-only catch-all marks the effective end of the
		// chain. Consecutive destination-only entries may still be part of the
		// same native chain; a later ordinary rule closes it above.
		if chainEnd < 0 && routePolicyHasIPv4CatchAll(policy.Match.Destinations) {
			chainEnd = index + 1
		} else if chainEnd >= 0 {
			chainEnd = index + 1
		}
	}
	if chainEnd < 0 {
		return map[string]routePolicyCommandOptions{}
	}
	chain := ordered[chainStart:chainEnd]
	if len(chain) < 2 || !routePolicyHasIPv4CatchAll(chain[len(chain)-1].Match.Destinations) {
		return map[string]routePolicyCommandOptions{}
	}
	// Equal priorities at the ACL/FIB boundary have no deterministic ordering,
	// so keep the conservative ACL path in that ambiguous case.
	if chainStart > 0 && ordered[chainStart-1].Priority == chain[0].Priority {
		return map[string]routePolicyCommandOptions{}
	}
	for index, policy := range chain {
		defaultVia := routePolicyTarget(policy, wanGroups)
		if index+1 < len(chain) {
			defaultVia = fmt.Sprintf("ip4-lookup-in-table %d", stableID("route-table:"+chain[index+1].ID, 50000, 49999))
		}
		options[policy.ID] = routePolicyCommandOptions{optimizedIPv4: true, defaultVia: defaultVia}
	}
	return options
}

// orderedRoutePolicySubsetForVPP preserves the dependency order of the full
// chain while emitting only the policies changed by an incremental apply.
// Without the full context, a one-policy repair would be mistaken for an
// incomplete chain and could fall back to a giant ACL.
func orderedRoutePolicySubsetForVPP(routes, context []trafficpolicy.RoutePolicy, options map[string]routePolicyCommandOptions, wanGroups map[string]trafficpolicy.WANGroup) []trafficpolicy.RoutePolicy {
	ordered := append([]trafficpolicy.RoutePolicy(nil), routes...)
	if len(options) == 0 {
		return orderedRoutePoliciesForVPP(ordered, wanGroups)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority == ordered[j].Priority {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Priority > ordered[j].Priority
	})
	return ordered
}

const maxRoutePolicyACLIPv4Prefixes = 256

// Large destination sets must never be serialized into one ACL command. The
// pre-NAT Radix classifier is the primary native path because it preserves the
// policy priority of every rule independently. A complete destination-only
// FIB chain remains a useful optimization, but it must never be a prerequisite
// for accepting a large GeoIP rule: an unrelated UI rule can legally sit
// between the GeoIP policy and the terminal catch-all.
func validateLargeRoutePolicyFallback(routes []trafficpolicy.RoutePolicy, options map[string]routePolicyCommandOptions, wanGroups map[string]trafficpolicy.WANGroup) error {
	for _, route := range routes {
		prefixes := routePolicyIPv4Destinations(route.Match.Destinations)
		if len(prefixes) <= maxRoutePolicyACLIPv4Prefixes {
			continue
		}
		option := options[route.ID]
		if _, nativeRadix := compileRoutePolicyRadixPlan(route, wanGroups, option); nativeRadix {
			continue
		}
		if option.optimizedIPv4 {
			continue
		}
		return fmt.Errorf("route policy %q has %d IPv4 destinations but no verified native Radix/FIB path; refusing large ACL fallback", route.ID, len(prefixes))
	}
	return nil
}

func routePolicyFIBChainEligible(policy trafficpolicy.RoutePolicy) bool {
	if strings.ToLower(strings.TrimSpace(policy.Action)) != "route" {
		return false
	}
	match := policy.Match
	return routePolicyAnySelector(match.Sources) &&
		routePolicyAnySelector(match.Protocols) &&
		routePolicyAnySelector(match.SourcePorts) &&
		routePolicyAnySelector(match.DestPorts)
}

func routePolicyAnySelector(values []string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "any", "0.0.0.0/0", "::/0":
			continue
		default:
			return false
		}
	}
	return true
}

func routePolicyIPv4Destinations(values []string) []string {
	seen := make(map[string]struct{})
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || strings.EqualFold(value, "any") || value == "0.0.0.0/0" {
			seen["0.0.0.0/0"] = struct{}{}
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Addr().Is4() {
			seen[prefix.Masked().String()] = struct{}{}
			continue
		}
		if address, err := netip.ParseAddr(value); err == nil && address.Is4() {
			seen[(netip.PrefixFrom(address, 32)).String()] = struct{}{}
		}
	}
	if len(seen) == 0 && len(values) == 0 {
		seen["0.0.0.0/0"] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for prefix := range seen {
		result = append(result, prefix)
	}
	sort.Strings(result)
	return result
}

func routePolicyHasIPv4CatchAll(values []string) bool {
	for _, value := range routePolicyIPv4Destinations(values) {
		if value == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

func routePolicyTarget(policy trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup) string {
	if wanGroups != nil {
		if group, ok := wanGroups[policy.Egress]; ok {
			return fmt.Sprintf("ip4-lookup-in-table %d", wanGroupTableID(group.ID))
		}
	}
	return routeNextHop(policy)
}

// routePolicyCommands is kept as the compatibility wrapper used by focused
// unit tests and the route/WAN lifecycle.  Production plan construction uses
// routePolicyCommandsWithOptions so large provider datasets never become one
// VPP CLI line.
func routePolicyCommands(policy trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup) []string {
	return routePolicyCommandsWithOptions(policy, wanGroups, routePolicyCommandOptions{})
}

// routePolicyUsesProxyServiceInterface reports whether a route policy steers
// into a VPP proxy service ingress TAP.  Those policies must keep the explicit
// ABF policy/attach in the compiled plan (the runtime pre-NAT classifier is
// installed on top of it), so the proxy handoff is visible to the gateway
// readback and a missing ABF object can never be reported as committed.
func routePolicyUsesProxyServiceInterface(policy trafficpolicy.RoutePolicy) bool {
	if policy.Path == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(policy.Path.VPPInterface)), "lypxin")
}

func routePolicyCommandsWithOptions(policy trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup, options routePolicyCommandOptions) []string {
	aclID := stableID("route-acl:"+policy.ID, 10000, 49999)
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	if !routePolicyNeedsLocalACLDeny(policy, options) && !routePolicyUsesProxyServiceInterface(policy) {
		if commands, ok := routePolicyRadixCommands(policy, wanGroups, options, policyID, tableID); ok {
			return commands
		}
	}
	groupTableID := 0
	if _, ok := wanGroups[policy.Egress]; ok {
		groupTableID = wanGroupTableID(policy.Egress)
	}
	match := policy.Match
	if options.optimizedIPv4 {
		match = trafficpolicy.Match{
			Sources:      append([]string(nil), match.Sources...),
			Destinations: []string{"0.0.0.0/0"},
			Protocols:    []string{"any"},
			SourcePorts:  []string{"any"},
			DestPorts:    []string{"any"},
		}
	}
	commands := routePolicyACLCommands(aclID, policy.ID, match, routeACLAction(policy.Action), options)
	commands = append(commands,
		fmt.Sprintf("?ip table add %d", tableID),
		fmt.Sprintf("?set ip flow-hash table %d src dst sport dport proto", tableID),
	)
	if policy.Action != "deny" {
		routeTarget := routePolicyTarget(policy, wanGroups)
		if options.optimizedIPv4 {
			commands = append(commands, vppRouteBatchBegin)
			for _, prefix := range options.localDestinations {
				commands = append(commands, fmt.Sprintf("ip route add table %d %s via lyroute-$LY_ROUTE_LAN_INTERFACE", tableID, prefix))
			}
			for _, prefix := range routePolicyIPv4Destinations(policy.Match.Destinations) {
				commands = append(commands, fmt.Sprintf("ip route add table %d %s via %s", tableID, prefix, routeTarget))
			}
			commands = append(commands, vppRouteBatchEnd)
			if strings.TrimSpace(options.defaultVia) != "" {
				routeTarget = options.defaultVia
			}
		}
		if strings.HasPrefix(routeTarget, "ip4-lookup-in-table ") || strings.HasPrefix(routeTarget, "ip6-lookup-in-table ") {
			// VPP accepts the table-first form for lookup DPOs as well.
			commands = append(commands, fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via %s", tableID, routeTarget))
		} else {
			commands = append(commands, fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via %s", tableID, routeTarget))
		}
	}
	abfVia := routePolicyTarget(policy, wanGroups)
	if options.optimizedIPv4 {
		abfVia = fmt.Sprintf("ip4-lookup-in-table %d", tableID)
	} else if groupTableID > 0 {
		abfVia = fmt.Sprintf("ip4-lookup-in-table %d", groupTableID)
	}
	commands = append(commands,
		fmt.Sprintf("?abf policy add id %d acl %d via %s", policyID, aclID, abfVia),
		fmt.Sprintf("?abf attach ip4 policy %d priority %d lyroute-$LY_ROUTE_LAN_INTERFACE", policyID, vppABFPriority(policy.Priority)),
		fmt.Sprintf("show acl-plugin acl index %d", aclID),
		fmt.Sprintf("show abf policy %d", policyID),
		// VPP 25.x may render a point-to-point ABF path as
		// "<next-hop> if_index:<n>". Keep the interface inventory in the
		// same transaction so readback can prove that the path belongs to
		// the intended service interface instead of accepting an arbitrary
		// unresolved/stale path.
		"show interface",
		"show abf attach lyroute-$LY_ROUTE_LAN_INTERFACE",
		fmt.Sprintf("show ip fib table %d", tableID),
	)
	if groupTableID > 0 {
		commands = append(commands, fmt.Sprintf("show ip fib table %d", groupTableID))
	}
	return commands
}

func routePolicyRadixCommands(policy trafficpolicy.RoutePolicy, wanGroups map[string]trafficpolicy.WANGroup, options routePolicyCommandOptions, policyID, tableID int) ([]string, bool) {
	plan, ok := compileRoutePolicyRadixPlan(policy, wanGroups, options)
	if !ok {
		return nil, false
	}
	commands := []string{
		fmt.Sprintf("?ip table add %d", tableID),
		fmt.Sprintf("?set ip flow-hash table %d src dst sport dport proto", tableID),
	}
	commands = append(commands, fmt.Sprintf("?ip route add table %d 0.0.0.0/0 via %s", tableID, plan.routeTarget))
	// The pre-NAT classifier below installs a higher-priority LAN-to-LAN
	// bypass (lanLocalPreNATRuleID). Do not duplicate that bypass as a local
	// FIB route: VPP 26.06 crashes in fib_attached_export_import when a
	// connected/interface path is added to a private table immediately after
	// its lookup-DPO default route. The bypass is still enforced before the
	// policy table is consulted.
	classifier, applied, err := buildPreNATRoutePolicyCommandsForTable(
		policy, policyID, tableID, "lyroute-$LY_ROUTE_LAN_INTERFACE", plan.lanPrefix, plan.skipNAT,
	)
	if err != nil || !applied {
		return nil, false
	}
	commands = append(commands, classifier...)
	commands = append(commands, fmt.Sprintf("show ip fib table %d", tableID))
	return commands, true
}

func vppABFPriority(priority int) int {
	// DNS fixed-egress overrides reserve the negative logical range beginning at
	// -100000. VPP accepts only non-negative ABF priorities, so preserve the
	// generated rule order in a dedicated low range ahead of ordinary PBR.
	if priority <= -100000 {
		mapped := priority + 100000
		if mapped < 0 {
			return 0
		}
		if mapped > 255 {
			return 255
		}
		return mapped
	}
	if priority < 0 {
		return 0
	}
	if priority > 255 {
		return 255
	}
	return priority
}

// builtInWANDenyACLID is the seeded catch-all "Default WAN Inbound Deny"
// rule. It is never rendered into the dataplane: see securityACLCommands.
const builtInWANDenyACLID = "sec-acl-default-deny-wan"

func securityACLCommands(acl trafficpolicy.SecurityACL) []string {
	aclID := stableID("security-acl:"+acl.ID, 50000, 49999)
	commands := aclMatchCommands(aclID, acl.ID, acl.Match, acl.Action)
	interfaceName := "lyroute-$LY_ROUTE_LAN_INTERFACE"
	directions := securityDirections(acl.Match.Direction)
	if acl.ID == builtInWANDenyACLID {
		// The seeded "Default WAN Inbound Deny" is an unconditional
		// catch-all deny (the "dst_ip: wan" selector compiles to
		// 0.0.0.0/0). It cannot be deployed: at LAN egress/ingress it
		// blackholes every client including DHCP replies, and at WAN
		// ingress it drops NAT44 return traffic and port-forwarded flows
		// before translation. NAT44-ED is stateful and already drops
		// unsolicited inbound traffic, so the rule stays unattached.
		return nil
	}
	for _, direction := range directions {
		commands = append(commands, fmt.Sprintf("?set interface %s acl intfc %s ip4-table %d", direction, interfaceName, aclID))
	}
	commands = append(commands, fmt.Sprintf("show acl-plugin acl index %d", aclID), "show interface "+interfaceName)
	return commands
}

func wanGroupCommands(group trafficpolicy.WANGroup) []string {
	tableID := wanGroupTableID(group.ID)
	commands := []string{
		fmt.Sprintf("?ip table add %d", tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/1", tableID),
		fmt.Sprintf("?ip route del table %d 128.0.0.0/1", tableID),
	}
	if group.Mode != trafficpolicy.WANGroupPrimaryBackup {
		commands = append(commands, fmt.Sprintf("?set ip flow-hash table %d src dst sport dport proto", tableID))
	}
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		command := fmt.Sprintf("?ip route add table %d %s", tableID, prefix)
		for index, member := range group.Members {
			path := group.Paths[member]
			via := routePathVia(path.VPPInterface, path.NextHop, member)
			weight := group.Weights[member]
			if weight < 1 {
				weight = 1
			}
			preference := 0
			if group.Mode == trafficpolicy.WANGroupPrimaryBackup {
				weight = 1
				preference = index
			}
			command += fmt.Sprintf(" via %s weight %d preference %d", via, weight, preference)
		}
		commands = append(commands, command)
	}
	commands = append(commands, fmt.Sprintf("show ip fib table %d", tableID))
	return commands
}

func wanGroupTableID(id string) int {
	return stableID("wan-group:"+id, 50000, 49999)
}

// WANGroupTableID exposes the stable table identity to service-chain
// compilers that need to hand traffic into an already-built WAN group.
func WANGroupTableID(id string) int {
	return wanGroupTableID(id)
}

func aclMatchCommands(aclID int, resource string, match trafficpolicy.Match, action string) []string {
	rules := []string{}
	for _, source := range nonEmptyList(match.Sources, "0.0.0.0/0") {
		for _, destination := range nonEmptyList(match.Destinations, "0.0.0.0/0") {
			source = aclAddressValue(source)
			destination = aclAddressValue(destination)
			for _, protocol := range nonEmptyList(match.Protocols, "any") {
				protocol = aclProtocolValue(protocol)
				for _, sourcePort := range nonEmptyList(match.SourcePorts, "any") {
					for _, destPort := range nonEmptyList(match.DestPorts, "any") {
						rules = append(rules, fmt.Sprintf("%s src %s dst %s proto %s sport %s dport %s", action, source, destination, protocol, portRange(sourcePort), portRange(destPort)))
					}
				}
			}
		}
	}
	return []string{fmt.Sprintf("?set acl-plugin acl index %d %s tag ly-route-%s", aclID, strings.Join(rules, ", "), safeTag(resource))}
}

func routePolicyNeedsLocalACLDeny(policy trafficpolicy.RoutePolicy, options routePolicyCommandOptions) bool {
	// A source-scoped catch-all on the route/ACL path needs an explicit LAN
	// deny before the catch-all permit.  NAT and destination-only radix paths
	// protect the LAN with table routes instead and must keep their native
	// classifier.
	return len(options.localDestinations) > 0 &&
		(policy.Action == "route" || policy.Action == "nat") &&
		routePolicyHasIPv4CatchAll(policy.Match.Destinations) &&
		len(policy.Match.Sources) > 0 &&
		policy.Path == nil
}

func routePolicyACLCommands(aclID int, resource string, match trafficpolicy.Match, action string, options routePolicyCommandOptions) []string {
	commands := aclMatchCommands(aclID, resource, match, action)
	if action != "permit" || options.optimizedIPv4 || len(options.localDestinations) == 0 || !routePolicyHasIPv4CatchAll(match.Destinations) || len(commands) == 0 {
		return commands
	}
	denies := make([]string, 0)
	for _, source := range nonEmptyList(match.Sources, "0.0.0.0/0") {
		for _, destination := range options.localDestinations {
			for _, protocol := range nonEmptyList(match.Protocols, "any") {
				for _, sourcePort := range nonEmptyList(match.SourcePorts, "any") {
					for _, destPort := range nonEmptyList(match.DestPorts, "any") {
						denies = append(denies, fmt.Sprintf("deny src %s dst %s proto %s sport %s dport %s", aclAddressValue(source), aclAddressValue(destination), aclProtocolValue(protocol), portRange(sourcePort), portRange(destPort)))
					}
				}
			}
		}
	}
	if len(denies) == 0 {
		return commands
	}
	tagIndex := strings.LastIndex(commands[0], " tag ")
	prefix := fmt.Sprintf("?set acl-plugin acl index %d ", aclID)
	if tagIndex < 0 || !strings.HasPrefix(commands[0], prefix) {
		return commands
	}
	// VPP ACLs are first-match. The local deny must precede the catch-all
	// permit or packets to the gateway/LAN are still sent into NAT and ABF.
	rules := commands[0][len(prefix):tagIndex]
	commands[0] = prefix + strings.Join(denies, ", ") + ", " + rules + commands[0][tagIndex:]
	return commands
}

func aclMatchCommandsWithFallback(aclID int, resource string, match trafficpolicy.Match, action string) []string {
	commands := aclMatchCommands(aclID, resource, match, action)
	if action != "permit" || len(commands) == 0 {
		return commands
	}
	command := commands[0]
	tagIndex := strings.LastIndex(command, " tag ")
	if tagIndex < 0 {
		return commands
	}
	// ACLs attached by a rate rule are evaluated by both IP feature arcs.
	// A v4-only catch-all does not match IPv6 and VPP defaults an unmatched
	// ACL packet to deny, which would turn an IPv4 rate rule into an IPv6
	// outage. Keep nonmatching traffic explicitly permitted in both families.
	fallbacks := []string{
		"permit src 0.0.0.0/0 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535",
		"permit src ::/0 dst ::/0 proto 0 sport 0-65535 dport 0-65535",
	}
	missing := make([]string, 0, len(fallbacks))
	for _, fallback := range fallbacks {
		if !strings.Contains(command, fallback) {
			missing = append(missing, fallback)
		}
	}
	if len(missing) == 0 {
		return commands
	}
	commands[0] = command[:tagIndex] + ", " + strings.Join(missing, ", ") + command[tagIndex:]
	return commands
}

func aclAddressValue(address string) string {
	address = strings.TrimSpace(address)
	if address == "" || strings.EqualFold(address, "any") {
		return "0.0.0.0/0"
	}
	return address
}

func aclProtocolValue(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "any":
		return "0"
	case "icmp":
		return "1"
	case "tcp":
		return "6"
	case "udp":
		return "17"
	case "icmpv6":
		return "58"
	default:
		return protocol
	}
}

func routeACLAction(action string) string {
	if action == "deny" {
		return "deny"
	}
	return "permit"
}

func routeNextHop(policy trafficpolicy.RoutePolicy) string {
	if policy.Path != nil {
		return routePathVia(policy.Path.VPPInterface, policy.Path.NextHop, "")
	}
	if policy.NextHop != "" {
		return policy.NextHop
	}
	if policy.Egress != "" {
		return policy.Egress
	}
	return "local"
}

// configuredLANVPPInterface is only a last-resort recovery path for cleanup
// transactions created from an older persisted snapshot. Normal plans carry
// the resolved LAN interface in RouteWANGroupPlan. The explicit VPP variable
// lets an appliance remove a stale ABF attachment safely during an upgrade
// without guessing from an arbitrary physical port.
// gatewayLANPrefix returns the LAN network prefix used by the pre-NAT
// classifier.  It is derived from the same address assignments used to build
// the VPP plan rather than from the process environment so a replay and a live
// apply use the same state.
func gatewayLANPrefix(plan Plan) string {
	for _, assignment := range plan.AddressAssignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		cidr := strings.TrimSpace(assignment.CIDR)
		if cidr == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		return prefix.Masked().String()
	}
	return ""
}

// tokenizePersistedWANAddress replaces a dynamic PPPoE address in durable
// replay commands with a token resolved from the current session status.  A
// lease can change while the box is down; persisting the old public address
// makes the boot replay fail its own readback and leaves NAT without a mapping.
func tokenizePersistedWANAddress(commands []string, wanInterface, externalAddress string, explicit bool) []string {
	wanInterface = strings.TrimSpace(wanInterface)
	externalAddress = strings.TrimSpace(externalAddress)
	if explicit || wanInterface == "" || externalAddress == "" || !isRuntimePPPoEInterface(wanInterface) {
		return commands
	}
	token := "ly-route-wan-ip:" + wanInterface
	result := make([]string, len(commands))
	for index, command := range commands {
		result[index] = strings.ReplaceAll(command, externalAddress, token)
	}
	return result
}

func configuredLANVPPInterface() string {
	for _, key := range []string{"LY_ROUTE_LAN_VPP_INTERFACE", "LY_ROUTE_LAN_INTERFACE"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" || strings.ContainsAny(value, " \t\r\n;$\"") {
			continue
		}
		if strings.HasPrefix(value, "lyroute-") {
			return value
		}
		return "lyroute-" + value
	}
	return ""
}

// routePathVia returns the VPP route path syntax for a resolved WAN path.
// VPP 25.10 accepts a PPPoE path only when its negotiated peer is supplied.
// An interface-only PPPoE ABF path can also crash `show abf policy`, so a
// temporarily incomplete session uses the native main-table lookup until the
// live peer readback is available. Ordinary interfaces keep their direct path.
func routePathVia(vppInterface, nextHop, fallback string) string {
	via := strings.TrimSpace(vppInterface)
	if via == "" {
		via = strings.TrimSpace(fallback)
	}
	if via == "" {
		return strings.TrimSpace(nextHop)
	}
	if nextHop = strings.TrimSpace(nextHop); nextHop != "" {
		return nextHop + " " + via
	}
	if isRuntimePPPoEInterface(via) {
		return "ip4-lookup-in-table 0"
	}
	return via
}

func isRuntimePPPoEInterface(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "pppoe_session") ||
		strings.HasPrefix(lower, "pppox") ||
		strings.HasPrefix(lower, "lyppp-") ||
		strings.HasPrefix(lower, "pppoe-runtime:")
}

func nonEmptyList(items []string, fallback string) []string {
	if len(items) == 0 {
		return []string{fallback}
	}
	return items
}

func portRange(value string) string {
	if value == "" || value == "any" {
		return "0-65535"
	}
	if strings.Contains(value, "-") {
		return value
	}
	return value + "-" + value
}

func stableID(value string, minimum, span int) int {
	if span <= 0 {
		return minimum
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return minimum + int(binary.BigEndian.Uint32(digest[:4])%uint32(span))
}

func safeTag(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), "_")
	if result == "" {
		return "default"
	}
	return result
}

func qosClassValue(target flow.Target) int {
	class := strings.ToLower(strings.TrimSpace(target.Class))
	if class == "" {
		class = strings.ToLower(strings.TrimSpace(target.RuleID))
	}
	return stableID("qos-class:"+class, 1, 62)
}

// dscpValue converts a DSCP codepoint (name or 0-63 number) into the value
// VPP's egress map writes into the IPv4 TOS byte. VPP assigns the map output
// directly to ip4->tos (qos_mark_node.c), so the DSCP must be shifted left by
// two bits here. Writing the bare codepoint marked EF traffic as TOS 46
// (DSCP 11) instead of TOS 184 (DSCP 46), which no downstream device could
// recognise.
func dscpValue(value string) int {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	if parsed, err := strconv.Atoi(trimmed); err == nil && parsed >= 0 && parsed <= 63 {
		return parsed << 2
	}
	if value, ok := map[string]int{
		"CS0":  0,
		"CS1":  8,
		"AF11": 10,
		"AF12": 12,
		"AF13": 14,
		"CS2":  16,
		"AF21": 18,
		"AF22": 20,
		"AF23": 22,
		"CS3":  24,
		"AF31": 26,
		"AF32": 28,
		"AF33": 30,
		"CS4":  32,
		"AF41": 34,
		"AF42": 36,
		"AF43": 38,
		"CS5":  40,
		"EF":   46,
		"CS6":  48,
		"CS7":  56,
	}[strings.ToUpper(trimmed)]; ok {
		return value << 2
	}
	return 0
}

func NormalizeRetval(operation, requestID string, retval int32) error {
	if retval == 0 {
		return nil
	}
	return VPPError{Operation: strings.TrimSpace(operation), RequestID: strings.TrimSpace(requestID), Retval: retval}
}

func ConsumeMultipart(ctx context.Context, operation Operation, stream MultipartStream) ([]Reply, error) {
	var replies []Reply
	for {
		reply, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return replies, nil
		}
		if err != nil {
			return nil, VPPError{Operation: operation.Name, RequestID: operation.RequestID, Err: err}
		}
		if err := NormalizeRetval(operation.Name, operation.RequestID, reply.Retval); err != nil {
			return nil, err
		}
		if reply.Done || reply.ControlPing {
			return replies, nil
		}
		replies = append(replies, reply)
	}
}

// rewriteRoutePolicyIngress points each route-policy operation at the LAN whose
// prefix contains the policy's source. Policies with an any-source selector (or
// a source outside every LAN) keep the shared placeholder and fall back to the
// first LAN in rewriteOperationsInterface.
func rewriteRoutePolicyIngress(operations []Operation, assignments []AddressAssignment) []Operation {
	bindings := routePolicyLANBindings(assignments)
	if len(bindings) == 0 {
		return operations
	}
	for index := range operations {
		if !strings.HasPrefix(operations[index].Name, "vpp.route-policy") {
			continue
		}
		policy, ok := operations[index].Payload.(trafficpolicy.RoutePolicy)
		if !ok {
			continue
		}
		binding, ok := routePolicySourceLANBinding(policy, bindings)
		if !ok {
			continue
		}
		for commandIndex, command := range operations[index].VPPCtlCommands {
			operations[index].VPPCtlCommands[commandIndex] = strings.ReplaceAll(command, "lyroute-$LY_ROUTE_LAN_INTERFACE", binding.VPPInterface)
		}
	}
	return operations
}

// RouteABFPolicyID is the stable ABF policy id for a route policy. Exported so
// the control plane can record which policies the desired state owns and sweep
// the ones left behind by deleted routes.
func RouteABFPolicyID(routeID string) int {
	return stableID("route-abf:"+routeID, 10000, 8999)
}
