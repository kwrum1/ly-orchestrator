package vpp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestForbiddenVPPCTLCommandRejectsPing(t *testing.T) {
	forbidden := []string{
		"ping 10.99.0.4 source lyroute-ens224 repeat 2",
		"?ping 10.99.0.4",
		"PING 10.99.0.4",
		"  ping ipv6 ::1  ",
	}
	for _, command := range forbidden {
		if err := forbiddenVPPCTLCommand(command); err == nil {
			t.Fatalf("forbiddenVPPCTLCommand(%q) accepted a command that crashes nat44-ed-out2in", command)
		}
	}
	allowed := []string{
		"show interface",
		"show nat44 interfaces",
		"ip route add 203.0.113.0/24 via 10.99.0.2",
		"set interface mtu 9000 lyroute-ens224",
		"set interface nat44 in tap0",
	}
	for _, command := range allowed {
		if err := forbiddenVPPCTLCommand(command); err != nil {
			t.Fatalf("forbiddenVPPCTLCommand(%q) rejected a legitimate command: %v", command, err)
		}
	}
}

func TestPingIsRefusedBeforeVPPCTLIsExecuted(t *testing.T) {
	channel := &vppctlChannel{binary: "/bin/false"}
	reply, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.test",
		VPPCtlCommands: []string{"ping 10.99.0.4 source lyroute-ens224"},
	})
	if err == nil {
		t.Fatalf("doCommands executed a forbidden ping command: %+v", reply)
	}
	if !strings.Contains(err.Error(), "refusing VPP command") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNativeProbeRejectedOnParavirtualDrivers(t *testing.T) {
	for _, driver := range []string{"vmxnet3", "e1000", "e1000e", "virtio_net", "VMXNET3"} {
		if !NativeProbeDriverUnsupported(NativeHookAFXDP, driver) {
			t.Fatalf("af_xdp accepted on %s", driver)
		}
		if !NativeProbeDriverUnsupported(NativeHookRDMA, driver) {
			t.Fatalf("rdma accepted on %s", driver)
		}
		if NativeProbeDriverUnsupported(NativeHookAFPacket, driver) {
			t.Fatalf("af_packet must stay available on %s", driver)
		}
	}
	if NativeProbeDriverUnsupported(NativeHookAFXDP, "mlx5_core") {
		t.Fatal("af_xdp rejected on a driver that can host it")
	}
}

func TestAFXDPCandidateOnVMXNet3IsRejected(t *testing.T) {
	now := time.Now().UTC()
	proof := CapabilityProof{
		VPPInterface:     "lyroute-ens256",
		Tier:             DataplaneTierNative,
		Hook:             NativeHookAFXDP,
		Mode:             NativeModeZeroCopy,
		Source:           ProofSourceRuntimeProbe,
		RuntimeVerified:  true,
		Native:           true,
		HighPerformance:  true,
		ObservedAt:       now.Add(-time.Minute),
		ValidUntil:       now.Add(time.Hour),
		PerformanceScore: 100,
		KernelDriver:     "vmxnet3",
		MACAddress:       "00:50:56:3a:15:22",
	}
	reasons := candidateRejectionReasons(proof, now, DataplaneTierNative, false, false)
	found := false
	for _, reason := range reasons {
		if strings.Contains(reason, "not supported on kernel driver vmxnet3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("vmxnet3 AF_XDP candidate was not rejected: %v", reasons)
	}
}
