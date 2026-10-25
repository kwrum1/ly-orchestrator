package dataplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

type fakeSysfs struct {
	root    string
	binds   int
	unbinds int
}

func (fake *fakeSysfs) write(path, value string) error {
	switch {
	case strings.HasSuffix(path, "/bind"):
		fake.binds++
		driverDir := filepath.Dir(path)
		deviceDir := filepath.Join(filepath.Dir(filepath.Dir(driverDir)), "devices", value)
		if err := os.Remove(filepath.Join(deviceDir, "driver")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(deviceDir, 0o755); err != nil {
			return err
		}
		return os.Symlink(driverDir, filepath.Join(deviceDir, "driver"))
	case strings.HasSuffix(path, "/unbind"):
		fake.unbinds++
		driverDir := filepath.Dir(path)
		deviceDir := filepath.Join(filepath.Dir(filepath.Dir(driverDir)), "devices", value)
		if err := os.Remove(filepath.Join(deviceDir, "driver")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	default:
		return os.WriteFile(path, []byte(value), 0o200)
	}
}

func newOwnershipHost(t *testing.T, created bool) (*LinuxHost, *fakeSysfs, string) {
	t.Helper()
	pci := "0000:03:00.0"
	root := t.TempDir()
	for _, directory := range []string{
		filepath.Join(root, "bus/pci/drivers/igb"),
		filepath.Join(root, "bus/pci/drivers/uio_pci_generic"),
		filepath.Join(root, "bus/pci/devices", pci),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "bus/pci/drivers/uio_pci_generic"), filepath.Join(root, "bus/pci/devices", pci, "driver")); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ownership := OwnershipState{Devices: []OwnershipDevice{{
		PCIAddress: pci, LinuxInterface: "eth1", KernelDriver: "igb", Transport: "uio", CreatedByProduct: created,
	}}}
	if err := saveOwnership(filepath.Join(stateDir, "ownership.json"), ownership); err != nil {
		t.Fatal(err)
	}
	startup := filepath.Join(root, "startup.conf")
	if err := os.WriteFile(startup, []byte("unix { nodaemon }\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSysfs{root: root}
	host := &LinuxHost{
		SysfsRoot:     root,
		StartupConfig: startup,
		StateDir:      stateDir,
		Runner:        &recordingRunner{},
		VPPCTL:        "vppctl",
		SysfsWriter:   fake.write,
	}
	return host, fake, pci
}

func TestSnapshotAcceptsNICAlreadyOwnedByUIO(t *testing.T) {
	host, _, pci := newOwnershipHost(t, true)
	snapshot, err := host.Snapshot(context.Background(), Request{
		TransactionID: "txn-preowned",
		Path: vpp.NativePath{Tier: vpp.DataplaneTierDPDK, Attachments: []vpp.NativeAttachment{{
			LinuxInterface: "eth1", VPPInterface: "GigabitEthernet3/0/0", Tier: vpp.DataplaneTierDPDK,
			Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: pci,
			KernelDriver: "igb", IOMMUGroup: "none",
		}}},
	})
	if err != nil {
		t.Fatalf("preflight-owned NIC was not snapshotable: %v", err)
	}
	if len(snapshot.Devices) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	device := snapshot.Devices[0]
	if device.KernelDriver != "igb" || device.BoundDriver != "uio_pci_generic" || !device.OwnedByProduct {
		t.Fatalf("device = %#v, want igb origin, current uio, product-owned", device)
	}
}

func TestConfigureDPDKSkipsAlreadyBoundDevice(t *testing.T) {
	host, fake, pci := newOwnershipHost(t, true)
	path := vpp.NativePath{Tier: vpp.DataplaneTierDPDK, Attachments: []vpp.NativeAttachment{{
		LinuxInterface: "eth1", VPPInterface: "GigabitEthernet3/0/0", Tier: vpp.DataplaneTierDPDK,
		Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: pci,
		KernelDriver: "igb", IOMMUGroup: "none",
	}}}
	snapshot, err := host.Snapshot(context.Background(), Request{TransactionID: "txn-idempotent", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ConfigureDPDK(context.Background(), path, snapshot); err != nil {
		t.Fatalf("idempotent configure failed: %v", err)
	}
	if fake.binds != 0 || fake.unbinds != 0 {
		t.Fatalf("re-bound an already VPP-owned device: binds=%d unbinds=%d", fake.binds, fake.unbinds)
	}
}

func TestRestoreReturnsProductCreatedBindingToKernel(t *testing.T) {
	host, fake, pci := newOwnershipHost(t, true)
	snapshot := Snapshot{TransactionID: "txn-rollback", StartupConfig: []byte("unix { nodaemon }\n"), Devices: []DeviceState{{
		LinuxInterface: "eth1", PCIAddress: pci, KernelDriver: "igb", BoundDriver: "uio_pci_generic",
		OwnedByProduct: true, LinkUp: false,
	}}}
	if err := host.Restore(context.Background(), snapshot); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if current := host.currentDriver(pci); current != "igb" {
		t.Fatalf("driver after restore = %q, want igb", current)
	}
	if fake.unbinds != 1 || fake.binds != 1 {
		t.Fatalf("unbind=%d bind=%d, want one each", fake.unbinds, fake.binds)
	}
	if _, ok := host.ownershipRecord(pci); ok {
		t.Fatal("product ownership record survived rollback")
	}
}

func TestRestoreLeavesExternallyOwnedUIODeviceAlone(t *testing.T) {
	host, fake, pci := newOwnershipHost(t, false)
	snapshot := Snapshot{TransactionID: "txn-external", StartupConfig: []byte("unix { nodaemon }\n"), Devices: []DeviceState{{
		LinuxInterface: "eth1", PCIAddress: pci, KernelDriver: "igb", BoundDriver: "uio_pci_generic",
		OwnedByProduct: false, LinkUp: false,
	}}}
	if err := host.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if current := host.currentDriver(pci); current != "uio_pci_generic" {
		t.Fatalf("externally owned device was confiscated: driver=%q", current)
	}
	if fake.unbinds != 0 || fake.binds != 0 {
		t.Fatalf("external device was re-bound: unbind=%d bind=%d", fake.unbinds, fake.binds)
	}
}
