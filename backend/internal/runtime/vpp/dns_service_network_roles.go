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

const (
	defaultDNSServiceInterfaceAttempts = 100
	defaultDNSServiceInterfaceInterval = 200 * time.Millisecond
	defaultDNSServiceNATAttempts       = 25
	defaultDNSServiceNATInterval       = 200 * time.Millisecond
)

// DNSServiceNetworkNATError reports exactly which side of the DNS service
// handoff is missing. The apply used to collapse "the role was not applied" and
// "the interface does not exist" into one empty `show nat44 interfaces` line,
// which made a clean-boot failure indistinguishable from a broken NAT44 plugin.
type DNSServiceNetworkNATError struct {
	UpstreamID      string
	VPPInterface    string
	Underlay        string
	MissingTAP      bool
	MissingUnderlay bool
	RolePresent     bool
	Observed        string
	Reason          string
	// The pool fields make an interface-role success distinguishable from a
	// usable translation path. VPP happily keeps `lydns... in` installed while
	// the service VRF has no outside address, then drops every packet with
	// "out of ports".
	ExpectedPoolAddress string
	PoolVRF             int
	MissingPool         bool
	MissingVRF          bool
	MissingRoute        bool
}

func (failure *DNSServiceNetworkNATError) Error() string {
	if failure == nil {
		return ""
	}
	tap := strings.TrimSpace(failure.VPPInterface)
	underlay := strings.TrimSpace(failure.Underlay)
	switch {
	case failure.MissingTAP && failure.MissingUnderlay:
		return fmt.Sprintf("DNS service interface %q and its underlay %q are both missing in VPP; the NAT44 input role was not applied", tap, underlay)
	case failure.MissingTAP:
		if underlay != "" {
			return fmt.Sprintf("DNS service interface %q is missing in VPP (the service TAP was not created); the NAT44 input role on underlay %q was not applied", tap, underlay)
		}
		return fmt.Sprintf("DNS service interface %q is missing in VPP (the service TAP was not created); the NAT44 input role was not applied", tap)
	case failure.MissingUnderlay:
		return fmt.Sprintf("DNS service interface %q exists but its underlay %q is missing in VPP (the WAN port is not attached); the NAT44 input role was not applied", tap, underlay)
	case failure.MissingVRF:
		return fmt.Sprintf("DNS service interface %q is not in tenant VRF %d; VPP cannot translate through a pool owned by another VRF: %s", tap, failure.PoolVRF, strings.TrimSpace(failure.Reason))
	case failure.MissingPool:
		return fmt.Sprintf("NAT44 has no outside address for DNS service interface %q in tenant VRF %d (expected %q): %s", tap, failure.PoolVRF, failure.ExpectedPoolAddress, strings.TrimSpace(failure.Reason))
	case failure.MissingRoute:
		return fmt.Sprintf("DNS service VRF %d has no default route for service interface %q: %s", failure.PoolVRF, tap, strings.TrimSpace(failure.Reason))
	}
	reason := strings.TrimSpace(failure.Reason)
	if reason == "" {
		reason = "the role was not observed after the interface existed"
	}
	return fmt.Sprintf("NAT44 input role for %q on underlay %q was not applied even though both interfaces exist in VPP: %s", tap, underlay, reason)
}

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
	poolAddresses, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		return err
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open service-network verify channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	for index, network := range plan.DNSServiceNetworks {
		network.NATPoolAddress = poolAddresses[index]
		if err := verifyDNSServiceNetworkNAT(ctx, channel, network, plan.NAT.Behavior); err != nil {
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
// vpp.dns-service.network operation. VPP silently accepts an interface role
// whose input or output interface does not exist yet. The repair therefore
// proves the TAP and its underlay first (creating the TAP again when it is
// genuinely absent), and only then issues the role. When an interface never
// appears it returns a precise error naming the missing side instead of
// reporting an empty NAT44 table as a role failure.
func ensureDNSServiceNetworkNAT(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	underlay, err := resolveDNSServiceNetworkUnderlay(network)
	if err != nil {
		return err
	}
	if underlay == "" {
		return fmt.Errorf("DNS service interface %q has no underlay for its NAT44 input role", network.VPPInterface)
	}
	// A clean boot has no pre-existing TAP. The operation normally creates it,
	// but an optional create/rename can be ignored while VPP is still settling.
	// Repair it once, idempotently, before declaring it missing.
	present, _ := dnsServiceNetworkInterfacePresence(ctx, channel)
	if !present[network.VPPInterface] {
		if tapErr := ensureDNSServiceNetworkTAP(ctx, channel, network); tapErr != nil {
			return fmt.Errorf("DNS service interface %q TAP repair failed: %w", network.VPPInterface, tapErr)
		}
	}
	tapPresent, underlayPresent, observed, waitErr := waitDNSServiceNetworkInterfaces(ctx, channel, network.VPPInterface, underlay)
	if !tapPresent || !underlayPresent {
		return &DNSServiceNetworkNATError{
			UpstreamID:      network.UpstreamID,
			VPPInterface:    network.VPPInterface,
			Underlay:        underlay,
			MissingTAP:      !tapPresent,
			MissingUnderlay: !underlayPresent,
			Observed:        observed,
			Reason:          interfaceWaitReason(waitErr),
		}
	}
	// The pool is tenant-scoped. Install/reconcile it before the role so a
	// readback can never observe a role that points at an untranslatable VRF.
	if err := ensureDNSServiceNetworkNATPool(ctx, channel, network, behavior); err != nil {
		return err
	}
	if natRolePresentFor(ctx, channel, network.VPPInterface, behavior) {
		return waitDNSServiceNetworkNATPool(ctx, channel, network, behavior)
	}
	// The role command is idempotent (delete then add). Retry a bounded number
	// of times because a VPP worker that is still attaching the interface can
	// accept the CLI without recording the role.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := attachDNSServiceNetworkNATRole(ctx, channel, network, underlay, behavior); err != nil {
			return fmt.Errorf("DNS service interface %q NAT44 input role attach failed on underlay %q: %w", network.VPPInterface, underlay, err)
		}
		if err := waitDNSServiceNetworkNAT(ctx, channel, network, behavior); err == nil {
			if poolErr := waitDNSServiceNetworkNATPool(ctx, channel, network, behavior); poolErr != nil {
				return poolErr
			}
			return nil
		} else {
			lastErr = err
		}
		// Re-check the interfaces: VPP may have restarted between the attach
		// and the readback. The error must still name the missing side.
		presentNow, observedNow := dnsServiceNetworkInterfaceState(ctx, channel)
		tapNow := presentNow[strings.TrimSpace(network.VPPInterface)]
		underlayNow := presentNow[strings.TrimSpace(underlay)]
		if !tapNow || !underlayNow {
			return &DNSServiceNetworkNATError{
				UpstreamID:      network.UpstreamID,
				VPPInterface:    network.VPPInterface,
				Underlay:        underlay,
				MissingTAP:      !tapNow,
				MissingUnderlay: !underlayNow,
				Observed:        observedNow,
				Reason:          lastErr.Error(),
			}
		}
	}
	return &DNSServiceNetworkNATError{
		UpstreamID:   network.UpstreamID,
		VPPInterface: network.VPPInterface,
		Underlay:     underlay,
		Observed:     natReadbackOutput(lastErr),
		Reason:       lastErr.Error(),
	}
}

