package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
)

// This file implements the rule-first adoption/migration (WS-AK).
//
// The static rule library is authoritative for the mode of a data port, but the
// DPDK plugin is disabled in startup.conf until a port is bound. That made the
// very first enumeration fall back to AF_PACKET for igb/igc even though the rule
// prefers dpdk_uio, and E38 then froze the fallback forever ("an existing mode
// is never upgraded"). The result was an appliance stuck on the compatibility
// path with no way to reach the mode its own rule library selects.
//
// The rule preference now gets exactly one transactional attempt:
//
//   - a first adoption keeps the rule's DPDK choice even while the plugin is
//     still disabled (the bind operation enables it), so the watcher no longer
//     auto-adopts AF_PACKET before that attempt;
//   - a port already live on AF_PACKET is migrated once through the ordinary
//     af_packet -> dpdk_uio switch (prepare-vfio bind, startup.conf dpdk block
//     and plugin enable, VPP restart, live readback);
//   - a failure rolls back to AF_PACKET, persists dpdk_attempt_failed and is
//     never replayed (neither in this boot nor after a reboot);
//   - a success clears the marker and the verification record names dpdk_uio.
//
// Everything is still backend-owned: no API request and no operator switch is
// involved, and the management port is never a candidate.

// dpdkAttemptStatePath is the durable record of the one rule-preferred DPDK
// attempt per port. It is deliberately separate from dpdk-verification.json:
// the verification file is rewritten by every apply and watcher tick, while the
// failure marker must survive both.
func dpdkAttemptStatePath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_DPDK_ATTEMPTS")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "dpdk-attempts.json")
}

type dpdkAttempt struct {
	LinuxInterface    string    `json:"linux_interface,omitempty"`
	PCIAddress        string    `json:"pci_address,omitempty"`
	Mode              string    `json:"mode,omitempty"`
	DPDKAttemptFailed bool      `json:"dpdk_attempt_failed"`
	Reason            string    `json:"reason,omitempty"`
	AttemptedAt       time.Time `json:"attempted_at,omitempty"`
}

type dpdkAttemptState struct {
	SchemaVersion int           `json:"schema_version"`
	UpdatedAt     time.Time     `json:"updated_at,omitempty"`
	Attempts      []dpdkAttempt `json:"attempts"`
}

func loadDPDKAttemptState() dpdkAttemptState {
	state := dpdkAttemptState{}
	data, err := os.ReadFile(dpdkAttemptStatePath())
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return dpdkAttemptState{}
	}
	return state
}

func saveDPDKAttemptState(state dpdkAttemptState) {
	path := dpdkAttemptStatePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("dataplane rule: persist DPDK attempt state: %v", err)
		return
	}
	state.SchemaVersion = 1
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		log.Printf("dataplane rule: persist DPDK attempt state: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("dataplane rule: persist DPDK attempt state: %v", err)
	}
}

// sameDPDKAttempt matches a record to a port. The PCI address is the stable
// identity (a DPDK-bound device has no netdev name), with the Linux name as the
// fallback when the address is not known.
// inventoryIdentityKey identifies one data interface in the operator inventory.
// PCI is stable across netdev renames; the Linux name is the fallback.
func inventoryIdentityKey(entry dataplane.InventoryInterface) string {
	if pci := strings.TrimSpace(entry.PCIAddress); pci != "" {
		return "pci:" + strings.ToLower(pci)
	}
	return "name:" + strings.TrimSpace(entry.LinuxInterface)
}

