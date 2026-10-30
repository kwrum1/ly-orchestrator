package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// The native (non-destructive) attachment modes share the Linux netdev with the
// kernel. Exactly one VPP plugin may own the netdev queues at a time, and VPP
// does not track that ownership across a restart: an AF_XDP dispatcher that was
// never detached stays attached to the netdev (it is kernel state, not process
// state) and makes the next xsk_socket__create fail with EBUSY. That is why the
// switch below is a live, verified teardown/build and not a VPP restart.
const (
	nativeHookAFPacket = "af_packet"
	nativeHookAFXDP    = "af_xdp"
	nativeHookRDMA     = "rdma"
)

// nativeSwitchMu serialises every mutation of a native attachment. The apply,
// the watcher, the rollback safety net and the recovery path all used to touch
// the same VPP interface, which produced duplicate interfaces, EBUSY and a
// "Cannot find device" storm. One owner at a time: a would-be second owner
// skips this pass and retries on its next tick.
var nativeSwitchMu sync.Mutex

// errNativeSwitchBusy is returned when another component already owns the
// native switch lock. It is not a dataplane failure.
var errNativeSwitchBusy = fmt.Errorf("another dataplane switch is in progress")

func (server *Server) tryNativeSwitchLock() bool { return nativeSwitchMu.TryLock() }

func (server *Server) unlockNativeSwitch() { nativeSwitchMu.Unlock() }

// vppInterfaceRow is one row of `show interface`.
type vppInterfaceRow struct {
	Name  string
	Index uint32
	State string
}

// parseVPPInterfaceRows reads the interface table. Counter lines and CLI error
// text are ignored instead of being parsed as interfaces.
func parseVPPInterfaceRows(output string) []vppInterfaceRow {
	rows := []vppInterfaceRow{}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" ||
			strings.Contains(trimmed, "unknown input") ||
			strings.Contains(trimmed, "unknown interface") ||
			strings.HasPrefix(trimmed, "Name") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		index, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			continue
		}
		state := ""
		for _, field := range fields[2:] {
			if field == "up" || field == "down" {
				state = field
				break
			}
		}
		rows = append(rows, vppInterfaceRow{Name: fields[0], Index: uint32(index), State: state})
	}
	return rows
}

func rowsNamed(rows []vppInterfaceRow, name string) []vppInterfaceRow {
	name = strings.TrimSpace(name)
	matched := []vppInterfaceRow{}
	if name == "" {
		return matched
	}
	for _, row := range rows {
		if row.Name == name {
			matched = append(matched, row)
		}
	}
	return matched
}

// vppHardwareHook identifies the VPP plugin that owns an interface from
// `show hardware-interfaces <name>`. The markers are the plugin's own device
// format, which is part of the VPP build and survives CLI/plugin quirks (unlike
// `show af_xdp`, which does not exist in VPP 26.06 at all).
func vppHardwareHook(output string) string {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "linux packet socket interface"):
		return nativeHookAFPacket
	case strings.Contains(lower, "netdev "):
		// af_xdp/device.c prints "netdev <linux-ifname>" as its device detail.
		return nativeHookAFXDP
	case strings.Contains(lower, "rdma"):
		return nativeHookRDMA
	}
	return ""
}

// vppHardwareNetdev extracts the Linux netdev named by an AF_XDP hardware
// readback ("netdev enp4s0"). It is empty for other plugins.
func vppHardwareNetdev(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != "netdev" || index+1 >= len(fields) {
				continue
			}
			return strings.TrimSpace(fields[index+1])
		}
	}
	return ""
}

// vppHardwareDeviceError extracts VPP's device error line from a hardware
// readback. Both af_packet and af_xdp print it as "error <text>"; an empty
// string means the device is healthy.
func vppHardwareDeviceError(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.ToLower(fields[0]) != "error" {
			continue
		}
		return strings.TrimSpace(strings.Join(fields[1:], " "))
	}
	return ""
}

// parseLCPHostInterface returns the Linux tap paired with a VPP interface from
// `show lcp` ("itf-pair: [1] lyroute-enp4s0 tap4097 lylan-enp4s0 9 type tap").
func parseLCPHostInterface(output, vppName string) string {
	vppName = strings.TrimSpace(vppName)
	if vppName == "" {
		return ""
	}
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "itf-pair:") {
			continue
		}
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != vppName || index+2 >= len(fields) {
				continue
			}
			return strings.TrimSpace(fields[index+2])
		}
	}
	return ""
}