func resolveDNSServiceNetworkUnderlay(network DNSServiceNetwork) (string, error) {
	underlay := dnsServiceNetworkUnderlayInterface(network)
	if !strings.HasPrefix(strings.TrimSpace(underlay), "pppoe-runtime:") {
		return underlay, nil
	}
	resolved, err := resolveRuntimePPPoEInterface(underlay)
	if err != nil {
		return "", fmt.Errorf("DNS service interface %q cannot attach NAT44: %w", network.VPPInterface, err)
	}
	return resolved, nil
}

func attachDNSServiceNetworkNATRole(ctx context.Context, channel Channel, network DNSServiceNetwork, underlay string, behavior nat.Behavior) error {
	commands := []string{}
	if behavior == nat.BehaviorFullCone {
		commands = append(commands,
			fmt.Sprintf("?set interface nat44 ei in %s out %s output-feature del", network.VPPInterface, underlay),
			fmt.Sprintf("?set interface nat44 ei in %s out %s", network.VPPInterface, underlay),
			fmt.Sprintf("?nat44 ei add interface address %s", underlay),
		)
	} else {
		commands = append(commands,
			fmt.Sprintf("?set interface nat44 in %s out %s output-feature del", network.VPPInterface, underlay),
			fmt.Sprintf("?set interface nat44 in %s out %s", network.VPPInterface, underlay),
		)
	}
	if _, err := doOperation(ctx, channel, Operation{
		Name:           "vpp.dns-service.network.nat44-role",
		RequestID:      network.UpstreamID,
		Resource:       network.UpstreamID,
		Payload:        network,
		VPPCtlCommands: commands,
	}); err != nil {
		return err
	}
	return nil
}

