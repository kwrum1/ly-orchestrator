package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/vpp"
)

func takeoverTestSnapshot() takeoverSnapshot {
	return takeoverSnapshot{
		LinuxInterface: "eth1",
		PCIAddress:     "0000:03:00.0",
		Netdev:         "eth1",
		KernelDriver:   "igc",
		BoundDriver:    "igc",
		VPPInterface:   "lyroute-eth1",
		CurrentMode:    takeoverModeAFPacket,
		Owned:          true,
		ModeConfirmed:  true,
		SWIfIndex:      3,
		QueueFDs:       []int{12, 13},
		XDPProgram:     "id:9",
	}
}

func supportedDPDKCapability() vpp.CapabilityAssessment {
	return vpp.AssessDPDKTakeover(vpp.NativeModeDPDKUIO, vpp.DPDKTakeoverEvidence{
		UIOPCIAvailable: vpp.KnownFact(true),
		DPDKPlugin:      vpp.KnownFact(true),
		Hugepages:       vpp.KnownFact(true),
		PCIAddressSafe:  vpp.KnownFact(true),
	})
}

func TestPlanTakeoverTransitionKeepsCurrentModeWithoutExplicitSwitch(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, false, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if transition.Status != takeoverStatusKept || len(transition.Steps) != 0 {
		t.Fatalf("transition = %#v, want an explicit kept decision with no steps", transition)
	}
	if transition.FromMode != takeoverModeAFPacket || transition.ToMode != takeoverModeDPDKUIO {
		t.Fatalf("modes = %s -> %s", transition.FromMode, transition.ToMode)
	}
	if !strings.Contains(transition.Reason, "not requested") {
		t.Fatalf("reason = %q", transition.Reason)
	}
}

func TestPlanTakeoverUnknownCapabilityBecomesAutomaticProbe(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, true, vpp.CapabilityAssessment{
		Status: vpp.CapabilityUnknown,
		Reason: "no runtime proof",
	}, now)
	if err != nil {
		t.Fatalf("automatic takeover with an unknown capability was rejected: %v", err)
	}
	if !transition.CapabilityUnprovenProbe || !transition.ProbeRequired || transition.ProbeResult != "pending" {
		t.Fatalf("transition = %#v, want one automatic transactional probe", transition)
	}
	if transition.Status != takeoverStatusPlanned {
		t.Fatalf("status = %q", transition.Status)
	}
}

func TestPlanTakeoverTransitionOrdersForwardAndRollbackSteps(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	kinds := []string{}
	for _, step := range transition.Steps {
		kinds = append(kinds, step.Kind)
	}
	if strings.Join(kinds, ",") != "detach,bind,verify,record" {
		t.Fatalf("steps = %v, want detach,bind,verify,record", kinds)
	}
	rollback := []string{}
	for _, step := range transition.Rollback {
		rollback = append(rollback, step.Kind+":"+step.Mode)
	}
	if strings.Join(rollback, ",") != "remove:dpdk_uio,restore:af_packet" {
		t.Fatalf("rollback = %v, want reverse dependency order", rollback)
	}
	if transition.FromDriver != "igc" || transition.ToDriver != "uio_pci_generic" {
		t.Fatalf("drivers = %s -> %s", transition.FromDriver, transition.ToDriver)
	}
	if !transition.RollbackOK || transition.ConfirmationRequired {
		t.Fatalf("rollback = %t, confirmation = %t", transition.RollbackOK, transition.ConfirmationRequired)
	}
}

func TestPlanTakeoverRecordsRiskyRollbackWithoutBlocking(t *testing.T) {
	now := time.Now().UTC()
	snapshot := takeoverTestSnapshot()
	snapshot.KernelDriver = ""
	transition, err := planTakeoverTransition(snapshot, takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatalf("automatic plan failed: %v", err)
	}
	if !transition.ConfirmationRequired || !transition.Confirmed || transition.RollbackOK {
		t.Fatalf("transition = %#v, want a recorded risky automatic switch", transition)
	}
	if transition.Status != takeoverStatusPlanned || len(transition.Steps) == 0 {
		t.Fatalf("transition = %#v, want the switch to be planned", transition)
	}
}

// fakeTakeoverExecutor records the exact call order and can fail one step.
type fakeTakeoverExecutor struct {
	calls          []string
	failOn         string
	rollbackFailOn string
	recoverErr     error
	recovered      bool
	result         takeoverStepResult
}