// nativeAttachmentLive is what VPP and the kernel actually have for one netdev.
// Every field is observed; nothing here is read from a persisted record.
type nativeAttachmentLive struct {
	Netdev     string
	VPPName    string
	Reachable  bool
	Rows       []vppInterfaceRow
	HostRows   []vppInterfaceRow
	Hook       string
	State      string
	SWIfIndex  uint32
	LCPHost    string
	XDPProgram string
	// ZeroCopy/ZeroCopyKnown carry the AF_XDP "zero-copy" device flag. Copy and
	// zero-copy are the same plugin, so a copy switch must not be accepted as a
	// verified zero-copy attachment (or the reverse).
	ZeroCopy      bool
	ZeroCopyKnown bool
	// DeviceError is VPP's own device error text from the hardware readback
	// ("error tx poll() failed: Device or resource busy"). A device in the error
	// state is attached but not forwarding, which is exactly what a copy-mode
	// AF_XDP socket on a NIC without a usable generic TX path looks like.
	DeviceError string
	// NetdevSeen is the netdev named by the AF_XDP hardware readback. A create
	// that reports success for a different netdev is not an attachment.
	NetdevSeen string
	Notes      []string
	ReadError  string
}

func (live nativeAttachmentLive) present() bool {
	return len(live.Rows) > 0 || len(live.HostRows) > 0
}

// ambiguous reports a state with more than one product-named interface, or the
// double af_packet+af_xdp state that makes every downstream readback useless.
func (live nativeAttachmentLive) ambiguous() bool {
	return len(live.Rows) > 1 || (len(live.Rows) > 0 && len(live.HostRows) > 0)
}

// mode maps the observed attachment to a takeover mode. It never guesses: an
// interface whose plugin cannot be read back stays "" and must not be treated as
// a working attachment.
func (live nativeAttachmentLive) mode() string {
	switch live.Hook {
	case nativeHookAFPacket:
		return takeoverModeAFPacket
	case nativeHookRDMA:
		return takeoverModeRDMA
	case nativeHookAFXDP:
		if live.ZeroCopyKnown && live.ZeroCopy {
			return takeoverModeAFXDPZeroCopy
		}
		return takeoverModeAFXDPCopy
	}
	return ""
}

func (live nativeAttachmentLive) matches(mode string) bool {
	mode = strings.TrimSpace(mode)
	if mode == "" || live.ambiguous() || len(live.Rows) != 1 {
		return false
	}
	switch mode {
	case takeoverModeAFPacket:
		return live.Hook == nativeHookAFPacket
	case takeoverModeAFXDPCopy:
		return live.Hook == nativeHookAFXDP && !(live.ZeroCopyKnown && live.ZeroCopy)
	case takeoverModeAFXDPZeroCopy:
		return live.Hook == nativeHookAFXDP && (!live.ZeroCopyKnown || live.ZeroCopy)
	case takeoverModeRDMA:
		return live.Hook == nativeHookRDMA
	}
	return false
}

func (live nativeAttachmentLive) summary() string {
	parts := []string{fmt.Sprintf("reachable=%t", live.Reachable)}
	parts = append(parts, fmt.Sprintf("rows=%d", len(live.Rows)))
	parts = append(parts, fmt.Sprintf("host_rows=%d", len(live.HostRows)))
	if live.Hook != "" {
		parts = append(parts, "hook="+live.Hook)
	}
	if live.State != "" {
		parts = append(parts, "state="+live.State)
	}
	if live.SWIfIndex != 0 {
		parts = append(parts, fmt.Sprintf("sw_if_index=%d", live.SWIfIndex))
	}
	if live.LCPHost != "" {
		parts = append(parts, "lcp_host="+live.LCPHost)
	}
	if live.XDPProgram != "" {
		parts = append(parts, "xdp_program="+live.XDPProgram)
	}
	if live.ReadError != "" {
		parts = append(parts, "read_error="+live.ReadError)
	}
	return strings.Join(parts, " ")
}

