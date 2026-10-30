package dataplane

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

// rebuildReadbackRunner models the delete/create/rename window that follows
// every VPP restart. A DPDK port is invisible for the first samples and then
// answers the read-only readback, either under the product name or (after a
// restart that lost the rename) under the PMD name derived from its PCI slot.
type rebuildReadbackRunner struct {
	mu       sync.Mutex
	commands []string
	samples  int
	// invisibleSamples counts how many hardware readback samples still report
	// the port as missing.
	invisibleSamples int
	// pmdName models a restart that re-created the port under its PMD name
	// after the probe had already renamed it to the product name. When set,
	// the product name only appears once the rename ran.
	pmdName string
	renamed bool
	product string
	pci     string
}

func (runner *rebuildReadbackRunner) visible() bool {
	if runner.pmdName != "" {
		return runner.renamed
	}
	return runner.samples > runner.invisibleSamples
}

func (runner *rebuildReadbackRunner) hardwareBlock(name string) string {
	if name == "" {
		return ""
	}
	return name + "\n  Ethernet address b8:27:eb:11:22:33\n  Intel i211 (Copper)\n    pci: device " + runner.pci + " address " + runner.paddedPCI() + " numa 0\n"
}

func (runner *rebuildReadbackRunner) paddedPCI() string {
	dot := strings.LastIndexByte(runner.pci, '.')
	if dot < 0 || len(runner.pci)-dot != 2 {
		return runner.pci
	}
	return runner.pci[:dot+1] + "0" + runner.pci[dot+1:]
}

func (runner *rebuildReadbackRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	switch command {
	case "systemctl is-active --quiet vpp.service":
		return nil, nil
	case "vppctl show hardware-interfaces":
		runner.mu.Lock()
		runner.samples++
		listing := ""
		if runner.pmdName != "" && !runner.renamed {
			listing = runner.hardwareBlock(runner.pmdName)
		}
		if runner.visible() {
			listing = runner.hardwareBlock(runner.product)
		}
		runner.mu.Unlock()
		return []byte(listing), nil
	case "vppctl show hardware-interfaces " + runner.product:
		runner.mu.Lock()
		runner.samples++
		visible := runner.visible()
		runner.mu.Unlock()
		if !visible {
			return nil, nil
		}
		return []byte(runner.hardwareBlock(runner.product)), nil
	case "vppctl show hardware-interfaces " + runner.pmdName:
		runner.mu.Lock()
		runner.samples++
		present := runner.pmdName != "" && !runner.renamed
		runner.mu.Unlock()
		if !present {
			return nil, nil
		}
		return []byte(runner.hardwareBlock(runner.pmdName)), nil
	case "vppctl show interface " + runner.product:
		runner.mu.Lock()
		visible := runner.visible()
		runner.mu.Unlock()
		if !visible {
			return nil, nil
		}
		return []byte(runner.product + " 1 up\n"), nil
	case "vppctl show interface " + runner.pmdName:
		runner.mu.Lock()
		present := runner.pmdName != "" && !runner.renamed
		runner.mu.Unlock()
		if !present {
			return nil, nil
		}
		return []byte(runner.pmdName + " 1 up\n"), nil
	case "vppctl set interface name " + runner.pmdName + " " + runner.product:
		runner.mu.Lock()
		runner.renamed = true
		runner.mu.Unlock()
		return nil, nil
	}
	return nil, nil
}

func (runner *rebuildReadbackRunner) issuedCommands() []string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]string(nil), runner.commands...)
}

func (runner *rebuildReadbackRunner) renames() int {
	count := 0
	for _, command := range runner.issuedCommands() {
		if strings.HasPrefix(command, "vppctl set interface name ") {
			count++
		}
	}
	return count
}

