package vpp

import (
	"context"
	"strings"
	"testing"
)

// TestForbiddenVPPCTLCommandRejectsUnsafePacketGeneratorNode pins the second
// image-specific VPP crash: `packet-generator new ... node interface-output
// interface <if>` parses but the first enabled packet SIGSEGVs inside
// vnet_per_buffer_interface_output_node_fn (+0x45b), taking VPP and both DPDK
// port carriers down (J1900 evidence 74-vpp-crash.txt). The safe injection
// points are real feature nodes: ethernet-input, ip4-input, ip4-lookup,
// ip6-lookup.
func TestForbiddenVPPCTLCommandRejectsUnsafePacketGeneratorNode(t *testing.T) {
	forbidden := []string{
		"packet-generator new { name t5 limit 1000 rate 1e4 size 64 node interface-output interface GigabitEthernet3/0/0 data { hex 0x00 } }",
		"?packet-generator new node INTERFACE-OUTPUT interface lyroute-enp3s0",
		"packet-generator new name t6 limit 10 node interface-output interface GigabitEthernet4/0/0 data { hex 0x00 }",
	}
	for _, command := range forbidden {
		if err := forbiddenVPPCTLCommand(command); err == nil {
			t.Fatalf("forbiddenVPPCTLCommand(%q) accepted a node that SIGSEGVs vnet_per_buffer_interface_output_node_fn", command)
		}
	}
	allowed := []string{
		"packet-generator new { name smoke limit 1000 rate 1e4 node ethernet-input interface lyroute-enp3s0 data { hex 0x00 } }",
		"packet-generator disable-stream smoke",
		"packet-generator delete smoke",
		"show packet-generator",
	}
	for _, command := range allowed {
		if err := forbiddenVPPCTLCommand(command); err != nil {
			t.Fatalf("forbiddenVPPCTLCommand(%q) rejected a safe packet-generator command: %v", command, err)
		}
	}
}

func TestUnsafePacketGeneratorIsRefusedBeforeVPPCTLIsExecuted(t *testing.T) {
	channel := &vppctlChannel{binary: "/bin/false"}
	reply, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.traffic.smoke",
		VPPCtlCommands: []string{"packet-generator new { name t5 limit 1000 node interface-output interface GigabitEthernet3/0/0 data { hex 0x00 } }"},
	})
	if err == nil {
		t.Fatalf("doCommands executed a crashing packet-generator command: %+v", reply)
	}
	if !strings.Contains(err.Error(), "interface-output") {
		t.Fatalf("unexpected error: %v", err)
	}
}