// observeNativeAttachment reads the live state of one port. It is cheap enough
// to call before and after every mutation and is the only authority the switch
// trusts.
func (server *Server) observeNativeAttachment(ctx context.Context, netdev, vppName string) nativeAttachmentLive {
	live := nativeAttachmentLive{Netdev: strings.TrimSpace(netdev), VPPName: strings.TrimSpace(vppName)}
	if live.VPPName == "" && live.Netdev != "" {
		live.VPPName = "lyroute-" + live.Netdev
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	live.Reachable = server.vppReachable(readCtx)
	if !live.Reachable {
		live.ReadError = "VPP is not reachable"
		return live
	}
	interfaces, err := execCommandCombined(readCtx, vppctl, "show", "interface")
	if err != nil && strings.TrimSpace(interfaces) == "" {
		live.ReadError = "show interface: " + err.Error()
		return live
	}
	rows := parseVPPInterfaceRows(interfaces)
	live.Rows = rowsNamed(rows, live.VPPName)
	live.HostRows = rowsNamed(rows, "host-"+live.Netdev)
	switch {
	case len(live.Rows) == 1:
		live.SWIfIndex = live.Rows[0].Index
		live.State = live.Rows[0].State
		hardware, hardwareErr := execCommandCombined(readCtx, vppctl, "show", "hardware-interfaces", live.VPPName)
		if hardwareErr != nil && strings.TrimSpace(hardware) == "" {
			live.ReadError = "show hardware-interfaces: " + hardwareErr.Error()
		}
		live.Hook = vppHardwareHook(hardware)
		live.DeviceError = vppHardwareDeviceError(hardware)
		if live.Hook == nativeHookAFXDP {
			live.NetdevSeen = vppHardwareNetdev(hardware)
			lower := strings.ToLower(hardware)
			live.ZeroCopyKnown = strings.Contains(lower, "flags:")
			live.ZeroCopy = strings.Contains(lower, "zero-copy")
		}
	case len(live.Rows) == 0 && len(live.HostRows) == 1:
		// A host interface that was created but never renamed still counts as
		// the af_packet owner of this netdev.
		live.SWIfIndex = live.HostRows[0].Index
		live.State = live.HostRows[0].State
		if hardware, hardwareErr := execCommandCombined(readCtx, vppctl, "show", "hardware-interfaces", "host-"+live.Netdev); hardwareErr == nil {
			live.Hook = vppHardwareHook(hardware)
		}
	}
	if lcp, lcpErr := execCommandCombined(readCtx, vppctl, "show", "lcp"); lcpErr == nil {
		live.LCPHost = parseLCPHostInterface(lcp, live.VPPName)
	}
	if live.Netdev != "" {
		live.XDPProgram = netdevXDPProgram(readCtx, live.Netdev)
		if live.XDPProgram != "" && len(live.Rows) == 0 {
			live.Notes = append(live.Notes, "netdev carries an XDP program but VPP has no interface for it (stale AF_XDP owner)")
		}
	}
	return live
}

// netdevXDPProgram returns "id:<n>" when the kernel netdev still has an XDP
// program attached, which is exactly what makes xsk_socket__create fail with
// EBUSY after a crashed or partially deleted AF_XDP interface.
func netdevXDPProgram(ctx context.Context, netdev string) string {
	if strings.TrimSpace(netdev) == "" {
		return ""
	}
	ip := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
	output, err := execCommandCombined(ctx, ip, "-details", "link", "show", "dev", netdev)
	if err != nil {
		return ""
	}
	return parseXDPProgramID(output)
}

func execCommandCombined(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// interfaceL3Config is the interface-level state VPP destroys together with the
// interface. It is captured before the teardown and replayed after the build so
// a switch (and especially a rollback) does not silently drop the LAN address.
type interfaceL3Config struct {
	Addresses []string `json:"addresses,omitempty"`
	MTU       int      `json:"mtu,omitempty"`
}

func (server *Server) captureInterfaceL3(ctx context.Context, vppName string) interfaceL3Config {
	config := interfaceL3Config{}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	if output, err := execCommandCombined(readCtx, vppctl, "show", "interface", "address", vppName); err == nil {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			for _, field := range fields {
				if !strings.Contains(field, "/") || strings.Contains(field, "unknown") {
					continue
				}
				if prefix, err := strconv.Atoi(strings.SplitN(field, "/", 2)[0]); err != nil || prefix <= 0 {
					continue
				}
				config.Addresses = append(config.Addresses, field)
				break
			}
		}
	}
	if output, err := execCommandCombined(readCtx, vppctl, "show", "interface", vppName); err == nil {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[0] != vppName {
				continue
			}
			for _, field := range fields[2:] {
				if !strings.Contains(field, "/") {
					continue
				}
				if value, err := strconv.Atoi(strings.SplitN(field, "/", 2)[0]); err == nil && value > 0 {
					config.MTU = value
				}
				break
			}
			break
		}
	}
	return config
}

