package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/vpp"
)

func takeoverSelectionProofs(now time.Time) (vpp.CapabilityProof, vpp.CapabilityProof) {
	observed := now.Add(-time.Minute)
	validUntil := now.Add(time.Hour)
	afPacket := vpp.CapabilityProof{
		Tier: vpp.DataplaneTierNative, Hook: vpp.NativeHookAFPacket, Mode: vpp.NativeModeAFPacket,
		Source: vpp.ProofSourceRuntimeProbe, RuntimeVerified: true, Native: true,
		AcceptanceOnly: true, MACAddress: "00:50:56:aa:bb:01",
		ObservedAt: observed, ValidUntil: validUntil,
	}
	afXDP := vpp.CapabilityProof{
		Tier: vpp.DataplaneTierNative, Hook: vpp.NativeHookAFXDP, Mode: vpp.NativeModeCopy,
		Source: vpp.ProofSourceRuntimeProbe, RuntimeVerified: true, Native: true,
		HighPerformance: true, KernelDriver: "igc",
		ObservedAt: observed, ValidUntil: validUntil,
	}
	return afPacket, afXDP
}

func takeoverSelectionRequest(now time.Time) vpp.NativePathRequest {
	afPacket, afXDP := takeoverSelectionProofs(now)
	return vpp.NativePathRequest{
		ManagementInterface: "ens192",
		Now:                 now,
		Assignments: []vpp.NativeAssignment{{
			LinuxInterface:  "eth1",
			Explicit:        true,
			AFPacketConsent: true,
			Proof:           afPacket,
			Candidates:      []vpp.CapabilityProof{afPacket, afXDP},
		}},
	}
}

func selectedAttachment(t *testing.T, path vpp.NativePath) vpp.NativeAttachment {
	t.Helper()
	if len(path.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want exactly one", path.Attachments)
	}
	return path.Attachments[0]
}

func TestVerifiedAFXDPKeepsSelectionOnAFXDPForAnAllowedDriver(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "ixgbe", "1", 1000, true)
	if err := writeNetdevAddress(fixture.root, "eth1", "00:50:56:aa:bb:01"); err != nil {
		t.Fatal(err)
	}
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_xdp_copy","verification":"verified","functional_verified":true}`)
	server := &Server{}
	now := time.Now().UTC()
	request := takeoverSelectionRequest(now)
	server.preferCommittedNativeCandidates(context.Background(), &request)
	if len(request.Assignments[0].Candidates) != 1 {
		t.Fatalf("candidates = %#v, want only the committed AF_XDP proof", request.Assignments[0].Candidates)
	}
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	attachment := selectedAttachment(t, path)
	if attachment.Hook != vpp.NativeHookAFXDP || attachment.Mode != vpp.NativeModeCopy {
		t.Fatalf("attachment = %#v, want AF_XDP copy", attachment)
	}
	commands := strings.Join(vpp.DataplaneAttachOperation("req", attachment).VPPCtlCommands, "\n")
	if !strings.Contains(commands, "create interface af_xdp") {
		t.Fatalf("commands do not create AF_XDP:\n%s", commands)
	}
	if strings.Contains(commands, "create host-interface") {
		t.Fatalf("a verified AF_XDP port still generated an af_packet create:\n%s", commands)
	}
}

// A dpdk-verification.json left behind by an older image must not outrank the
// rule library. On igc the AF_XDP TX path crashed VPP; the stale record is
// dropped and selection falls through to the permitted AF_PACKET path.
func TestStaleAFXDPVerificationOnIGCNeverDrivesSelection(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	if err := writeNetdevAddress(fixture.root, "eth1", "00:50:56:aa:bb:01"); err != nil {
		t.Fatal(err)
	}
	staleLog := filepath.Join(fixture.root, "stale-modes.jsonl")
	t.Setenv("LY_ROUTE_DATAPLANE_STALE_MODES", staleLog)
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_xdp_copy","verification":"verified","functional_verified":true}`)
	server := &Server{}
	now := time.Now().UTC()
	request := takeoverSelectionRequest(now)
	server.preferCommittedNativeCandidates(context.Background(), &request)
	server.enforceTakeoverRuleLibrary(context.Background(), &request)
	for _, candidate := range request.Assignments[0].Candidates {
		if candidate.Hook == vpp.NativeHookAFXDP {
			t.Fatalf("an igc assignment kept an AF_XDP candidate: %#v", candidate)
		}
	}
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	attachment := selectedAttachment(t, path)
	if attachment.Hook != vpp.NativeHookAFPacket {
		t.Fatalf("attachment = %#v, want the rule-permitted AF_PACKET fallback", attachment)
	}
	if entries := readVerificationFile(t, filepath.Join(fixture.root, "verification.json")); len(entries) != 0 {
		t.Fatalf("stale AF_XDP verification survived: %#v", entries)
	}
	record, err := os.ReadFile(staleLog)
	if err != nil || !strings.Contains(string(record), "stale_mode_dropped") || !strings.Contains(string(record), "af_xdp_copy") {
		t.Fatalf("stale-mode evidence = %q, err=%v", record, err)
	}
}