func (fake *fakeTakeoverExecutor) Execute(_ context.Context, transition *takeoverTransition, step takeoverStep) (takeoverStepResult, error) {
	fake.calls = append(fake.calls, step.Kind+":"+step.Mode)
	if step.Kind == fake.failOn {
		return takeoverStepResult{}, errors.New("injected forward failure")
	}
	if step.Kind == "verify" {
		return fake.result, nil
	}
	return takeoverStepResult{}, nil
}

func (fake *fakeTakeoverExecutor) Rollback(_ context.Context, transition *takeoverTransition, step takeoverStep) error {
	fake.calls = append(fake.calls, "rollback-"+step.Kind+":"+step.Mode)
	if step.Kind == fake.rollbackFailOn {
		return errors.New("injected rollback failure")
	}
	return nil
}

func (fake *fakeTakeoverExecutor) Recover(_ context.Context, transition *takeoverTransition) error {
	fake.calls = append(fake.calls, "recover")
	if fake.recoverErr != nil {
		return fake.recoverErr
	}
	fake.recovered = true
	return nil
}

func TestExecuteTakeoverTransitionRollsBackInReverseOrder(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeTakeoverExecutor{failOn: "verify"}
	execErr := executeTakeoverTransition(context.Background(), &transition, executor)
	if execErr == nil {
		t.Fatal("expected the injected verification failure")
	}
	want := []string{"detach:af_packet", "bind:dpdk_uio", "verify:dpdk_uio", "rollback-bind:dpdk_uio", "rollback-detach:af_packet", "recover"}
	if strings.Join(executor.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", executor.calls, want)
	}
	if !executor.recovered {
		t.Fatal("rollback finished without recovering VPP")
	}
	if transition.Status != takeoverStatusFailed || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want failed with a restored rollback", transition)
	}
	if transition.FromMode != takeoverModeAFPacket || transition.ToMode != takeoverModeDPDKUIO {
		t.Fatalf("modes = %s -> %s", transition.FromMode, transition.ToMode)
	}
	if transition.Netdev != "eth1" || transition.KernelDriver != "igc" {
		t.Fatalf("identity = %#v", transition)
	}
}

func TestExecuteTakeoverTransitionRecordsRuntimeHandles(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeTakeoverExecutor{result: takeoverStepResult{SWIfIndex: 42, QueueFDs: []int{21, 22}, XDPProgram: "id:77"}}
	if err := executeTakeoverTransition(context.Background(), &transition, executor); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if transition.Status != takeoverStatusApplied || transition.RollbackResult != takeoverRollbackNotNeeded {
		t.Fatalf("transition = %#v", transition)
	}
	if transition.SWIfIndex != 42 || len(transition.QueueFDs) != 2 || transition.XDPProgram != "id:77" {
		t.Fatalf("handles = %#v", transition)
	}
}

func TestExecuteTakeoverTransitionReportsRollbackFailure(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeTakeoverExecutor{failOn: "verify", rollbackFailOn: "bind"}
	execErr := executeTakeoverTransition(context.Background(), &transition, executor)
	if execErr == nil {
		t.Fatal("expected the injected bind failure")
	}
	if transition.Status != takeoverStatusRollbackFailed || transition.RollbackResult != takeoverRollbackRestoreFailed {
		t.Fatalf("transition = %#v, want a failed rollback", transition)
	}
	if !strings.Contains(execErr.Error(), "takeover rollback failed") {
		t.Fatalf("error = %v", execErr)
	}
}

func TestExecuteTakeoverTransitionFailsRollbackWhenVPPCannotBeRecovered(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, supportedDPDKCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeTakeoverExecutor{failOn: "verify", recoverErr: errors.New("injected recover failure")}
	execErr := executeTakeoverTransition(context.Background(), &transition, executor)
	if execErr == nil {
		t.Fatal("expected the injected verification failure")
	}
	if transition.RollbackResult != takeoverRollbackRestoreFailed || transition.Status != takeoverStatusRollbackFailed {
		t.Fatalf("transition = %#v, want a failed rollback because VPP is not usable", transition)
	}
}

