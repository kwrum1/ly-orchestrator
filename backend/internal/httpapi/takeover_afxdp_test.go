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

// afxdpSwitchFixture builds a fake VPP whose attachment state is a key=value
// file. The model is deliberately close to the real plugin behaviour that the
// switch depends on:
//
//   - `delete host-interface name <netdev>` is a no-op that can fail silently
//     (cli.c ignores af_packet_delete_if's return value);
//   - `set interface name host-<netdev> <product>` removes the host name;
//   - `delete interface af_xdp sw_if_index <n>` is the unambiguous AF_XDP delete;
//   - an AF_XDP interface leaves an XDP program on the netdev, and any create
//     while one is attached fails with EBUSY (libxdp uses UPDATE_IF_NOEXIST);
//   - deleting the phy interface deletes the LCP pair.
type afxdpSwitchFixture struct {
	root   string
	server *Server
	snap   takeoverSnapshot
	log    string
	state  string
	xdp    string
}

func newAFXDPSwitchFixture(t *testing.T) afxdpSwitchFixture {
	t.Helper()
	previousInventory := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{{"system_name": "eth1", "id": "eth1", "pci": "0000:13:00.0", "mac": "00:aa:bb:cc:dd:01"}}
	}
	t.Cleanup(func() { hostInterfaceInventory = previousInventory })
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	root := t.TempDir()
	// AF_XDP is exercised on an ixgbe-family fixture: the rule library forbids
	// it for igb/igc/e1000e/r8169, and those drivers are covered by the
	// migration/rejection tests instead.
	device := filepath.Join(root, "bus/pci/devices/0000:13:00.0")
	driver := filepath.Join(root, "bus/pci/drivers/ixgbe")
	for _, directory := range []string{filepath.Join(root, "class/net/eth1"), device, driver} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(device, filepath.Join(root, "class/net/eth1/device")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "class/net/eth1/address"), []byte("00:aa:bb:cc:dd:01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "class/net/eth1/carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "vpp-state")
	if err := os.WriteFile(state, []byte("mode=af_packet\nstate=up\nrows=1\nhost=0\nlcp=lylan-eth1\nsw=5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	xdpState := filepath.Join(root, "xdp-state")
	if err := os.WriteFile(xdpState, []byte("prog=none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "vpp.log")
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$VPP_LOG"
S="$VPP_STATE"
get() { sed -n "s/^$1=//p" "$S" | head -1; }
put() { sed -i "/^$1=/d" "$S"; printf '%s=%s\n' "$1" "$2" >> "$S"; }
mode=$(get mode); rows=$(get rows); host=$(get host); lcp=$(get lcp); st=$(get state); sw=$(get sw)
rows=${rows:-0}; host=${host:-0}; sw=${sw:-5}
xdp() { sed -n 's/^prog=//p' "$VPP_XDP" | head -1; }
case "$*" in
  "show version") exit 0 ;;
  "show interface")
    printf '%s\n' '              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)     Counter          Count'
    i=0
    while [ "$i" -lt "$rows" ]; do printf 'lyroute-eth1 %s %s 9000/0/0/0\n' "$sw" "$st"; i=$((i+1)); done
    if [ "$host" = 1 ]; then printf 'host-eth1 %s up 9000/0/0/0\n' "$((sw+1))"; fi
    ;;
  "show interface lyroute-eth1") printf 'lyroute-eth1 %s %s 9000/0/0/0\n' "$sw" "$st" ;;
  "show interface address lyroute-eth1")
    printf 'lyroute-eth1 (%s):\n  L3 192.168.77.1/24\n' "$st"
    ;;
  "show hardware-interfaces lyroute-eth1")
    case "$mode" in
      af_packet) printf 'lyroute-eth1 %s up lyroute-eth1\n  Linux PACKET socket interface v3\n' "$sw" ;;
      af_xdp) printf 'lyroute-eth1 %s up lyroute-eth1\n  netdev eth1\n  flags: copy \n' "$sw"
        if [ "${VPP_DEVICE_ERROR:-0}" = 1 ]; then printf '  error tx poll() failed: Device or resource busy\n'; fi ;;
    esac
    ;;
  "show hardware-interfaces host-eth1") printf 'host-eth1 %s up host-eth1\n  Linux PACKET socket interface v3\n' "$((sw+1))" ;;
  "show hardware-interfaces")
    printf '%s\n' '              Name                Idx   Link  Hardware'
    ;;
  "show lcp")
    printf '%s\n' "lcp default netns '<unset>'" 'lcp lcp-sync on'
    if [ "$lcp" != none ] && [ -n "$lcp" ]; then
      printf 'itf-pair: [0] lyroute-eth1 tap4097 %s 9 type tap\n' "$lcp"
    fi
    ;;
  "lcp delete lyroute-eth1") put lcp none ;;
  "lcp create lyroute-eth1 host-if "*) put lcp "${5}" ;;
  "delete host-interface name eth1")
    if [ "${VPP_FAIL_DELETE:-0}" = 1 ]; then
      printf 'Host interface eth1 does not exist\n' >&2
      exit 1
    fi
    put mode none; put rows 0; put host 0; put lcp none
    ;;
  "delete interface af_xdp sw_if_index "*)
    if [ "$mode" = af_xdp ]; then put mode none; put rows 0; put lcp none; fi
    ;;
  "create interface af_xdp"*)
    if [ "${VPP_FAIL_CREATE:-0}" = 1 ]; then
      printf 'create interface af_xdp: xsk_socket__create() failed: Device or resource busy\n' >&2
      exit 1
    fi
    if [ "${VPP_FAIL_CREATE_ONCE:-0}" = 1 ] && [ ! -f "$VPP_ROOT/create-attempted" ]; then
      : > "$VPP_ROOT/create-attempted"
      printf 'create interface af_xdp: xsk_socket__create() failed: Device or resource busy\n' >&2
      exit 1
    fi
    # libxdp refuses to attach while any XDP program is already on the netdev.
    if [ "$(xdp)" != none ]; then
      printf 'create interface af_xdp: xsk_socket__create() failed: Device or resource busy\n' >&2
      exit 1
    fi
    printf '9\n'
    put mode af_xdp; put rows 1; put host 0; put sw 9
    sed -i 's/^prog=.*/prog=id:42/' "$VPP_XDP"
    if [ "${VPP_XDP_DOWN:-0}" = 1 ]; then put state down; fi
    ;;
  "create host-interface name eth1 cksum-gso-disable")
    if [ "${VPP_FAIL_HOST:-0}" = 1 ]; then
      printf 'create host-interface: Interface already exists\n' >&2
      exit 1
    fi
    put mode af_packet; put rows 1; put host 1; put sw 5
    ;;
  "set interface name host-eth1 lyroute-eth1") put host 0 ;;
  "set interface state lyroute-eth1 up")
    if [ "${VPP_XDP_DOWN:-0}" != 1 ]; then put state up; fi
    ;;
  "set interface mac address "*|"set interface mtu packet "*|"set interface ip address "*) ;;
  "detach-host-state") put host 0 ;;