// restoreInterfaceL3 replays the captured interface-level configuration. It is
// best effort by design: a missing address is reported, never guessed.
func (executor systemTakeoverExecutor) restoreInterfaceL3(ctx context.Context, vppName string, config interfaceL3Config) []string {
	failures := []string{}
	if config.MTU > 0 && config.MTU != 1500 {
		if output, err := executor.vppCommand(ctx, "set", "interface", "mtu", "packet", strconv.Itoa(config.MTU), vppName); err != nil {
			failures = append(failures, "restore MTU "+strconv.Itoa(config.MTU)+": "+err.Error()+": "+firstLine(output, 200))
		}
	}
	for _, address := range config.Addresses {
		if output, err := executor.vppCommand(ctx, "set", "interface", "ip", "address", vppName, address); err != nil {
			failures = append(failures, "restore address "+address+": "+err.Error()+": "+firstLine(output, 200))
		}
	}
	return failures
}

// afxdpRXQueues is the num-rx-queues policy. Copy mode is pinned to one queue:
// on igc the kernel exposes four queues and `all` registers one UMEM per queue
// while VPP tolerates a *partial* queue failure for `all` (af_xdp_create_if),
// leaving a silently half-built interface and per-queue EBUSY. Zero-copy keeps
// `all` because there one socket per queue is the whole point.
func afxdpRXQueues(mode string) string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_AFXDP_RX_QUEUES")); value != "" {
		return value
	}
	if strings.TrimSpace(mode) == takeoverModeAFXDPZeroCopy {
		return "all"
	}
	return "1"
}

func afxdpModeFlag(mode string) string {
	if strings.TrimSpace(mode) == takeoverModeAFXDPZeroCopy {
		return "zero-copy"
	}
	return "no-zero-copy"
}

// detachStaleXDPProgram removes an XDP program left on the netdev. libxdp
// attaches its dispatcher with XDP_FLAGS_UPDATE_IF_NOEXIST, so any leftover
// program turns every later xsk_socket__create into EBUSY. Detaching is only
// attempted when VPP has no interface for the netdev, so a live attachment is
// never disturbed.
func (executor systemTakeoverExecutor) detachStaleXDPProgram(ctx context.Context, netdev string, live nativeAttachmentLive) error {
	if live.XDPProgram == "" || live.present() {
		return nil
	}
	ip := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
	attempts := [][]string{
		{"link", "set", "dev", netdev, "xdp", "off"},
		{"link", "set", "dev", netdev, "xdpgeneric", "off"},
		{"link", "set", "dev", netdev, "xdpdrv", "off"},
	}
	for _, args := range attempts {
		if _, err := exec.CommandContext(ctx, ip, args...).CombinedOutput(); err != nil {
			continue
		}
		if netdevXDPProgram(ctx, netdev) == "" {
			log.Printf("dataplane switch: detached stale %s from %s with `ip %s`", live.XDPProgram, netdev, strings.Join(args, " "))
			return nil
		}
	}
	if program := netdevXDPProgram(ctx, netdev); program != "" {
		return fmt.Errorf("netdev %s still carries XDP program %s after detach attempts; an AF_XDP create would fail with EBUSY", netdev, program)
	}
	return nil
}

