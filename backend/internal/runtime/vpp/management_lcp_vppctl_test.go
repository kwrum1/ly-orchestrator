package vpp

import (
	"reflect"
	"strings"
	"testing"
)

func TestManagementLCPPairs(t *testing.T) {
	output := `lcp default netns '<unset>'
itf-pair: [0] lyroute-eth0 tap4096 lymgmt0 2 type tap
itf-pair: [1] lyroute-eth1 tap4097 other0 3 type tap
`
	if got := managementLCPPairs(output, "lymgmt0"); !reflect.DeepEqual(got, []string{"lyroute-eth0"}) {
		t.Fatalf("management pairs = %#v", got)
	}
	if !managementLCPPresent(output, "lyroute-eth0", "lymgmt0") {
		t.Fatal("management pair was not found")
	}
}

func TestIPv4BroadcastLocalRoutePresent(t *testing.T) {
	if !ipv4BroadcastLocalRoutePresent("255.255.255.255/32\n  [@12]: dpo-receive: 0.0.0.0 on local0") {
		t.Fatal("limited broadcast local route was not recognized")
	}
	if ipv4BroadcastLocalRoutePresent("255.255.255.255/32\n  [@0]: dpo-drop ip4") {
		t.Fatal("drop route was accepted as a local route")
	}
}

func TestLANDHCPBroadcastPolicyReadbackUsesReceivePath(t *testing.T) {
	policy := lanDHCPBroadcastBypassPolicy("lyroute-ens192")
	if policy.PolicyID != lanDHCPBroadcastBypassPolicyID || policy.Priority != 1 || policy.NextHop != "local" {
		t.Fatalf("DHCP bypass policy = %#v", policy)
	}
	if policy.Match.DestinationIP != "255.255.255.255" || policy.Match.DestinationPort != 67 {
		t.Fatalf("DHCP bypass match = %#v", policy.Match)
	}
	output := `abf:[5]: policy:65000 acl:5
     path-list:[125] locks:1 flags:shared,no-uRPF, uRPF-list: None
      path:[148] pl-index:125 ip4 weight=1 pref=0 receive:  oper-flags:resolved, cfg-flags:local,
        [@0]: dpo-receive
`
	observed, err := parseObservedServiceChainABFPolicy(output)
	if err != nil {
		t.Fatalf("receive-path readback failed: %v", err)
	}
	if observed.ID != policy.PolicyID || observed.ACLID != 5 || observed.AddressFamily != "ip4" || observed.NextHop != "local" || observed.ServiceInterface != "" {
		t.Fatalf("receive-path readback = %#v", observed)
	}
	if !strings.Contains(serviceChainACLTag(policy), "lan_dhcp_broadcast") {
		t.Fatalf("unexpected DHCP bypass tag: %s", serviceChainACLTag(policy))
	}
	aclOutput := `acl-index 3 count 1 tag {ly-route-lan_dhcp_broadcast_forward_0}
          0: ipv4 permit src 0.0.0.0/32 dst 255.255.255.255/32 proto 17 sport 0-65535 dport 67
  applied inbound on sw_if_index:
  applied outbound on sw_if_index:
  used in lookup context index: 1
`
	acl, err := parseObservedServiceChainACL(aclOutput)
	if err != nil {
		t.Fatalf("VPP ACL attachment-state readback failed: %v", err)
	}
	if acl.ID != 3 || acl.Tag != serviceChainACLTag(policy) || acl.Match != policy.Match {
		t.Fatalf("VPP ACL attachment-state readback = %#v", acl)
	}
}

func TestLANDHCPBroadcastPolicyReadbackAcceptsLocalNoForwarding(t *testing.T) {
	output := `abf:[0]: policy:65000 acl:1
 no forwarding
`
	observed, err := parseObservedServiceChainABFPolicy(output)
	if err != nil {
		t.Fatalf("local no-forwarding readback failed: %v", err)
	}
	if observed.ID != 65000 || observed.ACLID != 1 || observed.AddressFamily != "ip4" || observed.NextHop != "local" || observed.ServiceInterface != "" {
		t.Fatalf("local no-forwarding readback = %#v", observed)
	}
}

func TestLANDNSABFPoliciesCoverLocalAndProxyDNS(t *testing.T) {
	policies := lanDNSABFPolicies(ManagementLCP{VPPInterface: "lyroute-ens256", LANAddress: "192.168.10.1", DNSSteering: true})
	if len(policies) != 4 {
		t.Fatalf("DNS ABF policy count = %d, want 4", len(policies))
	}
	seen := map[int]bool{}
	local, proxy := 0, 0
	for _, policy := range policies {
		if seen[policy.PolicyID] {
			t.Fatalf("duplicate DNS ABF policy id %d", policy.PolicyID)
		}
		seen[policy.PolicyID] = true
		if policy.PolicyID < 64000 || policy.PolicyID > 64399 {
			t.Fatalf("DNS ABF policy id %d outside reserved range", policy.PolicyID)
		}
		if policy.Match.DestinationPort != 53 || policy.IngressInterface != "lyroute-ens256" {
			t.Fatalf("unexpected DNS ABF policy match: %#v", policy)
		}
		switch policy.NextHop {
		case "local":
			local++
			if policy.Match.DestinationIP != "192.168.10.1" {
				t.Fatalf("local DNS policy destination = %q", policy.Match.DestinationIP)
			}
		case "ip4-lookup-in-table 100":
			proxy++
			if policy.Match.DestinationIP != "0.0.0.0/0" {
				t.Fatalf("proxy DNS policy destination = %q", policy.Match.DestinationIP)
			}
		default:
			t.Fatalf("unexpected DNS ABF next hop %q", policy.NextHop)
		}
	}
	if local != 2 || proxy != 2 {
		t.Fatalf("local/proxy DNS policies = %d/%d, want 2/2", local, proxy)
	}
}

// The health probe derives exactly the policy ids the apply path installs, and
// each LAN gets a disjoint family.
func TestLANDNSABFPolicyIDsArePerLAN(t *testing.T) {
	first := LANDNSABFPolicyIDs("lyroute-ens192", "192.168.10.1")
	second := LANDNSABFPolicyIDs("lyroute-ens224", "192.168.20.1")
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("policy ids = %v / %v, want four per LAN", first, second)
	}
	seen := map[int]bool{}
	for _, id := range append(append([]int(nil), first...), second...) {
		if seen[id] {
			t.Fatalf("policy id %d is shared between LANs", id)
		}
		seen[id] = true
	}
}