func newRebuiltReadbackHost(t *testing.T, runner *rebuildReadbackRunner) *LinuxHost {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{
		filepath.Join(root, "bus/pci/drivers/vfio-pci"),
		filepath.Join(root, "bus/pci/devices", runner.pci),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(
		filepath.Join(root, "bus/pci/drivers/vfio-pci"),
		filepath.Join(root, "bus/pci/devices", runner.pci, "driver"),
	); err != nil {
		t.Fatal(err)
	}
	return &LinuxHost{SysfsRoot: root, Runner: runner}
}

// rebuiltReadbackPath models the port that was just taken from LAN to WAN: the
// probe only knows the product name, so HardwareVPPInterface is empty.
func rebuiltReadbackPath(product, pci string) vpp.NativePath {
	return vpp.NativePath{Tier: vpp.DataplaneTierDPDK, Attachments: []vpp.NativeAttachment{{
		LinuxInterface: "enp2s0",
		VPPInterface:   product,
		Tier:           vpp.DataplaneTierDPDK,
		Hook:           vpp.NativeHookDPDK,
		Mode:           vpp.NativeModeDPDKVFIO,
		PCIAddress:     pci,
	}}}
}

func captureDataplaneLog(t *testing.T) func() string {
	t.Helper()
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string { return buffer.String() }
}

func TestRebuiltDPDKReadbackSettlesInterfaceRebuiltByThisTransaction(t *testing.T) {
	// Given a port that is missing on the first readback sample and appears
	// on a later one
	runner := &rebuildReadbackRunner{product: "lyroute-enp2s0", pci: "0000:02:00.0", invisibleSamples: 2}
	host := newRebuiltReadbackHost(t, runner)
	logs := captureDataplaneLog(t)

	// When
	err := host.VerifyRebuiltDPDK(context.Background(), rebuiltReadbackPath(runner.product, runner.pci))

	// Then the transaction readback commits instead of failing the apply
	if err != nil {
		t.Fatalf("rebuilt readback did not settle: %v", err)
	}
	if logged := logs(); !strings.Contains(logged, "attempt 1/5") || !strings.Contains(logged, "still missing lyroute-enp2s0 (PCI 0000:02:00.0)") {
		t.Fatalf("retry was not logged with its attempt and the remaining interface: %q", logged)
	}
}

func TestRebuiltDPDKReadbackClaimsThePMDNameAfterRestart(t *testing.T) {
	// Given a port the restart re-created under its PMD name while the probe
	// only knows the product name
	runner := &rebuildReadbackRunner{
		product: "lyroute-enp2s0",
		pci:     "0000:02:00.0",
		pmdName: "GigabitEthernet2/0/0",
	}
	host := newRebuiltReadbackHost(t, runner)
	logs := captureDataplaneLog(t)

	// When
	err := host.VerifyRebuiltDPDK(context.Background(), rebuiltReadbackPath(runner.product, runner.pci))

	// Then the port is located by PCI, renamed once and verified
	if err != nil {
		t.Fatalf("rebuilt readback did not recover the PMD-named port: %v", err)
	}
	if runner.renames() != 1 {
		t.Fatalf("renames=%d, want exactly 1 (commands=%v)", runner.renames(), runner.issuedCommands())
	}
	if logged := logs(); !strings.Contains(logged, "renamed rebuilt VPP interface GigabitEthernet2/0/0") {
		t.Fatalf("the rename was not logged: %q", logged)
	}
}

func TestRebuiltDPDKReadbackFailsAfterWindowWithTheSameReason(t *testing.T) {
	// Given a port that never reappears
	runner := &rebuildReadbackRunner{product: "lyroute-enp2s0", pci: "0000:02:00.0", invisibleSamples: 10000}
	host := newRebuiltReadbackHost(t, runner)
	logs := captureDataplaneLog(t)

	// When
	started := time.Now()
	err := host.VerifyRebuiltDPDK(context.Background(), rebuiltReadbackPath(runner.product, runner.pci))
	elapsed := time.Since(started)

	// Then the window is bounded and the hard failure is unchanged
	if err == nil || err.Error() != "VPP hardware readback missing interface lyroute-enp2s0 PCI 0000:02:00.0" {
		t.Fatalf("error=%v, want the unchanged missing-interface failure", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("stabilization window took %s, want <= 2s", elapsed)
	}
	if !strings.Contains(logs(), "attempt 5/5") {
		t.Fatalf("last retry was not logged: %q", logs())
	}
	if runner.renames() != 0 {
		t.Fatalf("renamed a port that was never located: %d", runner.renames())
	}
}

func TestRebuiltDPDKReadbackDoesNotRetryAnInterfaceThatIsAlreadyThere(t *testing.T) {
	// Given a port that survived the transaction
	runner := &rebuildReadbackRunner{product: "lyroute-enp2s0", pci: "0000:02:00.0"}
	host := newRebuiltReadbackHost(t, runner)
	logs := captureDataplaneLog(t)

	// When
	err := host.VerifyRebuiltDPDK(context.Background(), rebuiltReadbackPath(runner.product, runner.pci))

	// Then there is exactly one readback sample and no retry noise
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl is-active --quiet vpp.service",
		"vppctl show hardware-interfaces lyroute-enp2s0",
		"vppctl show interface lyroute-enp2s0",
	}
	if !reflect.DeepEqual(runner.issuedCommands(), want) {
		t.Fatalf("commands=%v, want=%v", runner.issuedCommands(), want)
	}
	if logged := logs(); logged != "" {
		t.Fatalf("unexpected retry noise: %q", logged)
	}
}

func TestStrictDPDKReadbackIsNotLoosenedByTheRebuildWindow(t *testing.T) {
	// Given a missing port verified outside this transaction's rebuild
	runner := &rebuildReadbackRunner{
		product:          "lyroute-enp2s0",
		pci:              "0000:02:00.0",
		pmdName:          "GigabitEthernet2/0/0",
		invisibleSamples: 10000,
	}
	host := newRebuiltReadbackHost(t, runner)
	logs := captureDataplaneLog(t)

	// When
	err := host.VerifyDPDK(context.Background(), rebuiltReadbackPath(runner.product, runner.pci))

	// Then it keeps the historical strict verification: no PCI discovery, no
	// rename and the unchanged failure.
	if err == nil || err.Error() != "VPP hardware readback missing interface lyroute-enp2s0 PCI 0000:02:00.0" {
		t.Fatalf("error=%v, want the unchanged missing-interface failure", err)
	}
	if runner.renames() != 0 {
		t.Fatalf("strict verification renamed the port: %v", runner.issuedCommands())
	}
	for _, command := range runner.issuedCommands() {
		if command == "vppctl show hardware-interfaces" {
			t.Fatalf("strict verification discovered the port by PCI: %v", runner.issuedCommands())
		}
	}
	if logged := logs(); logged != "" {
		t.Fatalf("unexpected retry noise: %q", logged)
	}
}

func TestHardwareReadbackInterfaceForPCIUsesTheBlockHeader(t *testing.T) {
	// Given a VPP hardware listing with two DPDK ports
	listing := strings.Join([]string{
		"              Name                Idx    Link  Hardware",
		"GigabitEthernet2/0/0               1     up   GigabitEthernet2/0/0",
		"  Ethernet address b8:27:eb:11:22:33",
		"  Intel i211 (Copper)",
		"    pci: device 0000:02:00.0 address 0000:02:00.00 numa 0",
		"lyroute-enp3s0                     2     up   GigabitEthernet3/0/0",
		"  Ethernet address b8:27:eb:44:55:66",
		"  Intel i211 (Copper)",
		"    pci: device 0000:03:00.0 address 0000:03:00.00 numa 0",
	}, "\n")

	for pci, want := range map[string]string{
		"0000:02:00.0": "GigabitEthernet2/0/0",
		"0000:03:00.0": "lyroute-enp3s0",
		"0000:09:00.0": "",
	} {
		if got := hardwareReadbackInterfaceForPCI(listing, pci); got != want {
			t.Fatalf("PCI %s resolved to %q, want %q", pci, got, want)
		}
	}
}

// rebuiltVerifyHost is a Host that can tell a rebuilt port apart from a
// missing one and records which verification the transaction used.
type rebuiltVerifyHost struct {
	*fakeHost
	rebuiltErr   error
	rebuiltCalls int
}

func (host *rebuiltVerifyHost) VerifyRebuiltDPDK(_ context.Context, path vpp.NativePath) error {
	host.rebuiltCalls++
	host.verifiedPaths = append(host.verifiedPaths, path)
	if host.rebuiltErr != nil {
		host.trace = append(host.trace, "verify-rebuilt-dpdk")
		return host.rebuiltErr
	}
	return host.call("verify-rebuilt-dpdk")
}

func TestDPDKApplyVerifiesWithTheRebuiltReadback(t *testing.T) {
	// Given a host that sees the port appear just after the restart
	host := &rebuiltVerifyHost{fakeHost: &fakeHost{}}

	// When
	receipt, err := (&Transaction{Host: host, Now: fixedClock}).Apply(context.Background(), dpdkRequest("txn-rebuilt"))

	// Then the apply commits through the bounded readback
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Changed || host.rebuiltCalls != 1 || len(host.verifiedPaths) != 1 {
		t.Fatalf("receipt=%#v rebuiltCalls=%d verified=%v", receipt, host.rebuiltCalls, host.verifiedPaths)
	}
	if last := host.trace[len(host.trace)-1]; last != "verify-rebuilt-dpdk" {
		t.Fatalf("last phase=%q, want verify-rebuilt-dpdk", last)
	}
}

func TestDPDKApplyFailsWhenTheRebuiltInterfaceNeverAppears(t *testing.T) {
	// Given a port that stays missing for the whole window
	host := &rebuiltVerifyHost{
		fakeHost:   &fakeHost{},
		rebuiltErr: errors.New("VPP hardware readback missing interface lyroute-eth1 PCI 0000:03:00.0"),
	}

	// When
	_, err := (&Transaction{Host: host, Now: fixedClock}).Apply(context.Background(), dpdkRequest("txn-rebuilt-missing"))

	// Then the transaction fails at readback with the unchanged reason and
	// rolls back
	if err == nil || !errors.Is(err, ErrTransactionFailed) {
		t.Fatalf("error=%v, want a dataplane transaction failure", err)
	}
	if want := "readback: VPP hardware readback missing interface lyroute-eth1 PCI 0000:03:00.0"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error=%q, want it to contain %q", err, want)
	}
	if last := host.trace[len(host.trace)-1]; last != "restore" {
		t.Fatalf("last phase=%q, want the rollback restore", last)
	}
}