// persistRulePreferredInventory records the rule-preferred DPDK data ports in
// the operator inventory even when no LAN/WAN role is configured yet.
//
// The boot ownership preflight derives the PCI set to bind from this file. A
// role-less appliance used to write an inventory without data interfaces, so
// the next boot found nothing to bind, cleared the managed DPDK block and
// released every port to the kernel (WS-AK risk #2). Registering the ports at
// the moment the rule selects DPDK keeps the takeover across a reboot.
//
// Existing records are never overwritten: an entry that already exists is only
// completed with missing identity fields, and its role is preserved.
func (server *Server) persistRulePreferredInventory(ctx context.Context, names []string) {
	if len(names) == 0 {
		return
	}
	path := dataplaneInventoryPath()
	inventory, err := dataplane.LoadInventory(path)
	if err != nil {
		log.Printf("dataplane inventory: load %s: %v", path, err)
		return
	}
	management := strings.TrimSpace(server.managementInterfaceID(ctx))
	if strings.TrimSpace(inventory.Management.LinuxInterface) == "" && management != "" {
		if item, ok := readInventoryInterface(management); ok {
			item.Role = "management"
			inventory.Management = item
		}
	}
	index := map[string]int{}
	for position, entry := range inventory.DataInterfaces {
		index[inventoryIdentityKey(entry)] = position
		if name := strings.TrimSpace(entry.LinuxInterface); name != "" {
			index["name:"+name] = position
		}
	}
	changed := false
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" || name == management {
			continue
		}
		if _, _, ok := server.rulePreferredDPDKTarget(ctx, name); !ok {
			continue
		}
		item, ok := readInventoryInterface(name)
		if !ok {
			continue
		}
		position, exists := index[inventoryIdentityKey(item)]
		if !exists {
			position, exists = index["name:"+name]
		}
		if exists {
			entry := &inventory.DataInterfaces[position]
			if strings.TrimSpace(entry.PCIAddress) == "" {
				entry.PCIAddress = item.PCIAddress
				changed = true
			}
			if strings.TrimSpace(entry.MACAddress) == "" {
				entry.MACAddress = item.MACAddress
				changed = true
			}
			if strings.TrimSpace(entry.KernelDriver) == "" || takeoverModeDestructive(takeoverModeFromDriver(entry.KernelDriver)) {
				entry.KernelDriver = item.KernelDriver
				changed = true
			}
			if strings.TrimSpace(entry.Role) == "" {
				entry.Role = "data"
				changed = true
			}
			continue
		}
		item.Role = "data"
		inventory.DataInterfaces = append(inventory.DataInterfaces, item)
		index[inventoryIdentityKey(item)] = len(inventory.DataInterfaces) - 1
		index["name:"+name] = len(inventory.DataInterfaces) - 1
		changed = true
	}
	if !changed {
		return
	}
	sort.Slice(inventory.DataInterfaces, func(left, right int) bool {
		return inventory.DataInterfaces[left].LinuxInterface < inventory.DataInterfaces[right].LinuxInterface
	})
	inventory.Source = "ly-route-control-api"
	inventory.UpdatedAt = server.now().UTC()
	if err := dataplane.SaveInventory(path, inventory); err != nil {
		log.Printf("dataplane inventory: persist %s: %v", path, err)
		return
	}
	log.Printf("dataplane inventory: recorded the rule-preferred DPDK data port(s) for reboot persistence")
}

func sameDPDKAttempt(record dpdkAttempt, pci, name string) bool {
	pci = strings.TrimSpace(pci)
	name = strings.TrimSpace(name)
	if pci != "" && strings.TrimSpace(record.PCIAddress) != "" {
		return samePCI(pci, record.PCIAddress)
	}
	return name != "" && strings.TrimSpace(record.LinuxInterface) == name
}

// dpdkAttemptFailure reports the durable "do not try DPDK again" marker.
func dpdkAttemptFailure(pci, name string) (bool, string) {
	for _, record := range loadDPDKAttemptState().Attempts {
		if sameDPDKAttempt(record, pci, name) && record.DPDKAttemptFailed {
			return true, record.Reason
		}
	}
	return false, ""
}

// recordDPDKAttemptFailed persists the failure and logs the stable
// dpdk_attempt_failed event. Callers have already rolled the port back to
// AF_PACKET; this only stops the next automatic attempt.
func recordDPDKAttemptFailed(name, pci, mode, reason string) {
	pci = strings.TrimSpace(pci)
	name = strings.TrimSpace(name)
	mode = strings.TrimSpace(mode)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "the DPDK takeover attempt failed"
	}
	state := loadDPDKAttemptState()
	updated := dpdkAttempt{
		LinuxInterface:    name,
		PCIAddress:        pci,
		Mode:              mode,
		DPDKAttemptFailed: true,
		Reason:            reason,
		AttemptedAt:       time.Now().UTC(),
	}
	replaced := false
	for index := range state.Attempts {
		if sameDPDKAttempt(state.Attempts[index], pci, name) {
			state.Attempts[index] = updated
			replaced = true
			break
		}
	}
	if !replaced {
		state.Attempts = append(state.Attempts, updated)
	}
	saveDPDKAttemptState(state)
	log.Printf("dataplane rule: dpdk_attempt_failed interface=%s pci=%s mode=%s reason=%s", name, pci, mode, reason)
}

// recordDPDKAttemptSucceeded clears the marker: the rule's preferred mode is
// live now, so there is nothing left to suppress.
// clearDPDKAttempt removes the marker for a port after an explicit ownership or
// role event. Those events are the documented way an operator asks the appliance
// to reconsider a port (for example after configuring LAN/WAN), so a previous
// automatic attempt must not suppress the decision forever.
func clearDPDKAttempt(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	recordDPDKAttemptSucceeded(name, "", "")
}

