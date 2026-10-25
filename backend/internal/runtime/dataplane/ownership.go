package dataplane

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

// OwnershipDevice records a PCI device the product moved off its kernel
// driver. The record is written by prepare-vfio.sh at boot and updated by the
// privileged dataplane transaction. It is the only source of truth for
// rollback: a device that was already bound to a DPDK driver by an operator
// (created_by_product=false) is never silently returned to Linux.
type OwnershipDevice struct {
	PCIAddress       string `json:"pci_address"`
	LinuxInterface   string `json:"linux_interface"`
	MACAddress       string `json:"mac_address,omitempty"`
	KernelDriver     string `json:"kernel_driver"`
	Transport        string `json:"transport"`
	CreatedByProduct bool   `json:"created_by_product"`
}

type OwnershipState struct {
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source,omitempty"`
	Transport     string            `json:"transport,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at,omitempty"`
	Devices       []OwnershipDevice `json:"devices"`
}

func (host *LinuxHost) ownershipPath() string {
	return filepath.Join(host.stateDir(), "ownership.json")
}

func loadOwnership(path string) (OwnershipState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return OwnershipState{}, nil
	}
	if err != nil {
		return OwnershipState{}, err
	}
	var state OwnershipState
	if err := json.Unmarshal(data, &state); err != nil {
		return OwnershipState{}, err
	}
	return state, nil
}

func saveOwnership(path string, state OwnershipState) error {
	state.SchemaVersion = 1
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}

func (host *LinuxHost) ownershipRecord(pci string) (OwnershipDevice, bool) {
	state, err := loadOwnership(host.ownershipPath())
	if err != nil {
		return OwnershipDevice{}, false
	}
	for _, device := range state.Devices {
		if strings.TrimSpace(device.PCIAddress) == pci {
			return device, true
		}
	}
	return OwnershipDevice{}, false
}

func (host *LinuxHost) recordOwnership(device DeviceState, targetDriver string, created bool) error {
	path := host.ownershipPath()
	state, err := loadOwnership(path)
	if err != nil {
		return err
	}
	transport, err := dpdkTransportForDriver(targetDriver)
	if err != nil {
		return err
	}
	updated := false
	for index := range state.Devices {
		if state.Devices[index].PCIAddress != device.PCIAddress {
			continue
		}
		state.Devices[index].LinuxInterface = device.LinuxInterface
		state.Devices[index].KernelDriver = device.KernelDriver
		state.Devices[index].Transport = transport
		state.Devices[index].CreatedByProduct = state.Devices[index].CreatedByProduct || created
		updated = true
		break
	}
	if !updated {
		state.Devices = append(state.Devices, OwnershipDevice{
			PCIAddress:       device.PCIAddress,
			LinuxInterface:   device.LinuxInterface,
			KernelDriver:     device.KernelDriver,
			Transport:        transport,
			CreatedByProduct: created,
		})
	}
	state.Source = "dataplane-transaction"
	state.Transport = transport
	return saveOwnership(path, state)
}

// releaseOwnership removes product-created records after Restore returned the
// NIC to Linux. Records for externally owned devices are left untouched.
func (host *LinuxHost) releaseOwnership(pci string) error {
	path := host.ownershipPath()
	state, err := loadOwnership(path)
	if err != nil {
		return err
	}
	kept := state.Devices[:0]
	removed := false
	for _, device := range state.Devices {
		if device.PCIAddress == pci && device.CreatedByProduct {
			removed = true
			continue
		}
		kept = append(kept, device)
	}
	if !removed {
		return nil
	}
	state.Devices = kept
	if len(state.Devices) == 0 {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return saveOwnership(path, state)
}

func dpdkTransportForDriver(driver string) (string, error) {
	switch strings.TrimSpace(driver) {
	case "vfio-pci":
		return "vfio", nil
	case "uio_pci_generic":
		return "uio", nil
	default:
		return "", errors.New("unsupported DPDK binding driver " + driver)
	}
}

func dpdkDriverForMode(mode vpp.NativeMode) (string, error) {
	switch mode {
	case vpp.NativeModeDPDKVFIO:
		return "vfio-pci", nil
	case vpp.NativeModeDPDKUIO:
		return "uio_pci_generic", nil
	default:
		return "", errors.New("unsupported DPDK binding mode " + string(mode))
	}
}