esac
exit 0
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ip := filepath.Join(root, "ip")
	ipScript := `#!/bin/sh
printf 'ip %s\n' "$*" >> "$VPP_LOG"
case "$*" in
  "-details link show dev eth1")
    prog=$(sed -n 's/^prog=//p' "$VPP_XDP" | head -1)
    printf '2: eth1: <BROADCAST,MULTICAST,UP> mtu 1500 state UP\n'
    if [ "$prog" != none ] && [ -n "$prog" ]; then printf '    prog/xdp id %s\n' "${prog#id:}"; fi
    ;;
  "link set dev eth1 xdp off"|"link set dev eth1 xdpgeneric off"|"link set dev eth1 xdpdrv off")
    printf 'prog=none\n' > "$VPP_XDP"
    ;;
esac
exit 0
`
	if err := os.WriteFile(ip, []byte(ipScript), 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(root, "capabilities.json")
	body := `{"proofs":[{"linux_interface":"eth1","candidates":[{"tier":"vpp_native","hook":"af_xdp","mode":"copy","source":"runtime_probe","runtime_verified":true,"observed_at":"2026-10-30T10:00:00Z","valid_until":"2027-10-30T11:00:00Z","kernel_driver":"igc","native":true,"high_performance":true}]}]}`
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPP_LOG", log)
	t.Setenv("VPP_STATE", state)
	t.Setenv("VPP_XDP", xdpState)
	t.Setenv("VPP_ROOT", root)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "/bin/true")
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", "/bin/true")
	t.Setenv("LY_ROUTE_IP_COMMAND", ip)
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", report)
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(root, "takeover-state.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_NATIVE_REPAIR_STATE", filepath.Join(root, "native-repair.json"))
	t.Setenv("LY_ROUTE_AFXDP_RX_QUEUES", "")

	return afxdpSwitchFixture{
		root:   root,
		server: &Server{},
		snap: takeoverSnapshot{
			LinuxInterface: "eth1",
			Netdev:         "eth1",
			VPPInterface:   "lyroute-eth1",
			KernelDriver:   "ixgbe",
			CurrentMode:    takeoverModeAFPacket,
			Owned:          true,
			ModeConfirmed:  true,
		},
		log:   log,
		state: state,
		xdp:   xdpState,
	}
}

