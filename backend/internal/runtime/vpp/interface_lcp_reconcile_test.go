package vpp

import (
	"strings"
	"testing"
)

func lanAssignment(id, linux string) AddressAssignment {
	return AddressAssignment{ID: id, LinuxInterface: linux, VPPInterface: "lyroute-" + linux, CIDR: "192.168.89.1/24", Role: "lan"}
}

// Clearing one LAN removes exactly that LCP pair. The pair for the surviving
// LAN is retained, and no native attachment is detached by a role edit.
func TestInterfaceLCPReconciliationDeletesOnlyClearedRole(t *testing.T) {
	prior := Plan{RequestID: "prior", AddressAssignments: []AddressAssignment{
		lanAssignment("eth2", "enp3s0"),
		lanAssignment("eth3", "enp4s0"),
	}}
	desired := Plan{RequestID: "desired", AddressAssignments: []AddressAssignment{
		lanAssignment("eth3", "enp4s0"),
	}}
	cleanup, err := SupplementalReconciliationCleanupOperations(prior, desired, SupplementalInterfaces)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) != 1 {
		t.Fatalf("cleanup operations = %#v, want one LCP deletion", cleanup)
	}
	operation := cleanup[0]
	if operation.Name != "vpp.lan-control-lcp.reconcile-delete" || operation.Resource != "eth2" {
		t.Fatalf("cleanup identity = %s/%s, want vpp.lan-control-lcp.reconcile-delete/eth2", operation.Name, operation.Resource)
	}
	management, ok := operation.Payload.(ManagementLCP)
	if !ok || management.Enabled {
		t.Fatalf("cleanup payload = %#v, want disabled ManagementLCP", operation.Payload)
	}
	if management.HostInterface != LANControlPlaneHostInterface("enp3s0") {
		t.Fatalf("cleanup host interface = %q", management.HostInterface)
	}
	if !strings.Contains(strings.Join(operation.VPPCtlCommands, "\n"), "show lcp") {
		t.Fatalf("cleanup has no LCP readback: %#v", operation.VPPCtlCommands)
	}
	for _, item := range cleanup {
		if strings.Contains(item.Name, "dataplane.attach") {
			t.Fatalf("role cleanup must not detach a native attachment: %s", item.Name)
		}
	}
}

func TestInterfaceLCPReconciliationIsIdempotent(t *testing.T) {
	plan := Plan{RequestID: "same", AddressAssignments: []AddressAssignment{
		lanAssignment("eth2", "enp3s0"),
	}}
	cleanup, err := SupplementalReconciliationCleanupOperations(plan, plan, SupplementalInterfaces)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) != 0 {
		t.Fatalf("unchanged generation produced cleanup: %#v", cleanup)
	}
}

func TestInterfaceLCPReconciliationDeletesEveryClearedLAN(t *testing.T) {
	prior := Plan{RequestID: "prior", AddressAssignments: []AddressAssignment{
		lanAssignment("eth2", "enp3s0"),
		lanAssignment("eth3", "enp4s0"),
	}}
	cleanup, err := SupplementalReconciliationCleanupOperations(prior, Plan{RequestID: "empty"}, SupplementalInterfaces)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) != 2 {
		t.Fatalf("cleanup operations = %d, want 2", len(cleanup))
	}
}
