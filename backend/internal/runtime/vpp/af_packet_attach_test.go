package vpp

import "testing"

// A vmxnet3 acceptance boot used to pre-create a TAP named lyroute-<port>; the
// runtime then attached AF_PACKET to the same port and renamed host-<port> onto
// the same product name. VPP does not enforce name uniqueness on rename, so the
// result was two lyroute-<port> interfaces and every snapshot decode failed
// with `interface "lyroute-<port>" is ambiguous`.
//
// The stale TAP is released by CleanupDuplicateDataplaneTAPs, which deletes it
// by sw_if_index before the apply; the attach itself must not delete by name
// (that can resolve to the live AF_PACKET interface).
func TestDataplaneAttachOperationAFPacketDoesNotDeleteByName(t *testing.T) {
	operation := DataplaneAttachOperation("req-af-packet", NativeAttachment{
		LinuxInterface: "ens161",
		VPPInterface:   "lyroute-ens161",
		Hook:           NativeHookAFPacket,
		Mode:           NativeModeAFPacket,
		MACAddress:     "00:50:56:3a:15:21",
	})
	want := []string{
		"?create host-interface name ens161 cksum-gso-disable",
		"?set interface name host-ens161 lyroute-ens161",
		"set interface mac address lyroute-ens161 00:50:56:3a:15:21",
		"set interface state lyroute-ens161 up",
		"show hardware-interfaces lyroute-ens161",
		"show interface lyroute-ens161",
	}
	if len(operation.VPPCtlCommands) != len(want) {
		t.Fatalf("af_packet commands = %#v", operation.VPPCtlCommands)
	}
	for index := range want {
		if operation.VPPCtlCommands[index] != want[index] {
			t.Fatalf("af_packet command %d = %q, want %q", index, operation.VPPCtlCommands[index], want[index])
		}
	}
	if operation.Payload.(NativeAttachment).Hook != NativeHookAFPacket {
		t.Fatalf("payload hook = %v", operation.Payload)
	}
}