// teardownNativeAttachment deletes every VPP attachment for the netdev and
// proves with a readback that nothing is left. The commands it ran are returned
// so a failed switch can report exactly what was attempted.
func (executor systemTakeoverExecutor) teardownNativeAttachment(ctx context.Context, transition *takeoverTransition, live nativeAttachmentLive) ([]string, error) {
	netdev := strings.TrimSpace(transition.Netdev)
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+netdev)
	if netdev == "" {
		return nil, fmt.Errorf("the Linux netdev is unknown; cannot tear down the native attachment")
	}
	commands := []string{}
	if !live.Reachable {
		return commands, fmt.Errorf("VPP is not reachable; cannot tear down %s", netdev)
	}
	// The LCP pair is deleted explicitly before the phy interface: VPP also
	// removes it through VNET_SW_INTERFACE_ADD_DEL_FUNCTION (linux-cp
	// lcp_itf_interface_add_del), but the host tap lylan-<netdev> must be gone
	// before the rebuild so its name and addresses are re-created, not inherited.
	if live.LCPHost != "" {
		command := "lcp delete " + vppName
		commands = append(commands, command)
		if output, err := executor.vppCommand(ctx, "lcp", "delete", vppName); err != nil {
			log.Printf("dataplane switch: %s: %v: %s (the phy interface delete removes the pair as well)", command, err, firstLine(output, 200))
		}
	}
	// Ambiguous duplicates are removed row by row. `delete interface af_xdp`
	// takes a sw_if_index, which is the only unambiguous handle when two
	// interfaces share the product name; a row that is not AF_XDP rejects the
	// command and is removed by the af_packet delete below instead.
	for _, row := range live.Rows {
		if row.Index == 0 {
			continue
		}
		commands = append(commands, fmt.Sprintf("delete interface af_xdp sw_if_index %d", row.Index))
		if output, err := executor.vppCommand(ctx, "delete", "interface", "af_xdp", "sw_if_index", strconv.FormatUint(uint64(row.Index), 10)); err != nil {
			log.Printf("dataplane switch: delete interface af_xdp sw_if_index %d: %v: %s", row.Index, err, firstLine(output, 200))
		}
	}
	if len(live.HostRows) > 0 || live.Hook == nativeHookAFPacket || len(live.Rows) > 0 {
		// `delete host-interface name <linux-netdev>` is the only valid form:
		// af_packet/cli.c matches the host_if_name it was created with, so
		// `host-enp4s0` and the renamed `lyroute-enp4s0` are both wrong. The CLI
		// ignores af_packet_delete_if's return value, which is why the readback
		// below (and not the command result) decides whether it worked.
		command := "delete host-interface name " + netdev
		commands = append(commands, command)
		_, _ = executor.vppCommand(ctx, "delete", "host-interface", "name", netdev)
	}
	after := executor.server.observeNativeAttachment(ctx, netdev, vppName)
	transition.Failures = appendTransitionCommand(transition.Failures, after.Notes)
	if after.present() {
		// The CLI ignores af_packet_delete_if's return value, so a "successful"
		// delete can leave the interface in place. Name the commands that were
		// tried: this error is the operator's only clue.
		return commands, fmt.Errorf("native attachment for %s still exists after teardown (%s) despite `%s`", netdev, after.summary(), strings.Join(commands, "` then `"))
	}
	if err := executor.detachStaleXDPProgram(ctx, netdev, after); err != nil {
		return commands, err
	}
	return commands, nil
}

