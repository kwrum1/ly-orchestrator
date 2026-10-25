package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

const managedDPDKStart = "# BEGIN LY ROUTE DPDK"
const managedDPDKEnd = "# END LY ROUTE DPDK"

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
}

type LinuxHost struct {
	SysfsRoot     string
	StartupConfig string
	StateDir      string
	Runner        CommandRunner
	VPPCTL        string
	// SysfsWriter is injectable for tests. Production leaves it nil so writes
	// go straight to the kernel's sysfs attributes.
	SysfsWriter func(path string, value string) error
}

func NewLinuxHost() *LinuxHost {
	return &LinuxHost{SysfsRoot: "/sys", StartupConfig: "/etc/vpp/startup.conf", StateDir: "/var/lib/ly-route/dataplane", Runner: ExecRunner{}, VPPCTL: "vppctl"}
}

func (host *LinuxHost) Snapshot(_ context.Context, request Request) (Snapshot, error) {
	if host == nil {
		return Snapshot{}, errors.New("linux host is nil")
	}
	config, err := os.ReadFile(host.startupConfig())
	if err != nil {
		return Snapshot{}, fmt.Errorf("read VPP startup config: %w", err)
	}
	snapshot := Snapshot{TransactionID: request.TransactionID, StartupConfig: config}
	seenPCI := map[string]bool{}
	for _, attachment := range request.Path.Attachments {
		if !isDPDKAttachment(attachment) {
			continue
		}
		state, stateErr := host.deviceState(attachment)
		if stateErr != nil {
			return Snapshot{}, stateErr
		}
		if seenPCI[state.PCIAddress] {
			return Snapshot{}, fmt.Errorf("duplicate PCI device %q", state.PCIAddress)
		}
		seenPCI[state.PCIAddress] = true
		snapshot.Devices = append(snapshot.Devices, state)
	}
	if err := host.persistSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (host *LinuxHost) StopVPP(ctx context.Context) error {
	_, err := host.run(ctx, "systemctl", "stop", "vpp.service")
	return err
}

func (host *LinuxHost) ConfigureDPDK(ctx context.Context, path vpp.NativePath, snapshot Snapshot) error {
	return host.configurePCI(ctx, path, snapshot)
}

func (host *LinuxHost) configurePCI(ctx context.Context, path vpp.NativePath, snapshot Snapshot) error {
	loaded := map[string]bool{}
	for _, attachment := range path.Attachments {
		if !isDPDKAttachment(attachment) {
			continue
		}
		targetDriver, err := dpdkDriverForMode(attachment.Mode)
		if err != nil {
			return err
		}
		if loaded[targetDriver] {
			continue
		}
		if _, err := host.run(ctx, "modprobe", targetDriver); err != nil {
			return fmt.Errorf("load %s: %w", targetDriver, err)
		}
		loaded[targetDriver] = true
	}
	states := map[string]DeviceState{}
	for _, state := range snapshot.Devices {
		states[state.LinuxInterface] = state
	}
	for _, attachment := range path.Attachments {
		if !isDPDKAttachment(attachment) {
			continue
		}
		targetDriver, err := dpdkDriverForMode(attachment.Mode)
		if err != nil {
			return err
		}
		state, found := states[attachment.LinuxInterface]
		if !found || state.PCIAddress != attachment.PCIAddress {
			return fmt.Errorf("snapshot identity mismatch for %q", attachment.LinuxInterface)
		}
		if _, err := os.Stat(host.sysfs(filepath.Join("class/net", state.LinuxInterface))); err == nil {
			if _, err := host.run(ctx, "ip", "link", "set", "dev", state.LinuxInterface, "down"); err != nil {
				return err
			}
		}
		currentDriver := state.BoundDriver
		if currentDriver == "" {
			currentDriver = host.currentDriver(state.PCIAddress)
		}
		created := state.OwnedByProduct
		if currentDriver != targetDriver {
			// Record the intent before mutating the device. If the bind fails or
			// the process dies mid-way, rollback still knows which kernel driver
			// the product must restore.
			if err := host.recordOwnership(state, targetDriver, true); err != nil {
				return fmt.Errorf("record ownership for %s: %w", state.PCIAddress, err)
			}
			if currentDriver != "" {
				if err := host.writeSysfs(filepath.Join("bus/pci/drivers", currentDriver, "unbind"), state.PCIAddress); err != nil {
					return err
				}
			}
			if err := host.writeSysfs(filepath.Join("bus/pci/devices", state.PCIAddress, "driver_override"), targetDriver); err != nil {
				return err
			}
			if err := host.writeSysfs(filepath.Join("bus/pci/drivers", targetDriver, "bind"), state.PCIAddress); err != nil {
				return err
			}
			created = true
		} else if err := host.writeSysfs(filepath.Join("bus/pci/devices", state.PCIAddress, "driver_override"), targetDriver); err != nil {
			return err
		}
		if bound := host.currentDriver(state.PCIAddress); bound != targetDriver {
			return fmt.Errorf("PCI device %s did not bind to %s", state.PCIAddress, targetDriver)
		}
		state.BoundDriver = targetDriver
		state.OwnedByProduct = created
		if err := host.recordOwnership(state, targetDriver, created); err != nil {
			return fmt.Errorf("record ownership for %s: %w", state.PCIAddress, err)
		}
	}
	config, err := RenderDPDKStartup(snapshot.StartupConfig, path.Attachments)
	if err != nil {
		return err
	}
	return atomicWrite(host.startupConfig(), config, 0o640)
}

func (host *LinuxHost) StartVPP(ctx context.Context) error {
	_, err := host.run(ctx, "systemctl", "start", "vpp.service")
	return err
}

func (host *LinuxHost) RebindDHCPService(ctx context.Context) error {
	if _, err := host.run(ctx, "systemctl", "is-active", "--quiet", "dnsmasq.service"); err != nil {
		return nil
	}
	_, err := host.run(ctx, "systemctl", "restart", "dnsmasq.service")
	return err
}

func (host *LinuxHost) VerifyDPDK(ctx context.Context, path vpp.NativePath) error {
	if _, err := host.run(ctx, "systemctl", "is-active", "--quiet", "vpp.service"); err != nil {
		return err
	}
	for _, attachment := range path.Attachments {
		if !isDPDKAttachment(attachment) {
			continue
		}
		expectedDriver, err := dpdkDriverForMode(attachment.Mode)
		if err != nil {
			return err
		}
		driver, err := filepath.EvalSymlinks(host.sysfs(filepath.Join("bus/pci/devices", attachment.PCIAddress, "driver")))
		if err != nil || filepath.Base(driver) != expectedDriver {
			return fmt.Errorf("PCI device %s is not bound to %s", attachment.PCIAddress, expectedDriver)
		}
		if err := host.waitForDPDKReadback(ctx, attachment); err != nil {
			return err
		}
	}
	return nil
}

func (host *LinuxHost) waitForDPDKReadback(ctx context.Context, attachment vpp.NativeAttachment) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		hardware, hardwareErr := host.run(ctx, host.vppctl(), "show", "hardware-interfaces", attachment.VPPInterface)
		if hardwareErr == nil && strings.Contains(string(hardware), attachment.VPPInterface) && hardwareReadbackContainsPCI(string(hardware), attachment.PCIAddress) {
			interfaces, interfaceErr := host.run(ctx, host.vppctl(), "show", "interface", attachment.VPPInterface)
			if interfaceErr == nil && strings.Contains(string(interfaces), attachment.VPPInterface) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("VPP hardware readback missing interface %s PCI %s", attachment.VPPInterface, attachment.PCIAddress)
		case <-ticker.C:
		}
	}
}

