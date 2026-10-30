package vpp

import (
	"testing"
	"time"
)

func TestInterfacesRollbackDetachesLCPBeforeNativeAttachment(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	plan := Plan{
		RequestID: "txn-rollback-order",
		NativePath: NativePathRequest{
			ManagementInterface: "eth0",
			Now:                 now,
			Assignments: []NativeAssignment{{
				LinuxInterface: "ens192",
				Explicit:       true,
				Proof:          provenAFXDPProof(now),
			}},
		},
		AddressAssignments: []AddressAssignment{{
			ID:             "lan",
			LinuxInterface: "ens192",
			VPPInterface:   "lyroute-ens192",
			CIDR:           "10.1.18.1/24",
			Role:           "lan",
		}},
	}

	cleanup, err := SupplementalCleanupOperations(plan, SupplementalInterfaces)
	if err != nil {
		t.Fatal(err)
	}
	lcp := operationIndex(cleanup, "vpp.lan-control-lcp.rollback-delete")
	attach := operationIndex(cleanup, "vpp.dataplane.attach.rollback-delete")
	if lcp < 0 || attach < 0 {
		t.Fatalf("cleanup is missing LCP or native attachment deletion: %#v", cleanup)
	}
	if lcp > attach {
		t.Fatalf("native attachment is deleted before LCP/ABF cleanup; interfaces cleanup would fail with an unknown ingress: %#v", operationNames(cleanup))
	}
	for _, operation := range cleanup {
		if operation.Name != "vpp.lan-control-lcp.rollback-delete" {
			continue
		}
		management, ok := operation.Payload.(ManagementLCP)
		if !ok || management.Enabled {
			t.Fatalf("LCP cleanup payload must be disabled: %#v", operation.Payload)
		}
	}
}

func TestServiceChainDeleteTreatsUnknownIngressAsDetached(t *testing.T) {
	policy := lanDHCPBroadcastBypassPolicy("lyroute-eth2")
	results := []VPPCTLCommandResult{
		{Command: "show abf policy 65000", Stdout: "Invalid policy ID:65000"},
		{Command: "show abf attach lyroute-eth2", Stdout: "show abf attach: unknown input 'lyroute-eth2'"},
	}
	if !serviceChainPolicyAbsent(results, policy) {
		t.Fatal("deleted ingress must count as detached after a rollback removed the interface")
	}
}

func TestServiceChainDeleteKeepsPolicyThatStillExists(t *testing.T) {
	policy := lanDHCPBroadcastBypassPolicy("lyroute-eth2")
	results := []VPPCTLCommandResult{
		{Command: "show abf policy 65000", Stdout: "abf:[0]: policy:65000 acl:7\n no forwarding\n"},
		{Command: "show abf attach lyroute-eth2", Stdout: "ipv4:\n abf-interface-attach: policy:65000 priority:1\n"},
	}
	if serviceChainPolicyAbsent(results, policy) {
		t.Fatal("a still-configured policy must not be reported as deleted")
	}
}