// buildNativeAttachment creates the requested attachment exactly once and
// proves the plugin and the netdev with a readback before reporting success.
func (executor systemTakeoverExecutor) buildNativeAttachment(ctx context.Context, transition *takeoverTransition, mode string) ([]string, error) {
	netdev := strings.TrimSpace(transition.Netdev)
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+netdev)
	if netdev == "" {
		return nil, fmt.Errorf("the Linux netdev for %s is unknown; cannot create a %s attachment", transition.Interface, takeoverModeLabel(mode))
	}
	commands := []string{}
	ip := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
	_, _ = exec.CommandContext(ctx, ip, "link", "set", "dev", netdev, "promisc", "on", "up").CombinedOutput()
	switch strings.TrimSpace(mode) {
	case takeoverModeAFXDPZeroCopy, takeoverModeAFXDPCopy:
		create := func() (string, error) {
			return executor.vppCommand(ctx, "create", "interface", "af_xdp",
				"host-if", netdev,
				"name", vppName,
				"rx-queue-size", "512",
				"tx-queue-size", "512",
				"num-rx-queues", afxdpRXQueues(mode),
				afxdpModeFlag(mode),
				"mac-reuse",
			)
		}
		commands = append(commands, fmt.Sprintf("create interface af_xdp host-if %s name %s rx-queue-size 512 tx-queue-size 512 num-rx-queues %s %s mac-reuse",
			netdev, vppName, afxdpRXQueues(mode), afxdpModeFlag(mode)))
		output, err := create()
		if err != nil && strings.Contains(strings.ToLower(output), "busy") {
			// EBUSY after a verified teardown means a queue or the dispatcher is
			// still held by a dead owner. Retry once after re-running the stale
			// XDP cleanup, then fail so the caller rolls back.
			current := executor.server.observeNativeAttachment(ctx, netdev, vppName)
			if cleanupErr := executor.detachStaleXDPProgram(ctx, netdev, current); cleanupErr == nil {
				commands = append(commands, "# retry after stale XDP cleanup")
				output, err = create()
			}
		}
		if err != nil {
			return commands, fmt.Errorf("create AF_XDP interface for %s: %w: %s", netdev, err, firstLine(output, 300))
		}
	case takeoverModeAFPacket:
		// cksum-gso-disable makes VPP ignore the virtio-net header, so the
		// host netdev must not deliver GRO-coalesced super-frames that VPP
		// cannot size down to the physical MTU.
		if err := disableHostGRO(netdev); err != nil {
			return commands, fmt.Errorf("disable host GRO for af_packet on %s: %w", netdev, err)
		}
		commands = append(commands, "create host-interface name "+netdev+" cksum-gso-disable")
		if output, err := executor.vppCommand(ctx, "create", "host-interface", "name", netdev, "cksum-gso-disable"); err != nil {
			return commands, fmt.Errorf("create af_packet host interface for %s: %w: %s", netdev, err, firstLine(output, 300))
		}
		commands = append(commands, "set interface name host-"+netdev+" "+vppName)
		if output, err := executor.vppCommand(ctx, "set", "interface", "name", "host-"+netdev, vppName); err != nil {
			return commands, fmt.Errorf("rename host-%s to %s: %w: %s", netdev, vppName, err, firstLine(output, 300))
		}
		if mac := netdevMAC(netdev); mac != "" {
			commands = append(commands, "set interface mac address "+vppName+" "+mac)
			if _, err := executor.vppCommand(ctx, "set", "interface", "mac", "address", vppName, mac); err != nil {
				return commands, fmt.Errorf("restore MAC address on %s: %w", vppName, err)
			}
		}
	case takeoverModeRDMA:
		commands = append(commands, "create interface rdma host-if "+netdev+" name "+vppName+" mode dv")
		if output, err := executor.vppCommand(ctx, "create", "interface", "rdma", "host-if", netdev, "name", vppName, "mode", "dv"); err != nil {
			return commands, fmt.Errorf("create RDMA interface for %s: %w: %s", netdev, err, firstLine(output, 300))
		}
	default:
		return commands, fmt.Errorf("unsupported native takeover mode %q", takeoverModeLabel(mode))
	}
	// Read the new attachment back before anything depends on it. A create that
	// printed no error is not proof.
	live := executor.server.observeNativeAttachment(ctx, netdev, vppName)
	if live.ambiguous() {
		return commands, fmt.Errorf("%s create for %s produced %d interfaces named %s", takeoverModeLabel(mode), netdev, len(live.Rows), vppName)
	}
	if !live.matches(mode) {
		return commands, fmt.Errorf("readback after creating %s for %s reports %s (wanted %s)", takeoverModeLabel(mode), netdev, live.summary(), takeoverModeLabel(mode))
	}
	if live.Hook == nativeHookAFXDP && live.NetdevSeen != "" && !strings.EqualFold(live.NetdevSeen, netdev) {
		return commands, fmt.Errorf("AF_XDP readback for %s names netdev %q", vppName, live.NetdevSeen)
	}
	commands = append(commands, "set interface state "+vppName+" up")
	if output, err := executor.vppCommand(ctx, "set", "interface", "state", vppName, "up"); err != nil {
		return commands, fmt.Errorf("set %s up: %w: %s", vppName, err, firstLine(output, 300))
	}
	return commands, nil
}

// recreateLCPPair restores the Linux control-plane pair that shared the port.
// It must run after the new phy interface exists: `lcp create` needs the
// sw_if_index, and the pair (plus its lylan-<netdev> tap) is what carries the
// LAN address and DHCP to the host stack.
func (executor systemTakeoverExecutor) recreateLCPPair(ctx context.Context, transition *takeoverTransition, hostInterface string) error {
	hostInterface = strings.TrimSpace(hostInterface)
	if hostInterface == "" {
		return nil
	}
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+transition.Netdev)
	if output, err := executor.vppCommand(ctx, "lcp", "create", vppName, "host-if", hostInterface); err != nil {
		return fmt.Errorf("recreate LCP pair %s host-if %s: %w: %s", vppName, hostInterface, err, firstLine(output, 200))
	}
	lcp, err := executor.vppCommand(ctx, "show", "lcp")
	if err != nil {
		return fmt.Errorf("read back LCP pairs: %w", err)
	}
	if parseLCPHostInterface(lcp, vppName) == "" {
		return fmt.Errorf("LCP pair %s host-if %s is missing after lcp create", vppName, hostInterface)
	}
	return nil
}

