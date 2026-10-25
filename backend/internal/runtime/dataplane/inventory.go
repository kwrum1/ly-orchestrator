package dataplane

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// InventoryInterface is one operator-configured data interface as persisted for
// the boot ownership preflight. It is deliberately separate from a capability
// proof: it describes intent and hardware identity, not a live VPP attach.
type InventoryInterface struct {
	LinuxInterface string `json:"linux_interface"`
	PCIAddress     string `json:"pci"`
	MACAddress     string `json:"mac,omitempty"`
	KernelDriver   string `json:"kernel_driver,omitempty"`
	Role           string `json:"role,omitempty"`
}

// Inventory is the control API's persisted data-interface mapping. The
// ownership preflight uses it to derive the PCI devices to bind when the
// operator configured interfaces through the API instead of the installer.
type Inventory struct {
	SchemaVersion  int                  `json:"schema_version"`
	Source         string               `json:"source"`
	Management     InventoryInterface   `json:"management"`
	DataInterfaces []InventoryInterface `json:"data_interfaces"`
	UpdatedAt      time.Time            `json:"updated_at,omitempty"`
}

func LoadInventory(path string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Inventory{}, nil
	}
	if err != nil {
		return Inventory{}, err
	}
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return Inventory{}, err
	}
	return inventory, nil
}

func SaveInventory(path string, inventory Inventory) error {
	inventory.SchemaVersion = 1
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}

// LoadOwnershipState exposes the preflight/transaction ownership record to
// callers that need to rebuild an inventory after the Linux netdev disappeared.
func LoadOwnershipState(path string) (OwnershipState, error) {
	return loadOwnership(path)
}
