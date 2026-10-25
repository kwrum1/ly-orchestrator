package dataplane

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// DPDKVerification records the functional verification performed by the apply
// bootstrap. A DPDK attach is only approved when this says the port can
// actually forward, even if the VPP status string is unreliable.
type DPDKVerification struct {
	LinuxInterface     string    `json:"linux_interface"`
	PCIAddress         string    `json:"pci_address"`
	Mode               string    `json:"mode,omitempty"`
	FunctionalVerified bool      `json:"functional_verified"`
	Verification       string    `json:"verification,omitempty"`
	Method             string    `json:"method,omitempty"`
	LinkState          string    `json:"link_state,omitempty"`
	HardwareLink       string    `json:"hardware_link,omitempty"`
	LinkSpeedMbps      int64     `json:"link_speed_mbps,omitempty"`
	TXBefore           int64     `json:"tx_packets_before,omitempty"`
	TXAfter            int64     `json:"tx_packets_after,omitempty"`
	RXBefore           int64     `json:"rx_packets_before,omitempty"`
	RXAfter            int64     `json:"rx_packets_after,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	ObservedAt         time.Time `json:"observed_at,omitempty"`
}

type DPDKVerificationState struct {
	SchemaVersion int                `json:"schema_version"`
	UpdatedAt     time.Time          `json:"updated_at,omitempty"`
	Interfaces    []DPDKVerification `json:"interfaces"`
}

func LoadDPDKVerification(path string) (DPDKVerificationState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DPDKVerificationState{}, nil
	}
	if err != nil {
		return DPDKVerificationState{}, err
	}
	var state DPDKVerificationState
	if err := json.Unmarshal(data, &state); err != nil {
		return DPDKVerificationState{}, err
	}
	return state, nil
}

func SaveDPDKVerification(path string, state DPDKVerificationState) error {
	state.SchemaVersion = 1
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}