func (host *LinuxHost) VerifySmartQoS(ctx context.Context, path vpp.NativePath) error {
	if path.Tier == vpp.DataplaneTierDPDK {
		if err := host.VerifyDPDK(ctx, path); err != nil {
			return err
		}
	}
	output, err := host.run(ctx, host.vppctl(), "show", "ly-route", "smart-qos")
	if err != nil {
		return fmt.Errorf("VPP smart-QoS readback failed: %w", err)
	}
	readback := string(output)
	if !strings.Contains(readback, "state running") || !strings.Contains(readback, "algorithm fq-codel") || !strings.Contains(readback, "qualification production") {
		return errors.New("VPP smart-QoS runtime is not production-qualified and running with fq-codel")
	}
	for _, attachment := range path.Attachments {
		if !strings.Contains(readback, "interface "+attachment.VPPInterface+" enabled") {
			return fmt.Errorf("VPP smart-QoS readback missing interface %s", attachment.VPPInterface)
		}
	}
	return nil
}

func (host *LinuxHost) Restore(ctx context.Context, snapshot Snapshot) error {
	var restoreErrors []error
	if err := host.StopVPP(ctx); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	if err := atomicWrite(host.startupConfig(), snapshot.StartupConfig, 0o640); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	restored := map[string]bool{}
	for _, state := range snapshot.Devices {
		record, recorded := host.ownershipRecord(state.PCIAddress)
		if !recorded || !record.CreatedByProduct {
			// The operator (or a previous system image) owned this binding.
			// Rollback must not confiscate a device the product did not take.
			continue
		}
		current := host.currentDriver(state.PCIAddress)
		if current != "" && current != state.KernelDriver {
			if err := host.writeSysfs(filepath.Join("bus/pci/drivers", current, "unbind"), state.PCIAddress); err != nil {
				restoreErrors = append(restoreErrors, err)
				continue
			}
		}
		if err := host.writeSysfs(filepath.Join("bus/pci/devices", state.PCIAddress, "driver_override"), state.KernelDriver); err != nil {
			restoreErrors = append(restoreErrors, err)
			continue
		}
		if host.currentDriver(state.PCIAddress) != state.KernelDriver {
			if err := host.writeSysfs(filepath.Join("bus/pci/drivers", state.KernelDriver, "bind"), state.PCIAddress); err != nil {
				restoreErrors = append(restoreErrors, err)
				continue
			}
		}
		if host.currentDriver(state.PCIAddress) != state.KernelDriver {
			restoreErrors = append(restoreErrors, fmt.Errorf("PCI device %s did not return to %s", state.PCIAddress, state.KernelDriver))
			continue
		}
		if err := host.releaseOwnership(state.PCIAddress); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
		restored[state.PCIAddress] = true
	}
	for _, state := range snapshot.Devices {
		if !restored[state.PCIAddress] || !state.LinkUp {
			continue
		}
		if err := host.waitForLinuxInterface(ctx, state); err != nil {
			restoreErrors = append(restoreErrors, err)
			continue
		}
		if _, err := host.run(ctx, "ip", "link", "set", "dev", state.LinuxInterface, "up"); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if err := host.StartVPP(ctx); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	if len(restoreErrors) == 0 {
		_ = os.Remove(host.snapshotPath(snapshot.TransactionID))
	}
	return errors.Join(restoreErrors...)
}

func hardwareReadbackContainsPCI(output, pci string) bool {
	output = strings.ToLower(output)
	pci = strings.ToLower(strings.TrimSpace(pci))
	if strings.Contains(output, pci) {
		return true
	}
	dot := strings.LastIndexByte(pci, '.')
	if dot < 0 || len(pci)-dot != 2 {
		return false
	}
	// DPDK prints the PCI function as two hexadecimal digits (for example
	// 0000:0b:00.00), while Linux sysfs uses one digit (0000:0b:00.0).
	return strings.Contains(output, pci[:dot+1]+"0"+pci[dot+1:])
}

func (host *LinuxHost) waitForLinuxInterface(ctx context.Context, state DeviceState) error {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		device, err := filepath.EvalSymlinks(host.sysfs(filepath.Join("class/net", state.LinuxInterface, "device")))
		if err == nil && filepath.Base(device) == state.PCIAddress {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("interface %s did not reappear for PCI %s", state.LinuxInterface, state.PCIAddress)
		case <-ticker.C:
		}
	}
}

