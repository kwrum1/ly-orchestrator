package httpapi

import (
	"net/netip"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/vpp"
)

func proxySNATTestAssignments() []vpp.AddressAssignment {
	return []vpp.AddressAssignment{
		{ID: "lan", VPPInterface: "lyroute-enp3s0", CIDR: "192.168.88.1/24", Role: "lan"},
		{ID: "wan", VPPInterface: "lyroute-enp4s0", CIDR: "10.1.18.24/24", Role: "wan"},
	}
}

func TestAllocateProxySNATAddressPrefersWANInterfaceAddress(t *testing.T) {
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "proxy-media",
		Assignments:   proxySNATTestAssignments(),
		UnderlayRoute: "10.1.18.1 lyroute-enp4s0",
		// The historical bug selected 10.1.18.100, a real host on the same
		// layer-2 segment. The interface-address handoff must not depend on a
		// subnet-wide hash at all.
		ExcludedAddresses: []string{"10.1.18.100"},
		StaticMappings:    []nat.StaticMapping{{ID: "host", ExternalAddress: "10.1.18.35"}},
	})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if allocation.Address != "10.1.18.24" {
		t.Fatalf("SNAT address = %q, want the WAN interface address 10.1.18.24", allocation.Address)
	}
	if allocation.Source != proxySNATSourceWANInterface {
		t.Fatalf("SNAT source = %q, want %q", allocation.Source, proxySNATSourceWANInterface)
	}
	if allocation.WANInterface != "lyroute-enp4s0" || allocation.WANPrefix != "10.1.18.0/24" {
		t.Fatalf("allocation metadata = %#v", allocation)
	}
}

func TestAllocateProxySNATAddressDoesNotHashTheWholeSubnet(t *testing.T) {
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "proxy-media",
		Assignments:   proxySNATTestAssignments(),
		UnderlayRoute: "10.1.18.1 lyroute-enp4s0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Address == "10.1.18.100" {
		t.Fatal("allocator selected the known conflicting host 10.1.18.100")
	}
	// The same input must be deterministic, but it must come from the WAN
	// interface rather than a digest-based offset.
	second, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "another-egress-id",
		Assignments:   proxySNATTestAssignments(),
		UnderlayRoute: "10.1.18.1 lyroute-enp4s0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Address != allocation.Address {
		t.Fatalf("two egresses on the same WAN got %q and %q; interface address must be shared", allocation.Address, second.Address)
	}
}

func TestAllocateProxySNATAddressUsesExplicitReservedPoolInOrder(t *testing.T) {
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.60", "10.1.18.61"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Address != "10.1.18.60" || allocation.Source != proxySNATSourceReservedPool {
		t.Fatalf("allocation = %#v, want first explicit address 10.1.18.60", allocation)
	}
}

func TestAllocateProxySNATAddressSkipsReservedAndNeighbourAddressesFromExplicitPool(t *testing.T) {
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "proxy-media",
		Assignments:   proxySNATTestAssignments(),
		UnderlayRoute: "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{
			"10.1.18.1",   // underlay gateway
			"10.1.18.24",  // WAN interface
			"10.1.18.35",  // static mapping
			"10.1.18.100", // neighbour conflict, discovered by ARP
			"10.1.18.255", // broadcast
			"10.1.18.90",  // first genuinely usable address
		},
		ExcludedAddresses: []string{"10.1.18.100"},
		StaticMappings:    []nat.StaticMapping{{ID: "host", ExternalAddress: "10.1.18.35"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Address != "10.1.18.90" {
		t.Fatalf("SNAT address = %q, want the first usable explicit address 10.1.18.90", allocation.Address)
	}
}

func TestAllocateProxySNATAddressRejectsKnownHostConflict(t *testing.T) {
	_, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.100"},
		ExcludedAddresses: []string{"10.1.18.100"},
	})
	if err == nil {
		t.Fatal("allocator accepted 10.1.18.100 even though it is excluded")
	}
	if !strings.Contains(err.Error(), "10.1.18.100") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want an explicit 10.1.18.100 conflict reason", err)
	}
}