func (fixture afxdpSwitchFixture) commandLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (fixture afxdpSwitchFixture) stateValue(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

func (fixture afxdpSwitchFixture) mutateState(t *testing.T, values map[string]string) {
	t.Helper()
	data, err := os.ReadFile(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key := strings.SplitN(line, "=", 2)[0]
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
			seen[key] = true
			continue
		}
		lines = append(lines, line)
	}
	for key, value := range values {
		if !seen[key] {
			lines = append(lines, key+"="+value)
		}
	}
	if err := os.WriteFile(fixture.state, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fixture afxdpSwitchFixture) setXDP(t *testing.T, program string) {
	t.Helper()
	if err := os.WriteFile(fixture.xdp, []byte("prog="+program+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fixture afxdpSwitchFixture) live(t *testing.T) nativeAttachmentLive {
	t.Helper()
	return fixture.server.observeNativeAttachment(context.Background(), "eth1", "lyroute-eth1")
}

func (fixture afxdpSwitchFixture) verification(t *testing.T) []dataplane.DPDKVerification {
	t.Helper()
	state, err := dataplane.LoadDPDKVerification(filepath.Join(fixture.root, "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	return state.Interfaces
}

func TestAFXDPSwitchDeletesAFPacketBeforeCreatingAFXDP(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("AF_XDP switch failed: %v", err)
	}
	log := fixture.commandLog(t)
	deleteAt := strings.Index(log, "delete host-interface name eth1")
	createAt := strings.Index(log, "create interface af_xdp")
	if deleteAt < 0 || createAt < 0 {
		t.Fatalf("missing teardown/create command:\n%s", log)
	}
	if deleteAt > createAt {
		t.Fatalf("AF_XDP was created before the af_packet teardown:\n%s", log)
	}
	if strings.Contains(log, "delete interface af_packet") {
		t.Fatalf("the invalid af_packet delete syntax was used:\n%s", log)
	}
	// The product must not create a second interface with the same name.
	live := fixture.live(t)
	if len(live.Rows) != 1 || live.Hook != nativeHookAFXDP || live.State != "up" {
		t.Fatalf("live attachment = %s, want one up af_xdp interface", live.summary())
	}
	transition, ok := lastTakeoverTransition("eth1")
	if !ok || transition.Status != takeoverStatusApplied || transition.ToMode != takeoverModeAFXDPCopy {
		t.Fatalf("transition = %#v, want an applied AF_XDP switch", transition)
	}
	if transition.RollbackResult != takeoverRollbackNotNeeded {
		t.Fatalf("rollback result = %q", transition.RollbackResult)
	}
	verification := fixture.verification(t)
	if len(verification) != 1 || verification[0].Mode != takeoverModeAFXDPCopy || verification[0].Verification != "verified" {
		t.Fatalf("verification = %#v, want the AF_XDP mode only after the readback", verification)
	}
}

func TestAFXDPSwitchRebuildsTheLCPPairAfterTheNewAttachment(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("AF_XDP switch failed: %v", err)
	}
	log := fixture.commandLog(t)
	if !strings.Contains(log, "lcp create lyroute-eth1 host-if lylan-eth1") {
		t.Fatalf("the switch did not recreate the LCP pair:\n%s", log)
	}
	live := fixture.live(t)
	if live.LCPHost != "lylan-eth1" {
		t.Fatalf("live LCP pair = %q, want lylan-eth1 (%s)", live.LCPHost, live.summary())
	}
}

func TestAFXDPSwitchRemovesStaleXDPProgramBeforeCreating(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	// A previous AF_XDP owner died without deleting its interface: no VPP row
	// but the dispatcher is still attached to the netdev.
	fixture.mutateState(t, map[string]string{"mode": "none", "rows": "0", "host": "0", "lcp": "none"})
	fixture.setXDP(t, "id:9")
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("AF_XDP switch with a stale XDP program failed: %v", err)
	}
	log := fixture.commandLog(t)
	if !strings.Contains(log, "link set dev eth1 xdp off") {
		t.Fatalf("the stale XDP program was not detached:\n%s", log)
	}
	if live := fixture.live(t); live.Hook != nativeHookAFXDP {
		t.Fatalf("live attachment = %s, want af_xdp", live.summary())
	}
}

func TestAFXDPSwitchRetriesEBUSYOnceAfterCleanup(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	t.Setenv("VPP_FAIL_CREATE_ONCE", "1")
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("the switch did not retry a transient EBUSY: %v", err)
	}
	log := fixture.commandLog(t)
	if strings.Count(log, "create interface af_xdp") != 2 {
		t.Fatalf("want exactly one retry after EBUSY:\n%s", log)
	}
	if live := fixture.live(t); live.Hook != nativeHookAFXDP {
		t.Fatalf("live attachment = %s, want af_xdp", live.summary())
	}
}

func TestAFXDPSwitchEBUSYFailsAndRestoresAFPacket(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	t.Setenv("VPP_FAIL_CREATE", "1")
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err == nil {
		t.Fatal("an EBUSY create was reported as success")
	}
	transition, ok := lastTakeoverTransition("eth1")
	if !ok {
		t.Fatal("failed switch was not journaled")
	}
	if transition.Status != takeoverStatusFailed || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want failed with a restored rollback", transition)
	}
	if !strings.Contains(strings.Join(transition.Failures, " "), "Device or resource busy") {
		t.Fatalf("failures = %#v, want the vppctl EBUSY text", transition.Failures)
	}
	log := fixture.commandLog(t)
	if !strings.Contains(log, "create host-interface name eth1 cksum-gso-disable") {
		t.Fatalf("rollback did not recreate the af_packet interface:\n%s", log)
	}
	live := fixture.live(t)
	if live.Hook != nativeHookAFPacket || len(live.Rows) != 1 {
		t.Fatalf("live attachment after rollback = %s, want one af_packet interface", live.summary())
	}
	for _, entry := range fixture.verification(t) {
		if entry.Mode == takeoverModeAFXDPCopy {
			t.Fatalf("failed switch persisted an AF_XDP proof: %#v", entry)
		}
	}
	if entries := fixture.verification(t); len(entries) != 1 || entries[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want the rolled-back af_packet mode", entries)
	}
}