func (host *LinuxHost) LoadActiveState(_ context.Context) (ActiveState, bool, error) {
	data, err := os.ReadFile(host.activeStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return ActiveState{}, false, nil
	}
	if err != nil {
		return ActiveState{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state ActiveState
	if err := decoder.Decode(&state); err != nil {
		return ActiveState{}, false, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ActiveState{}, false, errors.New("active dataplane state has trailing data")
	}
	if state.Path.Tier != vpp.DataplaneTierDPDK && state.Path.Tier != vpp.DataplaneTierNative || len(state.Path.Attachments) == 0 || state.AppliedAt.IsZero() {
		return ActiveState{}, false, errors.New("active dataplane state is incomplete")
	}
	return state, true, nil
}

func (host *LinuxHost) SaveActiveState(_ context.Context, state ActiveState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(host.activeStatePath(), append(data, '\n'), 0o600)
}

func (host *LinuxHost) ClearActiveState(_ context.Context) error {
	err := os.Remove(host.activeStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (host *LinuxHost) deviceState(attachment vpp.NativeAttachment) (DeviceState, error) {
	name := strings.TrimSpace(attachment.LinuxInterface)
	if !safeToken(name) || !safePCI(attachment.PCIAddress) || !safeToken(attachment.KernelDriver) {
		return DeviceState{}, fmt.Errorf("unsafe DPDK attachment identity")
	}
	targetDriver, err := dpdkDriverForMode(attachment.Mode)
	if err != nil {
		return DeviceState{}, err
	}
	devicePath := host.sysfs(filepath.Join("class/net", name, "device"))
	linkUp := false
	if _, statErr := os.Stat(devicePath); statErr == nil {
		device, err := filepath.EvalSymlinks(devicePath)
		if err != nil || filepath.Base(device) != attachment.PCIAddress {
			return DeviceState{}, fmt.Errorf("interface %s does not resolve to PCI %s", name, attachment.PCIAddress)
		}
		operstate, _ := os.ReadFile(host.sysfs(filepath.Join("class/net", name, "operstate")))
		linkUp = strings.TrimSpace(string(operstate)) == "up"
		devicePath = host.sysfs(filepath.Join("bus/pci/devices", attachment.PCIAddress))
	} else {
		// prepare-vfio.sh may have taken the NIC before the dataplane
		// transaction started. The Linux netdev is intentionally gone, but the
		// PCI device and the ownership record remain.
		devicePath = host.sysfs(filepath.Join("bus/pci/devices", attachment.PCIAddress))
		if _, err := os.Stat(devicePath); err != nil {
			return DeviceState{}, fmt.Errorf("interface %s and PCI device %s are both absent", name, attachment.PCIAddress)
		}
		linkUp = true
	}
	currentDriver := host.currentDriver(attachment.PCIAddress)
	switch {
	case currentDriver == attachment.KernelDriver:
		// The NIC is still kernel-owned; ConfigureDPDK will bind it.
	case currentDriver == targetDriver:
		// Already owned by the selected DPDK driver (boot preflight or replay).
	default:
		return DeviceState{}, fmt.Errorf("interface %s driver is %q, expected kernel driver %q or DPDK driver %q", name, currentDriver, attachment.KernelDriver, targetDriver)
	}
	if attachment.Mode == vpp.NativeModeDPDKUIO {
		if attachment.IOMMUGroup != "none" {
			return DeviceState{}, fmt.Errorf("interface %s has invalid UIO isolation identity", name)
		}
	} else {
		iommu, err := filepath.EvalSymlinks(filepath.Join(devicePath, "iommu_group"))
		if err != nil || filepath.Base(iommu) != attachment.IOMMUGroup {
			return DeviceState{}, fmt.Errorf("interface %s IOMMU group changed since capability proof", name)
		}
	}
	ownedByProduct := false
	if record, ok := host.ownershipRecord(attachment.PCIAddress); ok && record.CreatedByProduct {
		ownedByProduct = true
	}
	return DeviceState{
		LinuxInterface: name,
		PCIAddress:     attachment.PCIAddress,
		KernelDriver:   attachment.KernelDriver,
		BoundDriver:    currentDriver,
		OwnedByProduct: ownedByProduct,
		LinkUp:         linkUp,
	}, nil
}

func (host *LinuxHost) currentDriver(pci string) string {
	driver, err := filepath.EvalSymlinks(host.sysfs(filepath.Join("bus/pci/devices", pci, "driver")))
	if err != nil {
		return ""
	}
	name := filepath.Base(driver)
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return name
}

func RenderDPDKStartup(current []byte, attachments []vpp.NativeAttachment) ([]byte, error) {
	text := string(current)
	if start := strings.Index(text, managedDPDKStart); start >= 0 {
		end := strings.Index(text[start:], managedDPDKEnd)
		if end < 0 {
			return nil, errors.New("unterminated managed DPDK startup block")
		}
		text = text[:start] + text[start+end+len(managedDPDKEnd):]
	}
	dpdkAttachments := make([]vpp.NativeAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if isDPDKAttachment(attachment) {
			dpdkAttachments = append(dpdkAttachments, attachment)
		}
	}
	driver, err := dpdkBindingDriver(dpdkAttachments)
	if err != nil {
		return nil, err
	}
	var block strings.Builder
	block.WriteString("\n" + managedDPDKStart + "\ndpdk {\n  uio-driver " + driver + "\n")
	seen := map[string]bool{}
	for _, attachment := range dpdkAttachments {
		if !safePCI(attachment.PCIAddress) || seen[attachment.PCIAddress] {
			return nil, fmt.Errorf("invalid or duplicate DPDK startup attachment")
		}
		seen[attachment.PCIAddress] = true
		// Do not assign an explicit VPP name: the PCI slot name VPP derives
		// (for example GigabitEthernet4/0/0) is stable across restarts and is
		// the name the capability probe and address assignments already use.
		// A configured name also made VPP reject an otherwise valid device
		// start on the J1900 i211/UIO path.
		fmt.Fprintf(&block, "  dev %s\n", attachment.PCIAddress)
	}
	if len(seen) == 0 {
		return nil, errors.New("DPDK startup requires at least one device")
	}
	block.WriteString("}\n" + managedDPDKEnd + "\n")
	text = strings.TrimRight(text, "\n") + "\n" + block.String()
	return []byte(text), nil
}

func dpdkBindingDriver(attachments []vpp.NativeAttachment) (string, error) {
	driver := ""
	for _, attachment := range attachments {
		if !isDPDKAttachment(attachment) {
			continue
		}
		candidate := ""
		switch attachment.Mode {
		case vpp.NativeModeDPDKVFIO:
			candidate = "vfio-pci"
		case vpp.NativeModeDPDKUIO:
			candidate = "uio_pci_generic"
		default:
			return "", fmt.Errorf("unsupported DPDK binding mode %q", attachment.Mode)
		}
		if driver != "" && driver != candidate {
			// VPP exposes one global uio-driver. "auto" lets DPDK use the driver
			// each device is already bound to, so a device-wide path can mix
			// IOMMU-protected VFIO ports with no-IOMMU UIO ports.
			return "auto", nil
		}
		driver = candidate
	}
	if driver == "" {
		return "", errors.New("DPDK startup requires at least one device")
	}
	return driver, nil
}

func (host *LinuxHost) persistSnapshot(snapshot Snapshot) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(host.snapshotPath(snapshot.TransactionID), append(data, '\n'), 0o600)
}
func (host *LinuxHost) snapshotPath(id string) string {
	return filepath.Join(host.stateDir(), id+".json")
}
func (host *LinuxHost) activeStatePath() string {
	return filepath.Join(host.stateDir(), "active.json")
}
func (host *LinuxHost) startupConfig() string {
	if host.StartupConfig != "" {
		return host.StartupConfig
	}
	return "/etc/vpp/startup.conf"
}
func (host *LinuxHost) stateDir() string {
	if host.StateDir != "" {
		return host.StateDir
	}
	return "/var/lib/ly-route/dataplane"
}
func (host *LinuxHost) sysfs(path string) string {
	root := host.SysfsRoot
	if root == "" {
		root = "/sys"
	}
	return filepath.Join(root, path)
}
func (host *LinuxHost) vppctl() string {
	if host.VPPCTL != "" {
		return host.VPPCTL
	}
	return "vppctl"
}
func (host *LinuxHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if host.Runner == nil {
		host.Runner = ExecRunner{}
	}
	output, err := host.Runner.Run(ctx, name, args...)
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
func (host *LinuxHost) writeSysfs(path, value string) error {
	path = host.sysfs(path)
	if host.SysfsWriter != nil {
		return host.SysfsWriter(path, value)
	}
	return os.WriteFile(path, []byte(value), 0o200)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ly-route-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(mode); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func safeToken(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}
func safePCI(value string) bool {
	if len(value) != 12 || value[4] != ':' || value[7] != ':' || value[10] != '.' {
		return false
	}
	for index, char := range value {
		if index == 4 || index == 7 || index == 10 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func isDPDKAttachment(attachment vpp.NativeAttachment) bool {
	return attachment.Tier == vpp.DataplaneTierDPDK || attachment.Hook == vpp.NativeHookDPDK ||
		attachment.Mode == vpp.NativeModeDPDKVFIO || attachment.Mode == vpp.NativeModeDPDKUIO
}