// convergeNativeAttachment is the complete, idempotent AF_PACKET <-> AF_XDP
// switch to transition.ToMode. It is safe to run repeatedly: a port that
// already carries the requested attachment is only brought up and re-paired.
func (executor systemTakeoverExecutor) convergeNativeAttachment(ctx context.Context, transition *takeoverTransition) error {
	return executor.convergeNativeAttachmentTo(ctx, transition, transition.ToMode)
}

// convergeNativeAttachmentTo replaces whatever the netdev carries with one
// verified attachment of the requested mode.
func (executor systemTakeoverExecutor) convergeNativeAttachmentTo(ctx context.Context, transition *takeoverTransition, mode string) error {
	mode = strings.TrimSpace(mode)
	netdev := strings.TrimSpace(transition.Netdev)
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+netdev)
	live := executor.server.observeNativeAttachment(ctx, netdev, vppName)
	if live.Reachable && live.matches(mode) {
		if live.State != "up" {
			if output, err := executor.vppCommand(ctx, "set", "interface", "state", vppName, "up"); err != nil {
				return fmt.Errorf("bring converged %s up: %w: %s", vppName, err, firstLine(output, 200))
			}
		}
		if transition.LCPHostInterface != "" && live.LCPHost == "" {
			if err := executor.recreateLCPPair(ctx, transition, transition.LCPHostInterface); err != nil {
				return err
			}
		}
		log.Printf("dataplane switch: %s already carries %s (%s); no teardown needed", netdev, takeoverModeLabel(mode), live.summary())
		return nil
	}
	// Capture the interface-level state before it is destroyed with the interface.
	if live.present() {
		if transition.InterfaceAddresses == nil && transition.MTU == 0 {
			config := executor.server.captureInterfaceL3(ctx, vppName)
			transition.InterfaceAddresses = config.Addresses
			transition.MTU = config.MTU
		}
	}
	if live.LCPHost != "" {
		transition.LCPHostInterface = live.LCPHost
	}
	if transition.Netdev != "" && !transition.LinkWasUp {
		transition.LinkWasUp = netdevLinkState(transition.Netdev) == "up"
	}
	commands, err := executor.teardownNativeAttachment(ctx, transition, live)
	transition.Commands = appendTransitionCommand(transition.Commands, commands)
	if err != nil {
		return err
	}
	commands, err = executor.buildNativeAttachment(ctx, transition, mode)
	transition.Commands = appendTransitionCommand(transition.Commands, commands)
	if err != nil {
		return err
	}
	// The LCP pair must exist before the addresses are replayed: linux-cp syncs
	// addresses to the tap on address events, not when the pair is created, so
	// an address added before the pair never reaches lylan-<netdev>.
	if err := executor.recreateLCPPair(ctx, transition, transition.LCPHostInterface); err != nil {
		return err
	}
	if failures := executor.restoreInterfaceL3(ctx, vppName, interfaceL3Config{Addresses: transition.InterfaceAddresses, MTU: transition.MTU}); len(failures) > 0 {
		log.Printf("dataplane switch: interface L3 restore for %s: %s", vppName, strings.Join(failures, "; "))
		transition.Failures = append(transition.Failures, failures...)
	}
	return nil
}

// appendTransitionCommand records the exact privileged commands a step ran, so
// a failed switch can be replayed by hand from the journal.
func appendTransitionCommand(existing []string, commands []string) []string {
	for _, command := range commands {
		existing = append(existing, "cmd: "+command)
	}
	return existing
}