func TestTakeoverJournalPersistsTheRollbackOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "takeover-state.json")
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", path)
	now := time.Now().UTC()
	first := takeoverTransition{
		SchemaVersion: 1, Interface: "eth1", FromMode: takeoverModeAFPacket, ToMode: takeoverModeDPDKUIO,
		Status: takeoverStatusApplied, RollbackResult: takeoverRollbackNotNeeded, StartedAt: now,
	}
	second := takeoverTransition{
		SchemaVersion: 1, Interface: "eth1", FromMode: takeoverModeDPDKUIO, ToMode: takeoverModeAFXDPCopy,
		Status: takeoverStatusFailed, RollbackResult: takeoverRollbackRestored, Failures: []string{"verify: timed out"}, StartedAt: now.Add(time.Minute),
	}
	for _, transition := range []takeoverTransition{first, second} {
		if err := recordTakeoverTransition(transition); err != nil {
			t.Fatal(err)
		}
	}
	journal, err := loadTakeoverJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Last == nil || journal.Last.ToMode != takeoverModeAFXDPCopy || journal.Last.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("journal last = %#v", journal.Last)
	}
	if len(journal.History) != 2 {
		t.Fatalf("history = %d, want 2", len(journal.History))
	}
	latest, ok := lastTakeoverTransition("eth1")
	if !ok || latest.ToMode != takeoverModeAFXDPCopy {
		t.Fatalf("lastTakeoverTransition = %#v, %t", latest, ok)
	}
	if _, ok := lastTakeoverTransition("eth2"); ok {
		t.Fatal("journal leaked to an unrelated interface")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"from_mode\": \"dpdk_uio\"", "\"to_mode\": \"af_xdp_copy\"", "\"rollback_result\": \"restored\""} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("journal is missing %s: %s", want, data)
		}
	}
}

func TestParseTakeoverRuntimeFacts(t *testing.T) {
	interfaces := "              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)\n" +
		"       lyroute-eth1               7     down          9000/0/0/0\n"
	if got := parseVPPInterfaceIndex(interfaces, "lyroute-eth1"); got != 7 {
		t.Fatalf("sw_if_index = %d, want 7", got)
	}
	if got := parseVPPInterfaceIndex(interfaces, "lyroute-eth2"); got != 0 {
		t.Fatalf("unknown interface index = %d, want 0", got)
	}
	afxdp := "rx queue 0 fd 12\nrx queue 1 fd 13\ntx queue 0 fd 14\n"
	fds := parseAFXDPQueueFDs(afxdp)
	if len(fds) != 3 || fds[0] != 12 || fds[2] != 14 {
		t.Fatalf("queue fds = %#v", fds)
	}
	link := "2: eth1: <BROADCAST> mtu 1500 xdp qdisc noqueue state UP\n\tprog/xdp id 91 tag aa\n"
	if got := parseXDPProgramID(link); got != "id:91" {
		t.Fatalf("xdp program = %q, want id:91", got)
	}
	if got := parseXDPProgramID("2: eth1: <BROADCAST> mtu 1500 state UP"); got != "" {
		t.Fatalf("unexpected xdp program %q", got)
	}
}