func TestAllocateProxySNATAddressRejectsExhaustedExplicitPool(t *testing.T) {
	_, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       []vpp.AddressAssignment{{ID: "wan", VPPInterface: "lyroute-enp4s0", CIDR: "10.1.18.25/30", Role: "wan"}},
		UnderlayRoute:     "10.1.18.25 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.24", "10.1.18.25", "10.1.18.27"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want explicit exhaustion", err)
	}
}

func TestAllocateProxySNATAddressDeduplicatesAcrossProxyEgresses(t *testing.T) {
	first, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "egress-a",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.70"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "egress-b",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.70"},
		ExcludedAddresses: []string{first.Address},
	})
	if err == nil {
		t.Fatalf("second egress reused %q; want explicit exhaustion", second.Address)
	}
	if !strings.Contains(err.Error(), "10.1.18.70") {
		t.Fatalf("error = %v, want the duplicate address in the reason", err)
	}
}

func TestAllocateProxySNATAddressValidatesExplicitPoolMembership(t *testing.T) {
	for _, candidate := range []string{"10.1.18.0", "10.1.18.255", "10.1.18.1", "10.1.18.24", "192.0.2.10", "not-an-ip"} {
		_, err := allocateProxySNATAddress(proxySNATRequest{
			EgressID:          "proxy-media",
			Assignments:       proxySNATTestAssignments(),
			UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
			ReservedAddresses: []string{candidate},
		})
		if err == nil {
			t.Fatalf("allocator accepted explicit pool address %q", candidate)
		}
		if strings.TrimSpace(candidate) != "" && !strings.Contains(err.Error(), candidate) && candidate != "10.1.18.1" {
			// The interface/gateway entries can be described by role rather
			// than repeated verbatim.
			t.Logf("error for %q: %v", candidate, err)
		}
	}
}

func TestAllocateProxySNATAddressKeepsLegacyHandoffForDynamicUnderlays(t *testing.T) {
	for _, underlay := range []string{"ip4-lookup-in-table 70099", "pppoe-runtime:wan-primary", "pppox-wan", "lyppp-wan"} {
		allocation, err := allocateProxySNATAddress(proxySNATRequest{
			EgressID:      "proxy-media",
			Assignments:   proxySNATTestAssignments(),
			UnderlayRoute: underlay,
		})
		if err != nil {
			t.Fatalf("underlay %q: %v", underlay, err)
		}
		if allocation.Address != "" || allocation.Source != proxySNATSourceLegacyGlobal {
			t.Fatalf("underlay %q allocation = %#v, want legacy global handoff", underlay, allocation)
		}
	}
}

func TestAllocateProxySNATAddressRejectsExplicitPoolWithoutStaticWAN(t *testing.T) {
	_, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "ip4-lookup-in-table 70099",
		ReservedAddresses: []string{"10.1.18.60"},
	})
	if err == nil || !strings.Contains(err.Error(), "static IPv4 WAN") {
		t.Fatalf("error = %v, want an explicit static-WAN requirement", err)
	}
}

func TestAllocateProxySNATAddressHandlesTinyPrefix(t *testing.T) {
	// A /31 still has a usable interface address even though it cannot provide
	// a spare address for an explicit pool.
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "proxy-media",
		Assignments:   []vpp.AddressAssignment{{ID: "wan", VPPInterface: "lyroute-enp4s0", CIDR: "10.1.18.24/31", Role: "wan"}},
		UnderlayRoute: "10.1.18.25 lyroute-enp4s0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Address != "10.1.18.24" {
		t.Fatalf("SNAT address = %q, want 10.1.18.24", allocation.Address)
	}
	if _, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       []vpp.AddressAssignment{{ID: "wan", VPPInterface: "lyroute-enp4s0", CIDR: "10.1.18.24/31", Role: "wan"}},
		UnderlayRoute:     "10.1.18.25 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.25"},
	}); err == nil || !strings.Contains(err.Error(), "spare") {
		t.Fatalf("error = %v, want a usable-spare-range rejection", err)
	}
}

