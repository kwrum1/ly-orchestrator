package vpp

import (
	"strings"
	"testing"
	"time"
)

func TestDPDKAttachmentUsesProductNameAndKeepsHardwareNameForRename(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKVFIO, 30)
	proof.VPPInterface = "GigabitEthernet0/3/0"
	request := NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "enp0s3",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	}

	path, err := SelectNativePath(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want one DPDK attachment", path.Attachments)
	}
	attachment := path.Attachments[0]
	if attachment.VPPInterface != "lyroute-enp0s3" {
		t.Fatalf("VPPInterface = %q, want the product naming contract lyroute-enp0s3", attachment.VPPInterface)
	}
	if attachment.HardwareVPPInterface != "GigabitEthernet0/3/0" {
		t.Fatalf("HardwareVPPInterface = %q, want the observed DPDK name", attachment.HardwareVPPInterface)
	}

	plan := Plan{
		RequestID:         "txn-dpdk-naming",
		DataplanePrepared: true,
		NativePath:        request,
		AddressAssignments: []AddressAssignment{{
			ID:             "wan",
			LinuxInterface: "enp0s3",
			VPPInterface:   "lyroute-enp0s3",
			CIDR:           "100.77.0.2/24",
			Role:           "wan",
		}},
	}
	operations, err := BuildOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	var attach Operation
	for _, operation := range operations {
		if operation.Name == "vpp.dataplane.attach" {
			attach = operation
			break
		}
	}
	if attach.Name == "" {
		t.Fatal("build operations omitted the DPDK attach")
	}
	joined := strings.Join(attach.VPPCtlCommands, "\n")
	if !strings.Contains(joined, "?set interface name GigabitEthernet0/3/0 lyroute-enp0s3") {
		t.Fatalf("attach commands = %#v, want a hardware-to-product rename", attach.VPPCtlCommands)
	}
	if !strings.Contains(joined, "set interface state lyroute-enp0s3 up") || !strings.Contains(joined, "show interface lyroute-enp0s3") {
		t.Fatalf("attach commands = %#v, want every follow-up to use the product name", attach.VPPCtlCommands)
	}
	for _, command := range attach.VPPCtlCommands {
		if strings.Contains(command, "GigabitEthernet0/3/0") && !strings.HasPrefix(command, "?set interface name ") {
			t.Fatalf("hardware name leaked into follow-up command %q", command)
		}
	}
	for _, operation := range operations {
		for _, command := range operation.VPPCtlCommands {
			if operation.Name != "vpp.dataplane.attach" && strings.Contains(command, "GigabitEthernet0/3/0") {
				t.Fatalf("hardware name leaked into %s command %q", operation.Name, command)
			}
		}
	}
}

func TestDeclarativeDPDKPathUsesTheSameProductNamingContract(t *testing.T) {
	request := NativePathRequest{
		AllowUnproven: true,
		Assignments: []NativeAssignment{{
			LinuxInterface: "enp0s4",
			Explicit:       true,
			Proof:          CapabilityProof{Tier: DataplaneTierDPDK, Hook: NativeHookDPDK, Mode: NativeModeDPDKUIO, VPPInterface: "GigabitEthernet0/4/0"},
		}},
	}
	path, err := SelectNativePath(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Attachments) != 1 {
		t.Fatalf("attachments = %#v", path.Attachments)
	}
	if path.Attachments[0].VPPInterface != "lyroute-enp0s4" || path.Attachments[0].HardwareVPPInterface != "GigabitEthernet0/4/0" {
		t.Fatalf("attachment = %#v, want product name plus hardware locator", path.Attachments[0])
	}
}