func TestRollbackStopsVPPBeforeReleasingDPDKAndRecoversIt(t *testing.T) {
	root := t.TempDir()
	order := filepath.Join(root, "order.log")
	script := func(name string) string {
		return "#!/bin/sh\nprintf '%s\\n' " + name + " >> \"$ORDER_LOG\"\nexit 0\n"
	}
	for name, file := range map[string]string{
		"stop":    filepath.Join(root, "stop.sh"),
		"release": filepath.Join(root, "release.sh"),
		"prepare": filepath.Join(root, "prepare.sh"),
	} {
		if err := os.WriteFile(file, []byte(script(name)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vppctl := filepath.Join(root, "vppctl")
	if err := os.WriteFile(vppctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORDER_LOG", order)
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/sh "+filepath.Join(root, "stop.sh"))
	t.Setenv("LY_ROUTE_DPDK_RELEASE_COMMAND", filepath.Join(root, "release.sh"))
	t.Setenv("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", filepath.Join(root, "prepare.sh"))
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	transition := takeoverTransition{Interface: "eth1", PCIAddress: "0000:03:00.0", FromMode: takeoverModeAFPacket, ToMode: takeoverModeDPDKUIO}
	executor := systemTakeoverExecutor{server: &Server{}}
	if err := executor.Rollback(context.Background(), &transition, takeoverStep{Kind: "bind", Mode: takeoverModeDPDKUIO}); err != nil {
		t.Fatalf("rollback of the destructive bind failed: %v", err)
	}
	data, err := os.ReadFile(order)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(strings.ReplaceAll(string(data), "\r", ""))
	if len(lines) != 2 || lines[0] != "stop" || lines[1] != "release" {
		t.Fatalf("rollback order = %v, want stop before release", lines)
	}
	// Recover must leave VPP usable (here it is already reachable).
	if err := executor.Recover(context.Background(), &transition); err != nil {
		t.Fatalf("recover failed: %v", err)
	}
}

func TestRollbackRecoverRestartsStoppedVPP(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "vpp-up")
	restarted := filepath.Join(root, "restarted")
	stop := filepath.Join(root, "stop.sh")
	if err := os.WriteFile(stop, []byte("#!/bin/sh\nrm -f \"$VPP_UP\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restart := filepath.Join(root, "restart.sh")
	if err := os.WriteFile(restart, []byte("#!/bin/sh\nprintf 'ok\\n' > \"$VPP_UP\"\nprintf 'ok\\n' > \"$RESTART_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vppctl := filepath.Join(root, "vppctl")
	if err := os.WriteFile(vppctl, []byte("#!/bin/sh\n[ -f \"$VPP_UP\" ] || exit 1\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPP_UP", up)
	t.Setenv("RESTART_MARKER", restarted)
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/sh "+stop)
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", "/bin/sh "+restart)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	transition := takeoverTransition{Interface: "eth1", PCIAddress: "0000:03:00.0", FromMode: takeoverModeDPDKVFIO, ToMode: takeoverModeAFPacket}
	executor := systemTakeoverExecutor{server: &Server{}}
	if err := executor.stopVPP(context.Background()); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if _, err := os.Stat(up); err == nil {
		t.Fatal("stop did not take VPP down")
	}
	if err := executor.Recover(context.Background(), &transition); err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if _, err := os.Stat(up); err != nil {
		t.Fatalf("VPP is not active after recover: %v", err)
	}
	if _, err := os.Stat(restarted); err != nil {
		t.Fatalf("recover did not restart VPP: %v", err)
	}
	if !strings.Contains(transition.RollbackDetail, "restarted") {
		t.Fatalf("rollback detail = %q", transition.RollbackDetail)
	}
}

func TestLifecycleCommandsIgnoreRequestCancellation(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "stopped")
	stop := filepath.Join(root, "stop.sh")
	if err := os.WriteFile(stop, []byte("#!/bin/sh\nprintf 'ok\\n' > \"$STOP_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STOP_MARKER", marker)
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/sh "+stop)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := systemTakeoverExecutor{server: &Server{}}
	if err := executor.stopVPP(ctx); err != nil {
		t.Fatalf("stop with a cancelled request context failed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cancelled request prevented systemctl stop: %v", err)
	}
}

func TestFirstBindFailureJournalUsesPerDeviceReleaseOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "takeover-state.json")
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", path)
	failures := []dpdkTakeoverFailure{
		{LinuxInterface: "eth1", PCIAddress: "0000:03:00.0", Mode: takeoverModeDPDKUIO, Reason: "boom"},
		{LinuxInterface: "eth2", PCIAddress: "0000:04:00.0", Mode: takeoverModeDPDKUIO, Reason: "boom"},
	}
	(&Server{}).recordTakeoverFirstBindFailures(failures, map[string]bool{"eth1": false, "eth2": true})
	first, ok := lastTakeoverTransition("eth1")
	if !ok || first.RollbackResult != takeoverRollbackRestoreFailed {
		t.Fatalf("eth1 journal = %#v, want restore_failed", first)
	}
	second, ok := lastTakeoverTransition("eth2")
	if !ok || second.RollbackResult != takeoverRollbackRestored || second.Status != takeoverStatusRolledBack {
		t.Fatalf("eth2 journal = %#v, want restored", second)
	}
}

func TestApplyTakeoverSwitchRunsThePrivilegedStepsAndJournalsHandles(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "steps.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$(basename \"$0\") $*\" >> \"$STEP_MARKER\"\nexit 0\n"
	for _, file := range []string{
		filepath.Join(root, "prepare-vfio.sh"),
		filepath.Join(root, "stop.sh"),
		filepath.Join(root, "restart.sh"),
		filepath.Join(root, "release-dpdk-port.sh"),
	} {
		if err := os.WriteFile(file, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vppctl := filepath.Join(root, "vppctl")
	vppctlScript := `#!/bin/sh
case "$*" in
  "show version") exit 0 ;;
  "show plugins") printf '%s\n' 'af_xdp_plugin.so' ;;
  "show hardware-interfaces") printf '%s\n' 'GigabitEthernet1/0/0 5 up GigabitEthernet1/0/0' '  pci: device 8086:125c subsystem 8086:0000 address 0000:03:00.0 numa 0' ;;
  "show interface")
    if [ -f "$DELETED_FLAG" ]; then
      printf '%s\n' 'GigabitEthernet1/0/0 5 up 9000/0/0/0'
    else
      printf '%s\n' 'lyroute-eth1 5 up 9000/0/0/0' 'GigabitEthernet1/0/0 5 up 9000/0/0/0'
    fi
    ;;
  "delete host-interface name eth1") : > "$DELETED_FLAG" ;;
  "show af_xdp") printf '%s\n' 'rx queue 0 fd 12' ;;
esac
exit 0
`
	if err := os.WriteFile(vppctl, []byte(vppctlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELETED_FLAG", filepath.Join(root, "af_packet-deleted"))
	report := filepath.Join(root, "capabilities.json")
	body := `{"proofs":[{"linux_interface":"eth1","candidates":[{"tier":"vpp_dpdk","hook":"dpdk","mode":"uio_pci_generic","transport":"uio","source":"runtime_probe","runtime_verified":true,"observed_at":"2026-10-30T10:00:00Z","valid_until":"2027-10-30T11:00:00Z","pci_address":"0000:03:00.0","kernel_driver":"igc","uio_pci_available":true,"hugepages_available":true,"dpdk_plugin_available":true}]}]}`
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEP_MARKER", marker)
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(root, "takeover-state.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", report)
	t.Setenv("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", filepath.Join(root, "prepare-vfio.sh"))
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/sh "+filepath.Join(root, "stop.sh"))
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", "/bin/sh "+filepath.Join(root, "restart.sh"))
	t.Setenv("LY_ROUTE_DPDK_RELEASE_COMMAND", filepath.Join(root, "release-dpdk-port.sh"))
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_IP_COMMAND", "/bin/true")

	server := &Server{}
	if err := server.applyTakeoverSwitch(context.Background(), takeoverTestSnapshot(), takeoverModeDPDKUIO, false); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	steps, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stop.sh", "prepare-vfio.sh", "restart.sh"} {
		if !strings.Contains(string(steps), want) {
			t.Fatalf("step %s did not run: %s", want, steps)
		}
	}
	transition, ok := lastTakeoverTransition("eth1")
	if !ok {
		t.Fatal("switch was not journaled")
	}
	if transition.Status != takeoverStatusApplied || transition.FromMode != takeoverModeAFPacket || transition.ToMode != takeoverModeDPDKUIO {
		t.Fatalf("transition = %#v", transition)
	}
	if transition.SWIfIndex != 5 || len(transition.QueueFDs) != 1 || transition.QueueFDs[0] != 12 {
		t.Fatalf("runtime handles = %#v", transition)
	}
	if transition.Netdev != "eth1" || transition.KernelDriver != "igc" || transition.ToDriver != "uio_pci_generic" {
		t.Fatalf("identity = %#v", transition)
	}
	if transition.RollbackResult != takeoverRollbackNotNeeded {
		t.Fatalf("rollback result = %q", transition.RollbackResult)
	}
}

func unknownDPDKCapability() vpp.CapabilityAssessment {
	return vpp.CapabilityAssessment{Status: vpp.CapabilityUnknown, Reason: "no runtime capability proof exists for dpdk_uio"}
}

func TestPlanAutomaticUnknownCapabilityAllowsTransactionalProbe(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, unknownDPDKCapability(), now)
	if err != nil {
		t.Fatalf("automatic unknown capability was rejected: %v", err)
	}
	if !transition.CapabilityUnprovenProbe || !transition.ProbeRequired || transition.ProbeResult != "pending" {
		t.Fatalf("transition = %#v, want an automatic probe", transition)
	}
	if transition.Capability != string(vpp.CapabilityUnknown) || transition.Status != takeoverStatusPlanned {
		t.Fatalf("transition = %#v", transition)
	}
}

func TestPlanAutomaticUnsupportedCapabilityIsStillAttemptedOnce(t *testing.T) {
	// The capability report is advisory: the plan is backend-owned and a stale
	// "unsupported" verdict must not permanently block the automatic takeover.
	// The attempt either succeeds or fails and rolls back.
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, false, vpp.CapabilityAssessment{
		Status: vpp.CapabilityUnsupported,
		Reason: "uio_pci_generic is unavailable",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("automatic plan failed: %v", err)
	}
	if transition.Status != takeoverStatusPlanned || transition.ProbeRequired {
		t.Fatalf("transition = %#v, want a plain attempt without a probe flag", transition)
	}
}

func TestExecuteProbeFailureRollsBackAndRecoversVPP(t *testing.T) {
	now := time.Now().UTC()
	transition, err := planTakeoverTransition(takeoverTestSnapshot(), takeoverModeDPDKUIO, true, true, unknownDPDKCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeTakeoverExecutor{failOn: "verify"}
	if err := executeTakeoverTransition(context.Background(), &transition, executor); err == nil {
		t.Fatal("expected the injected probe verification failure")
	}
	if !executor.recovered {
		t.Fatal("a failed probe did not recover VPP")
	}
	if transition.ProbeResult != "failed" || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want a failed probe with a restored rollback", transition)
	}
	if transition.Status != takeoverStatusFailed {
		t.Fatalf("status = %q", transition.Status)
	}
}

func TestApplyAutomaticTakeoverRecordsANegativeThenPositiveProof(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "steps.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$(basename \"$0\")\" >> \"$STEP_MARKER\"\nexit 0\n"
	for _, file := range []string{
		filepath.Join(root, "prepare-vfio.sh"),
		filepath.Join(root, "stop.sh"),
		filepath.Join(root, "restart.sh"),
		filepath.Join(root, "release-dpdk-port.sh"),
	} {
		if err := os.WriteFile(file, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vppctl := filepath.Join(root, "vppctl")
	vppctlScript := `#!/bin/sh
case "$*" in
  "show version") exit 0 ;;
  "show hardware-interfaces") printf '%s\n' 'GigabitEthernet1/0/0 5 up GigabitEthernet1/0/0' '  pci: device 8086:125c subsystem 8086:0000 address 0000:03:00.0 numa 0' ;;
  "show interface") printf '%s\n' 'GigabitEthernet1/0/0 5 up 9000/0/0/0' ;;
esac
exit 0
`
	if err := os.WriteFile(vppctl, []byte(vppctlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEP_MARKER", marker)
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(root, "takeover-state.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", filepath.Join(root, "missing-capabilities.json"))
	t.Setenv("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", filepath.Join(root, "prepare-vfio.sh"))
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/sh "+filepath.Join(root, "stop.sh"))
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", "/bin/sh "+filepath.Join(root, "restart.sh"))
	t.Setenv("LY_ROUTE_DPDK_RELEASE_COMMAND", filepath.Join(root, "release-dpdk-port.sh"))
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_IP_COMMAND", "/bin/true")
	t.Setenv("LY_ROUTE_VPP_VERSION", "test-vpp")
	t.Setenv("LY_ROUTE_VPP_RUNTIME_ID", "test-runtime")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")

	server := &Server{}
	if err := server.applyTakeoverSwitch(context.Background(), takeoverTestSnapshot(), takeoverModeDPDKUIO, false); err != nil {
		t.Fatalf("automatic takeover failed: %v", err)
	}
	transition, ok := lastTakeoverTransition("eth1")
	if !ok {
		t.Fatal("probe was not journaled")
	}
	if !transition.CapabilityUnprovenProbe || transition.ProbeResult != "verified" || transition.Status != takeoverStatusApplied {
		t.Fatalf("transition = %#v, want a verified probe", transition)
	}

	capabilities, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "takeover-capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	environment := server.capabilityEnvironmentFor("0000:03:00.0", "igc")
	if proof, ok := capabilities.ActiveSupported("igc", "0000:03:00.0", environment, time.Now().UTC()); !ok || proof.NativeUnsupported {
		t.Fatalf("capabilities = %#v, want a positive proof", capabilities.Devices)
	}
}
