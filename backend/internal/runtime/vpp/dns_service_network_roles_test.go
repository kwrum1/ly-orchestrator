package vpp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

// fakeDNSServiceVPP models the small part of the VPP CLI state machine that
// matters for the DNS service-network handoff. It deliberately accepts a
// NAT44 role for a missing interface (as VPP does) instead of returning an
// error, so the product must prove interface presence itself.
type fakeDNSServiceVPP struct {
	interfaces    map[string]bool
	taps          map[string]bool
	tables        map[int]bool
	ifTables      map[string]int
	natPools      map[string]int
	natEIPools    map[string]int
	natRoles      map[string]string
	natEIRoles    map[string]string
	commands      []string
	ignoreRole    bool
	failTAPCreate bool
	down          bool
}

func newFakeDNSServiceVPP(interfaces ...string) *fakeDNSServiceVPP {
	fake := &fakeDNSServiceVPP{
		interfaces: map[string]bool{"local0": true},
		taps:       map[string]bool{},
		tables:     map[int]bool{},
		ifTables:   map[string]int{},
		natPools:   map[string]int{},
		natEIPools: map[string]int{},
		natRoles:   map[string]string{},
		natEIRoles: map[string]string{},
	}
	for _, name := range interfaces {
		fake.interfaces[name] = true
	}
	return fake
}

type fakeDNSServiceClient struct{ channel *fakeDNSServiceVPP }

func (client fakeDNSServiceClient) OpenChannel(context.Context) (Channel, error) {
	return client.channel, nil
}

func (fake *fakeDNSServiceVPP) Close() error { return nil }

func (fake *fakeDNSServiceVPP) Do(_ context.Context, operation Operation) (Reply, error) {
	results := make([]VPPCTLCommandResult, 0, len(operation.VPPCtlCommands))
	for _, raw := range operation.VPPCtlCommands {
		optional := strings.HasPrefix(strings.TrimSpace(raw), "?")
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		fake.commands = append(fake.commands, command)
		stdout, retval, err := fake.exec(command)
		results = append(results, VPPCTLCommandResult{Command: command, Stdout: stdout, Retval: retval})
		if err != nil && !optional {
			return Reply{Operation: operation.Name, Retval: retval, Payload: VPPCTLReplyPayload{CommandResults: results}}, err
		}
	}
	return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}, nil
}