// ensureDNSServiceNetworkTAP re-runs the idempotent TAP create/address subset.
// It deliberately does not touch NAT44: the caller proves both interfaces
// before issuing a role.
func ensureDNSServiceNetworkTAP(ctx context.Context, channel Channel, network DNSServiceNetwork) error {
	commands := dnsServiceNetworkTAPCommands(network)
	if len(commands) == 0 {
		return nil
	}
	_, err := doOperation(ctx, channel, Operation{
		Name:           "vpp.dns-service.network.tap-repair",
		RequestID:      network.UpstreamID,
		Resource:       network.UpstreamID,
		Payload:        network,
		VPPCtlCommands: commands,
	})
	return err
}

func dnsServiceNetworkInterfacePresence(ctx context.Context, channel Channel) (map[string]bool, string) {
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.interfaces", VPPCtlCommands: []string{"show interface"}})
	if err != nil {
		return map[string]bool{}, err.Error()
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok {
		return map[string]bool{}, "show interface returned no typed payload"
	}
	for _, result := range payload.CommandResults {
		if result.Command == "show interface" {
			return parseVPPInterfaceNames(result.Stdout), strings.TrimSpace(result.Stdout)
		}
	}
	return map[string]bool{}, ""
}

func parseVPPInterfaceNames(output string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "unknown input") || strings.Contains(trimmed, "unknown interface") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "Name" {
			continue
		}
		names[fields[0]] = true
	}
	return names
}

func dnsServiceNetworkInterfaceState(ctx context.Context, channel Channel) (map[string]bool, string) {
	return dnsServiceNetworkInterfacePresence(ctx, channel)
}

func waitDNSServiceNetworkInterfaces(ctx context.Context, channel Channel, vppInterface, underlay string) (bool, bool, string, error) {
	attempts, interval := dnsServiceInterfaceSettings()
	var observed string
	for attempt := 1; attempt <= attempts; attempt++ {
		present, output := dnsServiceNetworkInterfacePresence(ctx, channel)
		observed = output
		tapPresent := present[strings.TrimSpace(vppInterface)]
		underlayPresent := present[strings.TrimSpace(underlay)]
		if tapPresent && underlayPresent {
			return true, true, observed, nil
		}
		if attempt == attempts {
			return tapPresent, underlayPresent, observed, fmt.Errorf("interfaces did not appear within %s", time.Duration(attempts)*interval)
		}
		select {
		case <-ctx.Done():
			return tapPresent, underlayPresent, observed, ctx.Err()
		case <-time.After(interval):
		}
	}
	return false, false, observed, fmt.Errorf("interface readiness is unavailable")
}