func TestAFXDPSwitchRequiresAnUpInterfaceBeforeApplied(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	t.Setenv("VPP_XDP_DOWN", "1")
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err == nil {
		t.Fatal("a down AF_XDP interface was reported as applied")
	}
	transition, _ := lastTakeoverTransition("eth1")
	if transition.Status != takeoverStatusFailed || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want failed/restored", transition)
	}
	log := fixture.commandLog(t)
	if strings.Index(log, "delete interface af_xdp sw_if_index 9") < 0 {
		t.Fatalf("rollback did not remove the partial AF_XDP interface by sw_if_index:\n%s", log)
	}
	if !strings.Contains(log, "create host-interface name eth1 cksum-gso-disable") {
		t.Fatalf("rollback did not restore af_packet:\n%s", log)
	}
	if entries := fixture.verification(t); len(entries) != 1 || entries[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want af_packet after the rollback", entries)
	}
}

func TestAFXDPSwitchFailureLeavesAFPacketVerification(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	before := dataplane.DPDKVerification{
		LinuxInterface: "eth1", Mode: takeoverModeAFPacket, Verification: "verified",
		FunctionalVerified: true, ObservedAt: time.Now().UTC(),
	}
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{before}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPP_FAIL_CREATE", "1")
	_ = fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false)
	verification := fixture.verification(t)
	if len(verification) != 1 || verification[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want the rolled-back af_packet mode", verification)
	}
}