func readVerificationFile(t *testing.T, path string) []dataplane.DPDKVerification {
	t.Helper()
	state, err := dataplane.LoadDPDKVerification(path)
	if err != nil {
		t.Fatal(err)
	}
	return state.Interfaces
}

func TestVerifiedAFPacketKeepsSelectionOnAFPacket(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	if err := writeNetdevAddress(fixture.root, "eth1", "00:50:56:aa:bb:01"); err != nil {
		t.Fatal(err)
	}
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_packet","verification":"verified","functional_verified":true}`)
	server := &Server{}
	now := time.Now().UTC()
	request := takeoverSelectionRequest(now)
	request.Assignments[0].AFPacketConsent = false
	server.preferCommittedNativeCandidates(context.Background(), &request)
	if !request.Assignments[0].AFPacketConsent {
		t.Fatal("a committed af_packet attachment lost its consent and would force a mode change")
	}
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	attachment := selectedAttachment(t, path)
	if attachment.Hook != vpp.NativeHookAFPacket {
		t.Fatalf("attachment = %#v, want the committed af_packet mode", attachment)
	}
	commands := strings.Join(vpp.DataplaneAttachOperation("req", attachment).VPPCtlCommands, "\n")
	if !strings.Contains(commands, "create host-interface") {
		t.Fatalf("commands do not keep af_packet:\n%s", commands)
	}
	if strings.Contains(commands, "create interface af_xdp") {
		t.Fatalf("a verified af_packet port generated an AF_XDP create:\n%s", commands)
	}
}

func TestCommittedNativeSelectionIsIdempotent(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "ixgbe", "1", 1000, true)
	if err := writeNetdevAddress(fixture.root, "eth1", "00:50:56:aa:bb:01"); err != nil {
		t.Fatal(err)
	}
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_xdp_copy","verification":"verified","functional_verified":true}`)
	server := &Server{}
	run := func() (vpp.NativeAttachment, []string) {
		now := time.Now().UTC()
		request := takeoverSelectionRequest(now)
		server.preferCommittedNativeCandidates(context.Background(), &request)
		if len(request.Assignments[0].Candidates) != 1 {
			t.Fatalf("candidates = %#v, want a single committed proof", request.Assignments[0].Candidates)
		}
		path, err := vpp.SelectNativePath(request)
		if err != nil {
			t.Fatalf("selection failed: %v", err)
		}
		attachment := selectedAttachment(t, path)
		return attachment, vpp.DataplaneAttachOperation("req", attachment).VPPCtlCommands
	}
	firstAttachment, firstCommands := run()
	secondAttachment, secondCommands := run()
	if firstAttachment.Hook != secondAttachment.Hook || firstAttachment.Mode != secondAttachment.Mode {
		t.Fatalf("selection changed between applies: %#v vs %#v", firstAttachment, secondAttachment)
	}
	if strings.Join(firstCommands, "\n") != strings.Join(secondCommands, "\n") {
		t.Fatalf("attach commands changed between applies:\n%s\n---\n%s", strings.Join(firstCommands, "\n"), strings.Join(secondCommands, "\n"))
	}
	joined := strings.Join(firstCommands, "\n")
	if strings.Contains(joined, "create host-interface") || strings.Count(joined, "create interface af_xdp") != 1 {
		t.Fatalf("repeat apply would create an extra interface:\n%s", joined)
	}
}

func writeNetdevAddress(root, name, mac string) error {
	return os.WriteFile(filepath.Join(root, "class/net", name, "address"), []byte(mac+"\n"), 0o644)
}