func recordDPDKAttemptSucceeded(name, pci, mode string) {
	state := loadDPDKAttemptState()
	kept := make([]dpdkAttempt, 0, len(state.Attempts))
	removed := false
	for _, record := range state.Attempts {
		if sameDPDKAttempt(record, pci, name) {
			removed = true
			continue
		}
		kept = append(kept, record)
	}
	if !removed {
		return
	}
	state.Attempts = kept
	saveDPDKAttemptState(state)
	log.Printf("dataplane rule: dpdk_attempt_cleared interface=%s pci=%s mode=%s", strings.TrimSpace(name), strings.TrimSpace(pci), strings.TrimSpace(mode))
}

// destructiveFirstBindAvailable reports whether a destructive first bind (and
// therefore a rule-preferred migration) can actually run right now. A missing
// prepare-vfio.sh or an active bootstrap cooldown means the port must stay on
// the compatibility path instead of being left unowned.
func destructiveFirstBindAvailable() bool {
	command := envOrDefault("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", "/usr/lib/ly-route/prepare-vfio.sh")
	info, err := os.Stat(command)
	if err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	if !dataplaneBootstrapBlockedUntil.IsZero() && time.Now().Before(dataplaneBootstrapBlockedUntil) {
		return false
	}
	return true
}

// rulePreferredDPDKTarget returns the DPDK mode the static rule library selects
// for this port, independent of the currently disabled DPDK plugin. The plugin
// flag is not evidence about the NIC: startup.conf has it disabled until
// prepare-vfio.sh binds the first device and enables it.
func (server *Server) rulePreferredDPDKTarget(ctx context.Context, name string) (string, takeover.Rule, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == strings.TrimSpace(server.managementInterfaceID(ctx)) {
		return "", takeover.Rule{}, false
	}
	facts := server.takeoverFactsForPort(ctx, name)
	if !facts.Physical || facts.Management {
		return "", takeover.Rule{}, false
	}
	rule, matched := takeover.MatchRule(facts.Driver, facts.VendorDevice)
	if !matched {
		return "", takeover.Rule{}, false
	}
	switch rule.Preferred {
	case takeover.ModeDPDKUIO, takeover.ModeDPDKVFIO:
	default:
		return "", takeover.Rule{}, false
	}
	if !rule.Allows(rule.Preferred) {
		return "", takeover.Rule{}, false
	}
	if rule.Preferred == takeover.ModeDPDKVFIO && !facts.VFIOAllowedNow() {
		return "", takeover.Rule{}, false
	}
	return string(rule.Preferred), rule, true
}

// rulePreferredMigrationCandidate is the eligibility test without side effects.
// It answers "may the one rule-preferred DPDK attempt happen for this port?".
func (server *Server) rulePreferredMigrationCandidate(ctx context.Context, name, currentMode string, capabilities dataplane.TakeoverCapabilityState) (string, bool) {
	if strings.TrimSpace(currentMode) != takeoverModeAFPacket {
		return "", false
	}
	target, _, ok := server.rulePreferredDPDKTarget(ctx, name)
	if !ok {
		return "", false
	}
	// Hugepages are a hard environment prerequisite: without them the
	// transactional bind would restart VPP only to fail. The plugin flag is
	// not a prerequisite (the bind enables it).
	if _, _, enough := hugepagesShortfall(); !enough {
		return "", false
	}
	if !destructiveFirstBindAvailable() {
		return "", false
	}
	driver := firstNonEmptyTrimmed(portKernelDriver(name), inventoryDriver(name))
	pci := inventoryPCI(name)
	if server.takeoverGuardActive(capabilities, driver, pci, name) {
		return "", false
	}
	if failed, _ := dpdkAttemptFailure(pci, name); failed {
		return "", false
	}
	return target, true
}

// rulePreferredMigration reports whether a port that is already live on
// AF_PACKET still owes its one rule-preferred migration. It deliberately does
// not claim the per-boot switch budget: the caller may still be throttled (the
// watcher defers on a fresh auto-adoption), and claiming here would consume the
// only attempt without performing it. The caller claims immediately before the
// switch.
func (server *Server) rulePreferredMigration(ctx context.Context, name, currentMode string, capabilities dataplane.TakeoverCapabilityState) (string, bool) {
	return server.rulePreferredMigrationCandidate(ctx, name, currentMode, capabilities)
}
