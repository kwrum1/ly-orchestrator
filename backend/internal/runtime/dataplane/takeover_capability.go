package dataplane

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// TakeoverCapability is the persisted crash/capability guard. A driver+device
// that crashed VPP or the PMD during takeover is never retried in a loop.
type TakeoverCapability struct {
	Driver            string    `json:"driver,omitempty"`
	PCIAddress        string    `json:"pci_address,omitempty"`
	NativeUnsupported bool      `json:"native_unsupported"`
	Reason            string    `json:"reason,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

type TakeoverCapabilityState struct {
	SchemaVersion int                  `json:"schema_version"`
	UpdatedAt     time.Time            `json:"updated_at,omitempty"`
	Devices       []TakeoverCapability `json:"devices"`
}

func LoadTakeoverCapabilities(path string) (TakeoverCapabilityState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return TakeoverCapabilityState{}, nil
	}
	if err != nil {
		return TakeoverCapabilityState{}, err
	}
	var state TakeoverCapabilityState
	if err := json.Unmarshal(data, &state); err != nil {
		return TakeoverCapabilityState{}, err
	}
	return state, nil
}

func SaveTakeoverCapabilities(path string, state TakeoverCapabilityState) error {
	state.SchemaVersion = 1
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}

func (state TakeoverCapabilityState) NativeUnsupported(driver, pci string) (TakeoverCapability, bool) {
	for _, device := range state.Devices {
		if pci != "" && device.PCIAddress != "" && device.PCIAddress == pci {
			return device, device.NativeUnsupported
		}
		if driver != "" && device.Driver == driver && device.PCIAddress == "" {
			return device, device.NativeUnsupported
		}
	}
	return TakeoverCapability{}, false
}

func (state *TakeoverCapabilityState) MarkUnsupported(driver, pci, reason string) {
	for index := range state.Devices {
		if (pci != "" && state.Devices[index].PCIAddress == pci) ||
			(pci == "" && driver != "" && state.Devices[index].Driver == driver) {
			state.Devices[index].NativeUnsupported = true
			state.Devices[index].Reason = reason
			state.Devices[index].UpdatedAt = time.Now().UTC()
			return
		}
	}
	state.Devices = append(state.Devices, TakeoverCapability{
		Driver:            driver,
		PCIAddress:        pci,
		NativeUnsupported: true,
		Reason:            reason,
		UpdatedAt:         time.Now().UTC(),
	})
}
