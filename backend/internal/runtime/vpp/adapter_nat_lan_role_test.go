package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

// The LAN control-plane commands must not carry static NAT44 deletes. The
// operation instead carries a role plan that the lifecycle reconciles against
// live state with the shared nat44RoleReconcileCommands implementation.
func TestManagementLCPCommandsHaveNoStaticNAT44Roles(t *testing.T) {
	commands := strings.Join(managementLCPCommands(ManagementLCP{
		Enabled:       true,
		VPPInterface:  "lyroute-enp4s0",
		HostInterface: "lylan-enp4s0",
	}), "\n")
	if strings.Contains(commands, "set interface nat44") {
		t.Fatalf("LAN control-plane commands still contain static NAT44 roles:\n%s", commands)
	}
}

func TestBuildOperationsAttachesNAT44RolePlanToFirstLANOnly(t *testing.T) {
	plan := provenPlan(Plan{
		RequestID: "req-multilan-nat-role",
		AddressAssignments: []AddressAssignment{
			{ID: "lan10", LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", CIDR: "192.168.10.1/24", Role: "lan"},
			{ID: "lan20", LinuxInterface: "enp5s0", VPPInterface: "lyroute-enp5s0", CIDR: "192.168.20.1/24", Role: "lan"},
			{ID: "wan", LinuxInterface: "enp6s0", VPPInterface: "lyroute-enp6s0", CIDR: "203.0.113.2/30", Role: "wan"},
		},
		NAT: nat.CompiledConfig{Behavior: nat.BehaviorEndpointDependent},
	}, "enp4s0", "enp5s0", "enp6s0")
	operations := mustBuildOperations(t, plan)
	rolePlans := 0
	for _, operation := range operations {
		if operation.Name != "vpp.lan-control-lcp" {
			continue
		}
		lcp, ok := operation.Payload.(ManagementLCP)
		if !ok {
			t.Fatalf("unexpected LAN control-plane payload %T", operation.Payload)
		}
		if lcp.NAT44RolePlan == nil {
			continue
		}
		rolePlans++
		if got, want := lcp.NAT44RolePlan.LANInterfaces, []string{"lyroute-enp4s0", "lyroute-enp5s0"}; !slicesEqual(got, want) {
			t.Fatalf("role plan LANs = %#v, want %#v", got, want)
		}
		if got, want := lcp.NAT44RolePlan.OutsideInterfaces, []string{"lyroute-enp6s0"}; !slicesEqual(got, want) {
			t.Fatalf("role plan WANs = %#v, want %#v", got, want)
		}
	}
	if rolePlans != 1 {
		t.Fatalf("NAT44 role plan attached %d times, want exactly once", rolePlans)
	}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