func (fake *fakeDNSServiceVPP) exec(command string) (string, int32, error) {
	if fake.down && command != "show interface" {
		return "", 1, errors.New("connect: Connection refused")
	}
	fields := strings.Fields(command)
	switch {
	case command == "show interface":
		if fake.down {
			return "", 1, errors.New("connect: Connection refused")
		}
		names := make([]string, 0, len(fake.interfaces))
		for name := range fake.interfaces {
			names = append(names, name)
		}
		sort.Strings(names)
		return strings.Join(names, "\n") + "\n", 0, nil
	case command == "show tap":
		names := make([]string, 0, len(fake.taps))
		for name := range fake.taps {
			names = append(names, "Interface: "+name+" (ifindex 1)\n  name \""+name+"\"")
		}
		sort.Strings(names)
		return strings.Join(names, "\n"), 0, nil
	case command == "show nat44 interfaces" || command == "show nat44 ei interfaces":
		roles := fake.natRoles
		if command == "show nat44 ei interfaces" {
			roles = fake.natEIRoles
		}
		lines := []string{"NAT44 interfaces:"}
		inside := make([]string, 0, len(roles))
		for name := range roles {
			inside = append(inside, name)
		}
		sort.Strings(inside)
		for _, name := range inside {
			lines = append(lines, " "+name+" in")
		}
		outside := map[string]bool{}
		for _, name := range inside {
			outside[roles[name]] = true
		}
		wan := make([]string, 0, len(outside))
		for name := range outside {
			wan = append(wan, name)
		}
		sort.Strings(wan)
		for _, name := range wan {
			lines = append(lines, " "+name+" out")
		}
		return strings.Join(lines, "\n") + "\n", 0, nil
	case strings.HasPrefix(command, "show interface address "):
		name := strings.TrimSpace(strings.TrimPrefix(command, "show interface address "))
		if !fake.interfaces[name] {
			return "unknown interface " + name, 1, fmt.Errorf("unknown interface %s", name)
		}
		if tableID := fake.ifTables[name]; tableID != 0 {
			return name + " (up):\n  L3 198.19.0.1/30 ip4 table-id " + strconv.Itoa(tableID) + " fib-idx 1\n", 0, nil
		}
		return name + " (up):\n  L3 198.19.0.1/30\n", 0, nil
	case command == "show nat44 addresses" || command == "show nat44 ei addresses":
		pool := fake.natPools
		if command == "show nat44 ei addresses" {
			pool = fake.natEIPools
		}
		lines := []string{"NAT44 pool addresses:"}
		addresses := make([]string, 0, len(pool))
		for address := range pool {
			addresses = append(addresses, address)
		}
		sort.Strings(addresses)
		for _, address := range addresses {
			lines = append(lines, address, "  tenant VRF: "+strconv.Itoa(pool[address]))
		}
		lines = append(lines, "NAT44 twice-nat pool addresses:")
		return strings.Join(lines, "\n") + "\n", 0, nil
	case strings.HasPrefix(command, "nat44 add address ") || strings.HasPrefix(command, "nat44 ei add address "):
		return fake.execNATPool(command)
	case strings.HasPrefix(command, "set interface ip table "):
		if len(fields) < 6 {
			return "", 1, errors.New("invalid set interface ip table")
		}
		name := fields[4]
		tableID, err := strconv.Atoi(fields[5])
		if err != nil {
			return "", 1, err
		}
		fake.ifTables[name] = tableID
		fake.tables[tableID] = true
		return "", 0, nil
	case strings.HasPrefix(command, "show ip fib table "):
		tableID := strings.TrimSpace(strings.TrimPrefix(command, "show ip fib table "))
		return "ipv4-VRF:" + tableID + ", fib_index:0\n0.0.0.0/0\n", 0, nil
	case strings.HasPrefix(command, "create tap id "):
		if fake.failTAPCreate {
			return "tap create failed", 1, errors.New("tap create failed")
		}
		tapID := fields[3]
		fake.interfaces["tap"+tapID] = true
		fake.taps["tap"+tapID] = true
		return "", 0, nil
	case strings.HasPrefix(command, "set interface name "):
		from := fields[3]
		to := fields[4]
		if !fake.interfaces[from] {
			return "unknown interface " + from, 1, fmt.Errorf("unknown interface %s", from)
		}
		delete(fake.interfaces, from)
		delete(fake.taps, from)
		fake.interfaces[to] = true
		if fake.taps[from] || strings.HasPrefix(from, "tap") {
			fake.taps[to] = true
		}
		return "", 0, nil
	case strings.HasPrefix(command, "delete tap "):
		name := fields[2]
		delete(fake.interfaces, name)
		delete(fake.taps, name)
		delete(fake.natRoles, name)
		delete(fake.natEIRoles, name)
		return "", 0, nil
	case strings.HasPrefix(command, "ip table add "):
		return "", 0, nil
	case strings.HasPrefix(command, "ip table del "):
		return "", 0, nil
	case strings.HasPrefix(command, "ip route "):
		return "", 0, nil
	case strings.HasPrefix(command, "set interface ip "):
		return "", 0, nil
	case strings.HasPrefix(command, "nat44 plugin enable"), strings.HasPrefix(command, "nat44 ei plugin disable"), strings.HasPrefix(command, "nat44 add interface address"), strings.HasPrefix(command, "nat44 ei add interface address"):
		return "", 0, nil
	case strings.HasPrefix(command, "set interface nat44 ei in "):
		return fake.applyNATRole(command, fake.natEIRoles)
	case strings.HasPrefix(command, "set interface nat44 in "):
		return fake.applyNATRole(command, fake.natRoles)
	case strings.HasPrefix(command, "set interface state "), strings.HasPrefix(command, "set interface mtu "):
		name := fields[len(fields)-1]
		if !fake.interfaces[name] {
			return "unknown interface " + name, 1, fmt.Errorf("unknown interface %s", name)
		}
		return "", 0, nil
	}
	return "", 0, nil
}

func (fake *fakeDNSServiceVPP) execNATPool(command string) (string, int32, error) {
	fields := strings.Fields(command)
	// nat44 [ei] add address <addr> tenant-vrf <vrf> [del]
	address := ""
	tableID := 0
	deleteAddress := false
	for index, field := range fields {
		if index > 0 && field == "address" && index+1 < len(fields) {
			address = fields[index+1]
		}
		if field == "tenant-vrf" && index+1 < len(fields) {
			parsed, err := strconv.Atoi(fields[index+1])
			if err != nil {
				return "", 1, err
			}
			tableID = parsed
		}
		if field == "del" {
			deleteAddress = true
		}
	}
	pool := fake.natPools
	if strings.HasPrefix(command, "nat44 ei ") {
		pool = fake.natEIPools
	}
	if address == "" {
		return "", 1, errors.New("NAT44 pool address is required")
	}
	if deleteAddress {
		delete(pool, address)
		return "", 0, nil
	}
	if _, exists := pool[address]; exists {
		return "NAT address already in use", 1, errors.New("NAT address already in use")
	}
	pool[address] = tableID
	return "", 0, nil
}