func TestAFXDPSwitchDeleteFailureStopsBeforeCreatingAFXDP(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	t.Setenv("VPP_FAIL_DELETE", "1")
	err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false)
	if err == nil {
		t.Fatal("a failed af_packet delete was reported as success")
	}
	if !strings.Contains(err.Error(), "delete host-interface") {
		t.Fatalf("error = %v, want the delete failure text", err)
	}
	log := fixture.commandLog(t)
	if strings.Contains(log, "create interface af_xdp") {
		t.Fatalf("AF_XDP was created after the af_packet delete failed:\n%s", log)
	}
	transition, _ := lastTakeoverTransition("eth1")
	if transition.Status != takeoverStatusFailed || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want failed/restored", transition)
	}
}

func TestAFXDPSwitchRemovesDuplicateInterfacesBeforeCreating(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	// The historical broken state: af_packet and af_xdp both carry the product
	// name, so every name-based readback is ambiguous.
	fixture.mutateState(t, map[string]string{"mode": "af_packet", "rows": "2", "host": "0", "lcp": "lylan-eth1"})
	if !fixture.live(t).ambiguous() {
		t.Fatal("the fixture did not model the duplicate-interface state")
	}
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("the switch did not repair the duplicate state: %v", err)
	}
	live := fixture.live(t)
	if len(live.Rows) != 1 || live.Hook != nativeHookAFXDP {
		t.Fatalf("live attachment = %s, want exactly one af_xdp interface", live.summary())
	}
	log := fixture.commandLog(t)
	if !strings.Contains(log, "delete interface af_xdp sw_if_index") {
		t.Fatalf("duplicates were not removed by sw_if_index:\n%s", log)
	}
}

func TestAFXDPSwitchIsIdempotentAcrossRepeatedApplies(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	ctx := context.Background()
	if err := fixture.server.applyTakeoverSwitch(ctx, fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("first switch failed: %v", err)
	}
	first := len(fixture.commandLog(t))
	// A repeated apply sees the live af_xdp attachment and must converge
	// without touching the interface.
	snapshot := fixture.server.detectTakeoverSnapshot(ctx, "eth1", map[string]bool{"lyroute-eth1": true})
	if snapshot.CurrentMode != takeoverModeAFXDPCopy || !snapshot.ModeConfirmed {
		t.Fatalf("snapshot = %#v, want the live af_xdp mode", snapshot)
	}
	if err := fixture.server.applyTakeoverSwitch(ctx, snapshot, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("repeated switch failed: %v", err)
	}
	log := fixture.commandLog(t)[first:]
	if strings.Contains(log, "delete host-interface") || strings.Contains(log, "create interface af_xdp") || strings.Contains(log, "lcp delete") {
		t.Fatalf("a repeated apply re-created the interface:\n%s", log)
	}
	if live := fixture.live(t); len(live.Rows) != 1 {
		t.Fatalf("live attachment = %s, want a single interface", live.summary())
	}
}

func TestAFXDPSwitchBackToAFPacketIsVerifiedLive(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	ctx := context.Background()
	if err := fixture.server.applyTakeoverSwitch(ctx, fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("switch to af_xdp failed: %v", err)
	}
	snapshot := fixture.server.detectTakeoverSnapshot(ctx, "eth1", map[string]bool{"lyroute-eth1": true})
	if err := fixture.server.applyTakeoverSwitch(ctx, snapshot, takeoverModeAFPacket, false); err != nil {
		t.Fatalf("switch back to af_packet failed: %v", err)
	}
	live := fixture.live(t)
	if len(live.Rows) != 1 || live.Hook != nativeHookAFPacket {
		t.Fatalf("live attachment = %s, want one af_packet interface", live.summary())
	}
	if entries := fixture.verification(t); len(entries) != 1 || entries[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want af_packet", entries)
	}
	log := fixture.commandLog(t)
	if !strings.Contains(log, "delete interface af_xdp sw_if_index 9") {
		t.Fatalf("the AF_XDP interface was not removed by sw_if_index:\n%s", log)
	}
}

