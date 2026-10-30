package vpp

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

func poolTestPlan(network DNSServiceNetwork) Plan {
	return Plan{
		RequestID:          "dns-vrf-pool",
		AddressAssignments: []AddressAssignment{{ID: "wan-ens224", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", CIDR: "100.77.0.2/24", Role: "wan"}},
		DNSServiceNetworks: []DNSServiceNetwork{network},
		NAT:                nat.CompiledConfig{Behavior: nat.BehaviorEndpointDependent},
	}
}

func poolTestNetwork(t *testing.T, upstreamID string, tableID int) DNSServiceNetwork {
	t.Helper()
	network, err := DNSServiceNetworkForUpstreamID(upstreamID, "wan-primary", []string{"100.77.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	network.TableID = tableID
	if err := BindDNSServiceNetwork(&network, "100.77.0.1 lyroute-ens224"); err != nil {
		t.Fatal(err)
	}
	return network
}

func TestDNSServiceNetworkNATPoolAddressesAreTenantScopedAndSpare(t *testing.T) {
	first := poolTestNetwork(t, "dns-first", 57613)
	second := poolTestNetwork(t, "dns-second", 57614)
	plan := poolTestPlan(first)
	plan.DNSServiceNetworks = []DNSServiceNetwork{first, second}
	plan = provenPlan(plan, "ens224")

	pools, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("pool addresses = %#v, want two", pools)
	}
	if pools[0] == pools[1] {
		t.Fatalf("two service VRFs share NAT pool address %q", pools[0])
	}
	for _, address := range pools {
		if address == "100.77.0.1" || address == "100.77.0.2" || !strings.HasPrefix(address, "100.77.0.") {
			t.Fatalf("service pool address %q is not a spare WAN-subnet address", address)
		}
	}

	operations := mustBuildOperations(t, plan)
	assertOperationCommand(t, operations, "vpp.dns-service.network", "?nat44 add address "+pools[0]+" tenant-vrf "+strconv.Itoa(first.TableID))
	assertOperationCommand(t, operations, "vpp.dns-service.network", "?nat44 add address "+pools[1]+" tenant-vrf "+strconv.Itoa(second.TableID))
	// The global pool remains on the WAN interface address in VRF 0.
	assertOperationCommand(t, operations, "vpp.nat44.initialize", "?nat44 add address 100.77.0.2 tenant-vrf 0")
}

func TestDNSServiceNetworkNATPoolRequiresStaticWANAssignment(t *testing.T) {
	network := poolTestNetwork(t, "dns-dynamic", 57613)
	plan := Plan{RequestID: "dns-dynamic", DNSServiceNetworks: []DNSServiceNetwork{network}}
	_, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err == nil {
		t.Fatal("a service network without a static WAN prefix must fail closed")
	}
	if !strings.Contains(err.Error(), "static IPv4") && !strings.Contains(err.Error(), "cannot provision") {
		t.Fatalf("error does not explain the missing WAN prefix: %v", err)
	}
}

func TestDNSServiceNetworkNATPoolRejectsPreferredPrefixWithoutSpare(t *testing.T) {
	network := poolTestNetwork(t, "dns-narrow", 57613)
	plan := poolTestPlan(network)
	plan.AddressAssignments[0].CIDR = "100.77.0.2/30"
	_, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err == nil {
		t.Fatal("a /30 WAN with no spare address must fail closed")
	}
	if !strings.Contains(err.Error(), "spare") {
		t.Fatalf("error does not explain the exhausted prefix: %v", err)
	}
}

func TestParseNAT44PoolAddressesSeparatesTenantVRFs(t *testing.T) {
	output := `NAT44 pool addresses:
100.77.0.2
  tenant VRF: 0
  synced with interface address
100.77.0.3
  tenant VRF: 57613
NAT44 twice-nat pool addresses:
100.99.0.2
  tenant VRF: 57613
`
	entries := parseNAT44PoolAddresses(output)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two non-twice-nat rows", entries)
	}
	if !nat44PoolHasAddressForVRF(output, "100.77.0.3", 57613) {
		t.Fatal("service-VRF pool address was not recognised")
	}
	if nat44PoolHasAddressForVRF(output, "100.77.0.2", 57613) {
		t.Fatal("global pool address was falsely reported in the service VRF")
	}
	if got := nat44PoolAddressesForVRF(output, 57613); len(got) != 1 || got[0] != "100.77.0.3" {
		t.Fatalf("service-VRF addresses = %#v", got)
	}
}

func TestVerifyDNSServiceNetworksFailsWithoutServiceVRFPool(t *testing.T) {
	fastDNSRoleTiming(t)
	network := poolTestNetwork(t, "dns-verify", 57613)
	plan := poolTestPlan(network)
	pools, err := dnsServiceNetworkNATPoolAddresses(plan)
	if err != nil {
		t.Fatal(err)
	}
	pool := pools[0]

	fake := newFakeDNSServiceVPP(network.VPPInterface, "lyroute-ens224")
	fake.ifTables[network.VPPInterface] = network.TableID
	fake.natRoles[network.VPPInterface] = "lyroute-ens224"
	// Only the global pool exists, which is exactly the Gate A defect.
	fake.natPools["100.77.0.2"] = 0
	adapter := Adapter{Client: fakeDNSServiceClient{channel: fake}}
	if err := adapter.VerifyDNSServiceNetworks(context.Background(), plan); err == nil {
		t.Fatal("a service VRF without a NAT44 pool must fail verification")
	} else if !strings.Contains(err.Error(), "tenant VRF 57613") || !strings.Contains(err.Error(), pool) {
		t.Fatalf("verification error does not name the missing service-VRF pool: %v", err)
	}

	fake.natPools[pool] = network.TableID
	if err := adapter.VerifyDNSServiceNetworks(context.Background(), plan); err != nil {
		t.Fatalf("service VRF with its pool address did not verify: %v", err)
	}
}