func TestAllocateProxySNATAddressReportsInvalidStaticPrefix(t *testing.T) {
	_, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      "proxy-media",
		Assignments:   []vpp.AddressAssignment{{ID: "wan", VPPInterface: "lyroute-enp4s0", CIDR: "not-a-prefix", Role: "wan"}},
		UnderlayRoute: "10.1.18.1 lyroute-enp4s0",
	})
	if err == nil || !strings.Contains(err.Error(), "static IPv4 prefix") {
		t.Fatalf("error = %v, want an explicit invalid-prefix reason", err)
	}
}

func TestAllocateProxySNATAddressRejectsPortMappingConflict(t *testing.T) {
	_, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.40"},
		PortMappings:      []nat.PortMapping{{ID: "web", ExternalAddress: "10.1.18.40", ExternalPort: 443}},
	})
	if err == nil || !strings.Contains(err.Error(), "10.1.18.40") {
		t.Fatalf("error = %v, want the port-mapping conflict", err)
	}
}

func TestAllocateProxySNATAddressParsesViaDevAndTableUnderlay(t *testing.T) {
	for _, underlay := range []string{
		"10.1.18.1 via lyroute-enp4s0 table 100",
		"10.1.18.1 dev lyroute-enp4s0",
		"10.1.18.1 lyroute-enp4s0",
		"lyroute-enp4s0",
	} {
		allocation, err := allocateProxySNATAddress(proxySNATRequest{
			EgressID:      "proxy-media",
			Assignments:   proxySNATTestAssignments(),
			UnderlayRoute: underlay,
		})
		if err != nil {
			t.Fatalf("underlay %q: %v", underlay, err)
		}
		if allocation.Address != "10.1.18.24" || allocation.Source != proxySNATSourceWANInterface {
			t.Fatalf("underlay %q allocation = %#v, want interface address", underlay, allocation)
		}
	}
}

func TestAllocateProxySNATAddressRejectsUnrecognizedStaticUnderlay(t *testing.T) {
	for _, underlay := range []string{
		"table 100",
		"10.1.18.1 table 100",
		"10.1.18.1 lyroute-enp4s0 extra",
		"10.1.18.1 via lyroute-enp4s0 table",
	} {
		_, err := allocateProxySNATAddress(proxySNATRequest{
			EgressID:      "proxy-media",
			Assignments:   proxySNATTestAssignments(),
			UnderlayRoute: underlay,
		})
		if err == nil {
			t.Fatalf("underlay %q silently fell back to legacy", underlay)
		}
		if !strings.Contains(err.Error(), "underlay route") {
			t.Fatalf("underlay %q error = %v, want a route-specific reason", underlay, err)
		}
	}
}

func TestProxySNATAllocationStaysInsideWANPrefix(t *testing.T) {
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:          "proxy-media",
		Assignments:       proxySNATTestAssignments(),
		UnderlayRoute:     "10.1.18.1 lyroute-enp4s0",
		ReservedAddresses: []string{"10.1.18.60"},
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, parseErr := netip.ParseAddr(allocation.Address)
	if parseErr != nil || !netip.MustParsePrefix("10.1.18.0/24").Contains(parsed) {
		t.Fatalf("allocated address %q is outside the WAN subnet", allocation.Address)
	}
}

func TestValidateProxySNATAddressListRejectsMalformedAndDuplicateEntries(t *testing.T) {
	if err := validateProxySNATAddressList([]string{"10.1.18.60", "10.1.18.61"}); err != nil {
		t.Fatalf("valid pool rejected: %v", err)
	}
	for _, values := range [][]string{
		{"10.1.18.60", "not-an-ip"},
		{"2001:db8::1"},
		{"10.1.18.60", "10.1.18.60"},
	} {
		if err := validateProxySNATAddressList(values); err == nil {
			t.Fatalf("invalid SNAT pool %#v was accepted", values)
		}
	}
}

func TestNormalizeProxySNATAddressesPreservesOperatorOrder(t *testing.T) {
	got := normalizeProxySNATAddresses([]string{" 10.1.18.61 ", "", "10.1.18.60"})
	if len(got) != 2 || got[0] != "10.1.18.61" || got[1] != "10.1.18.60" {
		t.Fatalf("normalized pool = %#v, want order-preserving trim", got)
	}
}