func TestTakeoverSnapshotPrefersTheLiveAttachmentOverAStaleVerification(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	// A previous failed switch recorded af_xdp although the device carries
	// af_packet. The snapshot must trust the live readback.
	fixture.mutateState(t, map[string]string{"mode": "af_packet", "rows": "1", "host": "0"})
	fixture.setXDP(t, "none")
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface: "eth1", Mode: takeoverModeAFXDPCopy, Verification: "verified", FunctionalVerified: true,
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.server.detectTakeoverSnapshot(context.Background(), "eth1", map[string]bool{"lyroute-eth1": true})
	if snapshot.CurrentMode != takeoverModeAFPacket || !snapshot.ModeConfirmed {
		t.Fatalf("snapshot = %#v, want the live af_packet mode", snapshot)
	}
}

func TestPreferCommittedNativeCandidatesUsesLiveAFXDPOverRecordedAFPacket(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	fixture.mutateState(t, map[string]string{"mode": "af_xdp", "rows": "1", "host": "0"})
	fixture.setXDP(t, "id:42")
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface: "eth1", PCIAddress: inventoryPCI("eth1"), Mode: takeoverModeAFPacket, Verification: "verified", FunctionalVerified: true,
	}}); err != nil {
		t.Fatal(err)
	}
	request := takeoverSelectionRequest(time.Now().UTC())
	request.Assignments[0].LinuxInterface = "eth1"
	fixture.server.preferCommittedNativeCandidates(context.Background(), &request)
	if len(request.Assignments[0].Candidates) != 1 {
		t.Fatalf("candidates = %#v, want a single live proof", request.Assignments[0].Candidates)
	}
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	attachment := selectedAttachment(t, path)
	if attachment.Hook != vpp.NativeHookAFXDP {
		t.Fatalf("attachment = %#v, want the live AF_XDP attachment", attachment)
	}
	commands := strings.Join(vpp.DataplaneAttachOperation("req", attachment).VPPCtlCommands, "\n")
	if strings.Contains(commands, "create host-interface") {
		t.Fatalf("a live AF_XDP port still generated an af_packet create:\n%s", commands)
	}
	// The stale record is corrected from the live readback.
	if entries := fixture.verification(t); len(entries) != 1 || entries[0].Mode != takeoverModeAFXDPCopy {
		t.Fatalf("verification = %#v, want the live af_xdp mode", entries)
	}
}

func TestPreferCommittedNativeCandidatesRebuildsAFXDPWhenNothingIsLive(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	fixture.mutateState(t, map[string]string{"mode": "none", "rows": "0", "host": "0", "lcp": "none"})
	fixture.setXDP(t, "none")
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface: "eth1", PCIAddress: inventoryPCI("eth1"), Mode: takeoverModeAFXDPCopy, Verification: "verified", FunctionalVerified: true,
	}}); err != nil {
		t.Fatal(err)
	}
	request := takeoverSelectionRequest(time.Now().UTC())
	request.Assignments[0].LinuxInterface = "eth1"
	fixture.server.preferCommittedNativeCandidates(context.Background(), &request)
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	attachment := selectedAttachment(t, path)
	if attachment.Hook != vpp.NativeHookAFXDP {
		t.Fatalf("attachment = %#v, want the recorded AF_XDP mode to be rebuilt (not af_packet)", attachment)
	}
}

func TestAFXDPSwitchRollsBackWhenVPPReportsADeviceError(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	// The live igc port accepted the copy-mode socket but VPP reported
	// "error tx poll() failed: Device or resource busy": attached, not
	// forwarding. The switch must not report that as applied.
	t.Setenv("VPP_DEVICE_ERROR", "1")
	swErr := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false)
	if swErr == nil {
		t.Fatal("a device in VPP's error state was reported as an applied switch")
	}
	transition, _ := lastTakeoverTransition("eth1")
	if transition.Status != takeoverStatusFailed || transition.RollbackResult != takeoverRollbackRestored {
		t.Fatalf("transition = %#v, want failed/restored", transition)
	}
	if !strings.Contains(strings.Join(transition.Failures, " "), "tx poll() failed") {
		t.Fatalf("failures = %#v, want VPP's device error", transition.Failures)
	}
	var hardware *takeoverHardwareError
	if !errors.As(swErr, &hardware) {
		t.Fatalf("error = %v, want a takeoverHardwareError so the watcher backs off", swErr)
	}
	if got := switchFailureCooldown(swErr); got != dataplaneSwitchHardwareCooldown {
		t.Fatalf("cooldown = %v, want %v", got, dataplaneSwitchHardwareCooldown)
	}
	if entries := fixture.verification(t); len(entries) != 1 || entries[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want af_packet", entries)
	}
	if live := fixture.live(t); live.Hook != nativeHookAFPacket {
		t.Fatalf("live attachment = %s, want af_packet after the rollback", live.summary())
	}
}

