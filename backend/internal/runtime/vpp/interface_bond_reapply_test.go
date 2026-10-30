package vpp

import (
	"strings"
	"testing"
)

// TestReappliedInterfaceIsNotRequestedAbsent pins the repeatable-apply
// regression seen on the J1900: after a dataplane restart the DPDK ports come
// up admin-down, so the desired state differs from live. The diff queues the
// interface in both Interfaces (new state) and DeleteInterfaces (replace), and
// the old readback then required it to be absent while the apply had just set
// it admin-up with an address. BuildInterfaceBondOperations only drops stale
// addresses for a surviving interface, so the snapshot request must treat it as
// present.
func TestReappliedInterfaceIsNotRequestedAbsent(t *testing.T) {
	plan := InterfaceBondPlan{
		TransactionID: "txn-reapply",
		Interfaces: []InterfaceState{{
			Name:       "lyroute-enp3s0",
			AdminState: "up",
			Addresses:  []string{"192.168.88.1/24"},
		}},
		DeleteInterfaces:     []string{"lyroute-enp3s0", "lyroute-enp4s0"},
		DeleteInterfaceState: []InterfaceState{{Name: "lyroute-enp3s0"}, {Name: "lyroute-enp4s0"}},
	}
	request := lifecycleSnapshotRequest(plan.TransactionID, plan)
	for _, absent := range request.AbsentInterfaces {
		if absent == "lyroute-enp3s0" {
			t.Fatalf("a re-applied interface was requested absent: %#v", request.AbsentInterfaces)
		}
	}
	if !strings.Contains(strings.Join(request.AbsentInterfaces, ","), "lyroute-enp4s0") {
		t.Fatalf("a genuinely removed interface was not requested absent: %#v", request.AbsentInterfaces)
	}
}
