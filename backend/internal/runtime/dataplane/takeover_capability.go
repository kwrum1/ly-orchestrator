package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultTakeoverGuardTTL bounds how long a native_unsupported verdict may
// suppress a retry. A crash guard is evidence from one environment, not a
// permanent property of the hardware: a driver re-bind, a VPP upgrade, a
// hugepage change or a manual VPP restart must be allowed to prove the port
// again. Callers persist the environment fingerprint alongside the verdict.
const DefaultTakeoverGuardTTL = 10 * time.Minute

// TakeoverCapability is the persisted crash/capability guard. A driver+device
// that crashed VPP or the PMD during takeover is not retried before its TTL
// expires or its environment changes.
type TakeoverCapability struct {
	Driver            string    `json:"driver,omitempty"`
	PCIAddress        string    `json:"pci_address,omitempty"`
	NativeUnsupported bool      `json:"native_unsupported"`
	Reason            string    `json:"reason,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
	// EnvironmentFingerprint identifies the hardware/software environment the
	// verdict was produced in. An empty fingerprint is a legacy entry from a
	// build that cached the verdict forever; it is treated as expired so the
	// next apply actually re-probes instead of skipping the port again.
	EnvironmentFingerprint string    `json:"environment_fingerprint,omitempty"`
	BoundDriver            string    `json:"bound_driver,omitempty"`
	VPPVersion             string    `json:"vpp_version,omitempty"`
	VPPRuntimeID           string    `json:"vpp_runtime_id,omitempty"`
	Hugepages              int       `json:"hugepages,omitempty"`
	ValidUntil             time.Time `json:"valid_until,omitempty"`
}

type TakeoverCapabilityState struct {
	SchemaVersion int                  `json:"schema_version"`
	UpdatedAt     time.Time            `json:"updated_at,omitempty"`
	Devices       []TakeoverCapability `json:"devices"`
}

// CapabilityEnvironment is the minimum set of facts that make a native
// capability verdict meaningful. PCIAddress and KernelDriver identify the
// device, BoundDriver proves which transport currently holds it, and the VPP
// identity and hugepage count describe the runtime that produced the verdict.
type CapabilityEnvironment struct {
	KernelDriver string
	BoundDriver  string
	VPPVersion   string
	VPPRuntimeID string
	Hugepages    int
	PCIAddress   string
}

// Fingerprint is a stable digest of the environment. It is deliberately not
// human-readable but is stored next to the verdict for diagnostics.
func (environment CapabilityEnvironment) Fingerprint() string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"ly-route-takeover-v1",
		strings.TrimSpace(environment.KernelDriver),
		strings.TrimSpace(environment.BoundDriver),
		strings.TrimSpace(environment.VPPVersion),
		strings.TrimSpace(environment.VPPRuntimeID),
		strconv.Itoa(environment.Hugepages),
		strings.TrimSpace(environment.PCIAddress),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
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

// NativeUnsupported reports the structural guard entry. It deliberately does
// not evaluate the TTL or the environment: callers that actually decide
// whether to retry a port must use ActiveUnsupported. Keeping this method
// structural preserves the persisted-file contract and lets diagnostics show
// stale records that are no longer active.
func (state TakeoverCapabilityState) NativeUnsupported(driver, pci string) (TakeoverCapability, bool) {
	// A PCI-specific guard always wins. A driver-only guard (recorded when the
	// exact device could not be identified at the time) applies to every device
	// of that driver, which is the safe direction: never retry a native path
	// that is known to crash on this hardware.
	for _, device := range state.Devices {
		if device.NativeUnsupported && pci != "" && device.PCIAddress == pci {
			return device, true
		}
	}
	if driver != "" {
		for _, device := range state.Devices {
			if device.NativeUnsupported && device.Driver == driver && device.PCIAddress == "" {
				return device, true
			}
		}
	}
	return TakeoverCapability{}, false
}

// ActiveUnsupported reports whether the guard may still suppress a probe. The
// third return value explains an inactive guard (expired, legacy, or produced
// in a different environment) so callers can log why a retry is happening.
func (state TakeoverCapabilityState) ActiveUnsupported(driver, pci string, environment CapabilityEnvironment, now time.Time) (TakeoverCapability, bool, string) {
	capability, exists := state.NativeUnsupported(driver, pci)
	if !exists {
		return TakeoverCapability{}, false, ""
	}
	if strings.TrimSpace(capability.EnvironmentFingerprint) == "" {
		return capability, false, "legacy guard has no environment fingerprint"
	}
	if capability.ValidUntil.IsZero() || !now.Before(capability.ValidUntil) {
		return capability, false, "guard expired"
	}
	if capability.EnvironmentFingerprint != environment.Fingerprint() {
		return capability, false, "environment changed"
	}
	return capability, true, ""
}

// ActiveSupported reports the positive counterpart of ActiveUnsupported: a
// proof recorded after a takeover probe actually attached and verified the
// port. The proof is scoped to the same device and environment (driver, VPP
// runtime, hugepages); a VPP restart or a driver change invalidates it.
func (state TakeoverCapabilityState) ActiveSupported(driver, pci string, environment CapabilityEnvironment, now time.Time) (TakeoverCapability, bool) {
	for _, capability := range state.Devices {
		if capability.NativeUnsupported {
			continue
		}
		matchesDevice := pci != "" && strings.TrimSpace(capability.PCIAddress) == strings.TrimSpace(pci)
		if !matchesDevice && pci == "" && driver != "" {
			matchesDevice = strings.TrimSpace(capability.Driver) == strings.TrimSpace(driver) && strings.TrimSpace(capability.PCIAddress) == ""
		}
		if !matchesDevice {
			continue
		}
		if strings.TrimSpace(capability.EnvironmentFingerprint) == "" {
			continue
		}
		if capability.ValidUntil.IsZero() || !now.Before(capability.ValidUntil) {
			continue
		}
		if capability.EnvironmentFingerprint != environment.Fingerprint() {
			continue
		}
		return capability, true
	}
	return TakeoverCapability{}, false
}

// MarkUnsupported keeps the historical signature for callers that only have a
// driver/PCI reason. It persists the verdict without an environment
// fingerprint, so ActiveUnsupported treats it as a legacy record and the next
// real probe re-runs. Runtime call sites should use
// MarkUnsupportedWithEnvironment.
func (state *TakeoverCapabilityState) MarkUnsupported(driver, pci, reason string) {
	updated := TakeoverCapability{
		Driver:            driver,
		PCIAddress:        pci,
		NativeUnsupported: true,
		Reason:            reason,
		UpdatedAt:         time.Now().UTC(),
	}
	for index := range state.Devices {
		if (pci != "" && state.Devices[index].PCIAddress == pci) ||
			(pci == "" && driver != "" && state.Devices[index].Driver == driver) {
			state.Devices[index] = updated
			return
		}
	}
	state.Devices = append(state.Devices, updated)
}

// MarkUnsupportedWithEnvironment persists a crash verdict together with the
// environment it was observed in and an explicit expiry.
func (state *TakeoverCapabilityState) MarkUnsupportedWithEnvironment(driver, pci, reason string, environment CapabilityEnvironment, ttl time.Duration) {
	if ttl <= 0 {
		ttl = DefaultTakeoverGuardTTL
	}
	now := time.Now().UTC()
	updated := TakeoverCapability{
		Driver:                 driver,
		PCIAddress:             pci,
		NativeUnsupported:      true,
		Reason:                 reason,
		UpdatedAt:              now,
		EnvironmentFingerprint: environment.Fingerprint(),
		BoundDriver:            strings.TrimSpace(environment.BoundDriver),
		VPPVersion:             strings.TrimSpace(environment.VPPVersion),
		VPPRuntimeID:           strings.TrimSpace(environment.VPPRuntimeID),
		Hugepages:              environment.Hugepages,
		ValidUntil:             now.Add(ttl),
	}
	for index := range state.Devices {
		if (pci != "" && state.Devices[index].PCIAddress == pci) ||
			(pci == "" && driver != "" && state.Devices[index].Driver == driver) {
			state.Devices[index] = updated
			return
		}
	}
	state.Devices = append(state.Devices, updated)
}

// MarkSupported records that the same device/probe environment now has a
// positive proof. It updates an existing guard in place (rather than deleting
// it) so the cache always states why a port is trusted and until when. It
// returns true when a persisted record changed. An already-positive entry with
// the same fingerprint is left untouched to avoid rewriting state on every
// status request.
func (state *TakeoverCapabilityState) MarkSupported(driver, pci, reason string, environment CapabilityEnvironment, ttl time.Duration) bool {
	return state.setSupported(driver, pci, reason, environment, ttl, false)
}

func (state *TakeoverCapabilityState) setSupported(driver, pci, reason string, environment CapabilityEnvironment, ttl time.Duration, force bool) bool {
	if pci == "" && driver == "" {
		return false
	}
	if ttl <= 0 {
		ttl = DefaultTakeoverGuardTTL
	}
	changed := false
	for index := range state.Devices {
		if !((pci != "" && state.Devices[index].PCIAddress == pci) ||
			(pci == "" && driver != "" && state.Devices[index].Driver == driver)) {
			continue
		}
		now := time.Now().UTC()
		if !force && !state.Devices[index].NativeUnsupported &&
			state.Devices[index].EnvironmentFingerprint == environment.Fingerprint() &&
			now.Before(state.Devices[index].ValidUntil) {
			continue
		}
		state.Devices[index].NativeUnsupported = false
		state.Devices[index].Reason = reason
		state.Devices[index].UpdatedAt = now
		state.Devices[index].EnvironmentFingerprint = environment.Fingerprint()
		state.Devices[index].BoundDriver = strings.TrimSpace(environment.BoundDriver)
		state.Devices[index].VPPVersion = strings.TrimSpace(environment.VPPVersion)
		state.Devices[index].VPPRuntimeID = strings.TrimSpace(environment.VPPRuntimeID)
		state.Devices[index].Hugepages = environment.Hugepages
		state.Devices[index].ValidUntil = now.Add(ttl)
		changed = true
	}
	return changed
}

// RecordSupported is MarkSupported with upsert semantics. It is used after a
// successful DPDK probe so the capability cache is a positive, fingerprinted
// observation instead of silently disappearing.
func (state *TakeoverCapabilityState) RecordSupported(driver, pci, reason string, environment CapabilityEnvironment, ttl time.Duration) {
	if state.setSupported(driver, pci, reason, environment, ttl, true) {
		return
	}
	if pci == "" && driver == "" {
		return
	}
	if ttl <= 0 {
		ttl = DefaultTakeoverGuardTTL
	}
	now := time.Now().UTC()
	state.Devices = append(state.Devices, TakeoverCapability{
		Driver:                 driver,
		PCIAddress:             pci,
		NativeUnsupported:      false,
		Reason:                 reason,
		UpdatedAt:              now,
		EnvironmentFingerprint: environment.Fingerprint(),
		BoundDriver:            strings.TrimSpace(environment.BoundDriver),
		VPPVersion:             strings.TrimSpace(environment.VPPVersion),
		VPPRuntimeID:           strings.TrimSpace(environment.VPPRuntimeID),
		Hugepages:              environment.Hugepages,
		ValidUntil:             now.Add(ttl),
	})
}