func verifyDNSServiceNetworkNAT(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	underlay, err := resolveDNSServiceNetworkUnderlay(network)
	if err != nil {
		return err
	}
	present, observed := dnsServiceNetworkInterfaceState(ctx, channel)
	tapPresent := present[strings.TrimSpace(network.VPPInterface)]
	underlayPresent := present[strings.TrimSpace(underlay)]
	if !tapPresent || !underlayPresent {
		return &DNSServiceNetworkNATError{
			UpstreamID:      network.UpstreamID,
			VPPInterface:    network.VPPInterface,
			Underlay:        underlay,
			MissingTAP:      !tapPresent,
			MissingUnderlay: !underlayPresent,
			Observed:        observed,
			Reason:          "interface absent before the NAT44 role readback",
		}
	}
	if err := waitDNSServiceNetworkNAT(ctx, channel, network, behavior); err != nil {
		return &DNSServiceNetworkNATError{
			UpstreamID:   network.UpstreamID,
			VPPInterface: network.VPPInterface,
			Underlay:     underlay,
			Observed:     natReadbackOutput(err),
			Reason:       err.Error(),
		}
	}
	// Role presence alone is not proof of a data path. An inside interface in
	// tenant VRF N with no pool address in N reads exactly like a healthy role,
	// but VPP drops every new flow with "out of ports".
	return waitDNSServiceNetworkNATPool(ctx, channel, network, behavior)
}

// ensureDNSServiceNetworkNATPool makes the service VRF's pool match the plan.
// It removes any other pool address owned by this tenant VRF before adding the
// desired entry, so a WAN address change cannot leave a stale translation
// source. Other VRFs (including the global LAN->WAN pool) are untouched.
func ensureDNSServiceNetworkNATPool(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	address := strings.TrimSpace(network.NATPoolAddress)
	if address == "" {
		return &DNSServiceNetworkNATError{
			UpstreamID:   network.UpstreamID,
			VPPInterface: network.VPPInterface,
			PoolVRF:      network.TableID,
			MissingPool:  true,
			Reason:       "the plan did not allocate a NAT44 outside address for the service VRF",
		}
	}
	command := nat44PoolShowCommand(behavior)
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-pool-readback", RequestID: network.UpstreamID, VPPCtlCommands: []string{command}})
	if err != nil {
		return err
	}
	var observed string
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if ok {
		for _, result := range payload.CommandResults {
			if result.Command == command {
				observed = result.Stdout
			}
		}
	}
	commands := make([]string, 0, len(nat44PoolAddressesForVRF(observed, network.TableID))+1)
	for _, existing := range nat44PoolAddressesForVRF(observed, network.TableID) {
		if existing == address {
			continue
		}
		commands = append(commands, nat44PoolDeleteCommand(behavior, existing, network.TableID))
	}
	commands = append(commands, nat44PoolAddCommand(behavior, address, network.TableID))
	if _, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-pool", RequestID: network.UpstreamID, Resource: network.UpstreamID, Payload: network, VPPCtlCommands: commands}); err != nil {
		return err
	}
	return nil
}