// removeNativeAttachmentLive deletes the current attachment without building a
// replacement (the rollback of a first bind that never had a previous owner).
func (executor systemTakeoverExecutor) removeNativeAttachmentLive(ctx context.Context, transition *takeoverTransition) error {
	netdev := strings.TrimSpace(transition.Netdev)
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+netdev)
	live := executor.server.observeNativeAttachment(ctx, netdev, vppName)
	if !live.present() {
		return nil
	}
	commands, err := executor.teardownNativeAttachment(ctx, transition, live)
	transition.Commands = appendTransitionCommand(transition.Commands, commands)
	return err
}

// persistLiveVerification writes the verification record from a live readback.
// It refuses to write a mode the netdev does not actually carry, so the file can
// never claim an attachment that does not exist.
func (server *Server) persistLiveVerification(ctx context.Context, snapshot takeoverSnapshot, mode, method string) bool {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return false
	}
	netdev := firstNonEmptyTrimmed(snapshot.Netdev, snapshot.LinuxInterface)
	vppName := firstNonEmptyTrimmed(snapshot.VPPInterface, "lyroute-"+netdev)
	live := server.observeNativeAttachment(ctx, netdev, vppName)
	if !live.matches(mode) {
		log.Printf("dataplane switch: refusing to persist %s verification for %s: live attachment is %s", takeoverModeLabel(mode), netdev, live.summary())
		return false
	}
	if live.DeviceError != "" {
		log.Printf("dataplane switch: refusing to persist %s verification for %s: VPP reports a device error (%s)", takeoverModeLabel(mode), netdev, live.DeviceError)
		return false
	}
	link := netdevLinkState(netdev)
	if takeoverModeDestructive(mode) {
		// A DPDK port has no Linux netdev, so the kernel view is always
		// "down". The PHY carrier of the VPP hardware interface is the real
		// link state; without it a working DPDK port is recorded as
		// pending_link forever.
		if attached := vppInterfaceForPCI(ctx, snapshot.PCIAddress); attached != "" {
			if carrier := vppCarrierState(ctx, attached); carrier != "" {
				link = carrier
			}
		}
	}
	verification := "verified"
	if link != "up" {
		verification = "pending_link"
	}
	entry := dataplane.DPDKVerification{
		LinuxInterface:     snapshot.LinuxInterface,
		PCIAddress:         snapshot.PCIAddress,
		Mode:               mode,
		FunctionalVerified: verification == "verified",
		Verification:       verification,
		Method:             method,
		LinkState:          link,
		HardwareLink:       link,
		ObservedAt:         time.Now().UTC(),
	}
	if err := mergeDPDKVerification(dataplaneVerificationPath(), []dataplane.DPDKVerification{entry}); err != nil {
		log.Printf("persist %s verification for %s: %v", takeoverModeLabel(mode), snapshot.LinuxInterface, err)
		return false
	}
	return true
}

// repairRecordPath is the operator-facing record of a switch that could not be
// rolled back. It is the only place that claims a port needs manual work.
func repairRecordPath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_NATIVE_REPAIR_STATE")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "native-repair.json")
}

type nativeRepairRecord struct {
	SchemaVersion int       `json:"schema_version"`
	Interface     string    `json:"linux_interface"`
	VPPInterface  string    `json:"vpp_interface,omitempty"`
	TargetMode    string    `json:"target_mode"`
	Reason        string    `json:"reason"`
	Live          string    `json:"live,omitempty"`
	Commands      []string  `json:"commands,omitempty"`
	RecordedAt    time.Time `json:"recorded_at"`
}

func recordNativeRepair(transition takeoverTransition, reason, live string) {
	record := nativeRepairRecord{
		SchemaVersion: 1,
		Interface:     transition.Interface,
		VPPInterface:  transition.VPPInterface,
		TargetMode:    transition.ToMode,
		Reason:        reason,
		Live:          live,
		Commands:      append([]string(nil), transition.Failures...),
		RecordedAt:    time.Now().UTC(),
	}
	path := repairRecordPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("record native repair: %v", err)
		return
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		log.Printf("record native repair: %v", err)
		return
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		log.Printf("record native repair: %v", err)
		return
	}
	log.Printf("dataplane switch: %s needs repair: %s (recorded in %s)", transition.Interface, reason, path)
}

// clearNativeRepair removes a stale repair marker once a port converges again.
func clearNativeRepair(name string) {
	path := repairRecordPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var record nativeRepairRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return
	}
	if strings.TrimSpace(record.Interface) != strings.TrimSpace(name) {
		return
	}
	_ = os.Remove(path)
}