func TestAFXDPSwitchRollbackFailureRecordsNeedsRepair(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	// Both the AF_XDP create and the af_packet recovery fail: the port is left
	// unattached and the operator must be told exactly that.
	t.Setenv("VPP_FAIL_CREATE", "1")
	t.Setenv("VPP_FAIL_HOST", "1")
	err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false)
	if err == nil {
		t.Fatal("a switch that could not be rolled back was reported as success")
	}
	transition, _ := lastTakeoverTransition("eth1")
	if transition.Status != takeoverStatusRollbackFailed || !transition.NeedsRepair {
		t.Fatalf("transition = %#v, want rollback_failed + needs_repair", transition)
	}
	for _, entry := range fixture.verification(t) {
		if entry.Mode == takeoverModeAFXDPCopy {
			t.Fatalf("a failed rollback kept an AF_XDP proof: %#v", entry)
		}
	}
	record, readErr := os.ReadFile(filepath.Join(fixture.root, "native-repair.json"))
	if readErr != nil {
		t.Fatalf("no repair record was written: %v", readErr)
	}
	if !strings.Contains(string(record), "eth1") {
		t.Fatalf("repair record does not name the port: %s", record)
	}
}

func TestAFXDPSwitchUsesOneRXQueueForCopyMode(t *testing.T) {
	t.Setenv("LY_ROUTE_AFXDP_RX_QUEUES", "")
	if got := afxdpRXQueues(takeoverModeAFXDPCopy); got != "1" {
		t.Fatalf("copy mode rx queues = %q, want 1", got)
	}
	if got := afxdpRXQueues(takeoverModeAFXDPZeroCopy); got != "all" {
		t.Fatalf("zero-copy rx queues = %q, want all", got)
	}
	t.Setenv("LY_ROUTE_AFXDP_RX_QUEUES", "all")
	if got := afxdpRXQueues(takeoverModeAFXDPCopy); got != "all" {
		t.Fatalf("override rx queues = %q, want all", got)
	}
}

func TestAFXDPSwitchDetectsAmbiguousDuplicateNames(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	fixture.mutateState(t, map[string]string{"mode": "af_packet", "rows": "2", "host": "1"})
	live := fixture.live(t)
	if !live.ambiguous() {
		t.Fatalf("live = %s, want ambiguous", live.summary())
	}
	if live.matches(takeoverModeAFPacket) {
		t.Fatal("an ambiguous duplicate was reported as a matching attachment")
	}
}

func TestAFXDPSwitchDeleteUsesTheLinuxNetdevName(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	if err := fixture.server.applyTakeoverSwitch(context.Background(), fixture.snap, takeoverModeAFXDPCopy, false); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	log := fixture.commandLog(t)
	for _, wrong := range []string{"delete host-interface name host-eth1", "delete host-interface name lyroute-eth1"} {
		if strings.Contains(log, wrong) {
			t.Fatalf("the delete used a non-netdev name: %q\n%s", wrong, log)
		}
	}
	if !strings.Contains(log, "delete host-interface name eth1") {
		t.Fatalf("no delete with the Linux netdev name:\n%s", log)
	}
}

func TestTransitionSummaryExposesNeedsRepair(t *testing.T) {
	summary := takeoverTransitionSummary(takeoverTransition{ToMode: takeoverModeAFXDPCopy, NeedsRepair: true, Commands: []string{"cmd: create interface af_xdp"}})
	if summary["needs_repair"] != true {
		t.Fatalf("summary = %#v, want needs_repair", summary)
	}
	if _, ok := summary["commands"]; !ok {
		t.Fatalf("summary = %#v, want the command audit trail", summary)
	}
}