func waitDNSServiceNetworkNATPool(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	attempts, interval := dnsNATRoleSettings()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := verifyDNSServiceNetworkNATPool(ctx, channel, network, behavior); err == nil {
			return nil
		} else {
			lastErr = err
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
	return lastErr
}

func verifyDNSServiceNetworkNATPool(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	address := strings.TrimSpace(network.NATPoolAddress)
	command := nat44PoolShowCommand(behavior)
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-pool-verify", RequestID: network.UpstreamID, VPPCtlCommands: []string{command}})
	if err != nil {
		return err
	}
	var observed string
	if payload, ok := reply.Payload.(VPPCTLReplyPayload); ok {
		for _, result := range payload.CommandResults {
			if result.Command == command {
				observed = result.Stdout
			}
		}
	}
	if address == "" || !nat44PoolHasAddressForVRF(observed, address, network.TableID) {
		return &DNSServiceNetworkNATError{
			UpstreamID:          network.UpstreamID,
			VPPInterface:        network.VPPInterface,
			ExpectedPoolAddress: address,
			PoolVRF:             network.TableID,
			MissingPool:         true,
			Observed:            observed,
			Reason:              fmt.Sprintf("%s has no %q entry for tenant VRF %d", command, address, network.TableID),
		}
	}
	if err := verifyDNSServiceNetworkVRF(ctx, channel, network); err != nil {
		return err
	}
	return nil
}

func verifyDNSServiceNetworkVRF(ctx context.Context, channel Channel, network DNSServiceNetwork) error {
	interfaceCommand := "show interface address " + network.VPPInterface
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.vrf-verify", RequestID: network.UpstreamID, VPPCtlCommands: []string{interfaceCommand}})
	if err != nil {
		return err
	}
	var observed string
	if payload, ok := reply.Payload.(VPPCTLReplyPayload); ok {
		for _, result := range payload.CommandResults {
			if result.Command == interfaceCommand {
				observed = result.Stdout
			}
		}
	}
	tableID, found := interfaceIPv4TableID(observed)
	if !found || tableID != network.TableID {
		return &DNSServiceNetworkNATError{
			UpstreamID:   network.UpstreamID,
			VPPInterface: network.VPPInterface,
			PoolVRF:      network.TableID,
			MissingVRF:   true,
			Observed:     observed,
			Reason:       fmt.Sprintf("interface address output does not report ip4 table-id %d", network.TableID),
		}
	}
	routeCommand := fmt.Sprintf("show ip fib table %d", network.TableID)
	routeReply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.route-verify", RequestID: network.UpstreamID, VPPCtlCommands: []string{routeCommand}})
	if err != nil {
		return err
	}
	var routeOutput string
	if payload, ok := routeReply.Payload.(VPPCTLReplyPayload); ok {
		for _, result := range payload.CommandResults {
			if result.Command == routeCommand {
				routeOutput = result.Stdout
			}
		}
	}
	if !strings.Contains(routeOutput, "0.0.0.0/0") {
		return &DNSServiceNetworkNATError{
			UpstreamID:   network.UpstreamID,
			VPPInterface: network.VPPInterface,
			PoolVRF:      network.TableID,
			MissingRoute: true,
			Observed:     routeOutput,
			Reason:       fmt.Sprintf("table %d has no default route for the service TAP", network.TableID),
		}
	}
	return nil
}

func interfaceIPv4TableID(output string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != "table-id" || index+1 >= len(fields) {
				continue
			}
			if tableID, err := strconv.Atoi(fields[index+1]); err == nil {
				return tableID, true
			}
		}
	}
	return 0, false
}

func natRolePresentFor(ctx context.Context, channel Channel, vppInterface string, behavior nat.Behavior) bool {
	command := dnsNATShowCommand(behavior)
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.dns-service.network.nat44-readback", VPPCtlCommands: []string{command}})
	if err != nil {
		return false
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok {
		return false
	}
	for _, result := range payload.CommandResults {
		if result.Command == command && natRolePresent(result.Stdout, vppInterface) {
			return true
		}
	}
	return false
}

func waitDNSServiceNetworkNAT(ctx context.Context, channel Channel, network DNSServiceNetwork, behavior nat.Behavior) error {
	command := dnsNATShowCommand(behavior)
	attempts, interval := dnsNATRoleSettings()
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

func natReadbackOutput(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if index := strings.LastIndex(message, "("); index >= 0 && strings.HasSuffix(message, ")") {
		return strings.TrimSpace(message[index+1 : len(message)-1])
	}
	return message
}

func interfaceWaitReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func dnsServiceInterfaceSettings() (int, time.Duration) {
	attempts := defaultDNSServiceInterfaceAttempts
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_INTERFACE_ATTEMPTS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attempts = parsed
		}
	}
	interval := defaultDNSServiceInterfaceInterval
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_INTERFACE_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	return attempts, interval
}

func dnsNATRoleSettings() (int, time.Duration) {
	attempts := defaultDNSServiceNATAttempts
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_NAT_ROLE_ATTEMPTS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attempts = parsed
		}
	}
	interval := defaultDNSServiceNATInterval
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_NAT_ROLE_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	return attempts, interval
}
