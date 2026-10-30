package vpp

import (
	"testing"
	"time"
)

func TestAcceptanceAFPacketProofAllowsExplicitE1000VMnet(t *testing.T) {
	now := time.Now().UTC()
	proof := CapabilityProof{
		Tier: DataplaneTierNative, Hook: NativeHookAFPacket, Mode: NativeModeAFPacket,
		Source: ProofSourceRuntimeProbe, RuntimeVerified: true, Native: true,
		AcceptanceOnly: true, KernelDriver: "e1000", MACAddress: "52:54:00:aa:bb:02",
		ObservedAt: now, ValidUntil: now.Add(time.Hour), HighPerformance: false,
	}
	if !acceptanceAFPacketProof(true, proof) {
		t.Fatal("e1000 VMnet AF_PACKET proof must be accepted when explicitly enabled")
	}
	if acceptanceAFPacketProof(false, proof) {
		t.Fatal("e1000 AF_PACKET must stay disabled without explicit VMnet opt-in")
	}
}