func (fake *fakeDNSServiceVPP) applyNATRole(command string, roles map[string]string) (string, int32, error) {
	fields := strings.Fields(command)
	inside := ""
	outside := ""
	deleteRole := false
	for index, field := range fields {
		switch field {
		case "in":
			if index+1 < len(fields) {
				inside = fields[index+1]
			}
		case "out":
			if index+1 < len(fields) {
				outside = fields[index+1]
			}
		case "del":
			deleteRole = true
		}
	}
	if inside == "" {
		return "", 1, errors.New("inside interface is required")
	}
	if deleteRole {
		delete(roles, inside)
		return "", 0, nil
	}
	// VPP accepts an interface role it cannot resolve and simply records
	// nothing. That silent acceptance is exactly what the pre-role interface
	// proof protects against.
	if !fake.interfaces[inside] || (outside != "" && !fake.interfaces[outside]) {
		return "", 0, nil
	}
	if fake.ignoreRole {
		return "", 0, nil
	}
	roles[inside] = outside
	return "", 0, nil
}

func dnsRoleFixture(t *testing.T) DNSServiceNetwork {
	t.Helper()
	network, err := DNSServiceNetworkForUpstreamID("dns-clean-boot", "wan-primary", []string{"9.9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := BindDNSServiceNetwork(&network, "192.0.2.1 lyroute-eth2"); err != nil {
		t.Fatal(err)
	}
	network.NATPoolAddress = "192.0.2.2"
	return network
}

func dnsRoleWANAssignment() AddressAssignment {
	return AddressAssignment{ID: "wan-eth2", LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", CIDR: "192.0.2.5/24", Role: "wan"}
}

func fastDNSRoleTiming(t *testing.T) {
	t.Helper()
	t.Setenv("LY_ROUTE_DNS_INTERFACE_ATTEMPTS", "3")
	t.Setenv("LY_ROUTE_DNS_INTERFACE_INTERVAL", "1ms")
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_ATTEMPTS", "2")
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_INTERVAL", "1ms")
}

func TestEnsureDNSServiceNetworkNATRepairsMissingInputRole(t *testing.T) {
	fastDNSRoleTiming(t)
	network := dnsRoleFixture(t)
	fake := newFakeDNSServiceVPP(network.VPPInterface, "lyroute-eth2")
	fake.ifTables[network.VPPInterface] = network.TableID
	if err := ensureDNSServiceNetworkNAT(context.Background(), fake, network, nat.BehaviorEndpointDependent); err != nil {
		t.Fatalf("NAT role repair failed: %v", err)
	}
	if got := fake.natRoles[network.VPPInterface]; got != "lyroute-eth2" {
		t.Fatalf("NAT44 role outside = %q, want lyroute-eth2 (commands: %v)", got, fake.commands)
	}
	if got := fake.natPools[network.NATPoolAddress]; got != network.TableID {
		t.Fatalf("NAT44 pool %q tenant VRF = %d, want %d", network.NATPoolAddress, got, network.TableID)
	}
}

func TestEnsureDNSServiceNetworkNATFailsWhenRoleIsNotAppliedButInterfacesExist(t *testing.T) {
	fastDNSRoleTiming(t)
	network := dnsRoleFixture(t)
	fake := newFakeDNSServiceVPP(network.VPPInterface, "lyroute-eth2")
	fake.ignoreRole = true
	err := ensureDNSServiceNetworkNAT(context.Background(), fake, network, nat.BehaviorEndpointDependent)
	if err == nil {
		t.Fatal("missing NAT role must fail closed")
	}
	for _, want := range []string{"NAT44 input role", network.VPPInterface, "lyroute-eth2", "both interfaces exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "missing in VPP") {
		t.Fatalf("role failure falsely claims an interface is missing: %v", err)
	}
}

func TestEnsureDNSServiceNetworkNATNamesMissingUnderlay(t *testing.T) {
	fastDNSRoleTiming(t)
	network := dnsRoleFixture(t)
	fake := newFakeDNSServiceVPP(network.VPPInterface)
	err := ensureDNSServiceNetworkNAT(context.Background(), fake, network, nat.BehaviorEndpointDependent)
	if err == nil {
		t.Fatal("missing underlay must fail closed")
	}
	for _, want := range []string{"underlay", "lyroute-eth2", "missing in VPP", "was not applied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "service TAP was not created") {
		t.Fatalf("underlay failure falsely blames the TAP: %v", err)
	}
	if len(fake.natRoles) != 0 {
		t.Fatalf("role was applied without an underlay: %#v", fake.natRoles)
	}
}

func TestEnsureDNSServiceNetworkNATNamesMissingTAP(t *testing.T) {
	fastDNSRoleTiming(t)
	network := dnsRoleFixture(t)
	fake := newFakeDNSServiceVPP("lyroute-eth2")
	fake.failTAPCreate = true
	err := ensureDNSServiceNetworkNAT(context.Background(), fake, network, nat.BehaviorEndpointDependent)
	if err == nil {
		t.Fatal("missing TAP must fail closed")
	}
	for _, want := range []string{network.VPPInterface, "service TAP was not created", "was not applied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "underlay") && strings.Contains(err.Error(), "underlay \"lyroute-eth2\" is missing") {
		t.Fatalf("TAP failure falsely blames the underlay: %v", err)
	}
	if len(fake.natRoles) != 0 {
		t.Fatalf("role was applied without a TAP: %#v", fake.natRoles)
	}
}

// TestDNSServiceNetworkCleanBootApplyAndRollback proves the Gate B handoff:
// a clean boot has no DNS TAP, the service-networks node creates it and its
// role, the readback passes, and rollback removes both the role and the TAP.
func TestDNSServiceNetworkCleanBootApplyAndRollback(t *testing.T) {
	fastDNSRoleTiming(t)
	network := dnsRoleFixture(t)
	fake := newFakeDNSServiceVPP("lyroute-eth2") // native interfaces node already ran
	adapter := Adapter{Client: fakeDNSServiceClient{channel: fake}}
	plan := provenPlan(Plan{
		RequestID:          "dns-clean-boot",
		AddressAssignments: []AddressAssignment{dnsRoleWANAssignment()},
		DNSServiceNetworks: []DNSServiceNetwork{network},
		NAT:                nat.CompiledConfig{Behavior: nat.BehaviorEndpointDependent},
	}, "eth2")

	readbacks, err := adapter.ApplySupplemental(context.Background(), plan, SupplementalServiceNetworks)
	if err != nil {
		t.Fatalf("clean-boot DNS service network apply failed: %v", err)
	}
	if len(readbacks) != 1 || readbacks[0].Resource != network.UpstreamID {
		t.Fatalf("readbacks = %#v", readbacks)
	}
	if !fake.interfaces[network.VPPInterface] || !fake.taps[network.VPPInterface] {
		t.Fatalf("TAP was not created on a clean boot: interfaces=%v taps=%v", fake.interfaces, fake.taps)
	}
	if got := fake.natRoles[network.VPPInterface]; got != "lyroute-eth2" {
		t.Fatalf("NAT44 input role = %q, want lyroute-eth2", got)
	}
	pools, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		t.Fatal(err)
	}
	servicePool := pools[0]
	if got := fake.natPools[servicePool]; got != network.TableID {
		t.Fatalf("service NAT44 pool %q tenant VRF = %d, want %d", servicePool, got, network.TableID)
	}
	if !strings.Contains(strings.Join(fake.commands, "\n"), "show nat44 interfaces") {
		t.Fatal("clean-boot apply did not read back show nat44 interfaces")
	}
	if !strings.Contains(strings.Join(fake.commands, "\n"), "show nat44 addresses") {
		t.Fatal("clean-boot apply did not read back show nat44 addresses")
	}

	cleanup, err := SupplementalReconciliationCleanupOperations(plan, Plan{RequestID: "dns-clean-boot-next"}, SupplementalServiceNetworks)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) == 0 {
		t.Fatal("rollback produced no DNS service-network cleanup operations")
	}
	if err := adapter.ExecuteOperations(context.Background(), cleanup); err != nil {
		t.Fatalf("DNS service network rollback failed: %v", err)
	}
	if fake.interfaces[network.VPPInterface] || fake.taps[network.VPPInterface] {
		t.Fatalf("rollback left the DNS TAP behind: interfaces=%v taps=%v", fake.interfaces, fake.taps)
	}
	if _, exists := fake.natRoles[network.VPPInterface]; exists {
		t.Fatalf("rollback left the NAT44 input role behind: %#v", fake.natRoles)
	}
	if strings.Contains(fake.showNAT(), network.VPPInterface+" in") {
		t.Fatalf("show nat44 interfaces still lists the DNS role: %s", fake.showNAT())
	}
	if _, exists := fake.natPools[servicePool]; exists {
		t.Fatalf("rollback left the service-VRF NAT44 pool address %q behind: %#v", servicePool, fake.natPools)
	}
}

func (fake *fakeDNSServiceVPP) showNAT() string {
	output, _, _ := fake.exec("show nat44 interfaces")
	return output
}
