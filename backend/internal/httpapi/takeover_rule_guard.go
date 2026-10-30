package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
	"ly-route/backend/internal/runtime/vpp"
)

// This file makes the static rule library (internal/runtime/takeover) the only
// authority for transport selection.
//
// dpdk-verification.json, active.json and the cached capability report are
// observations. They are useful to reconcile an already running attachment, but
// they are never allowed to drive the port into a mode the rule library forbids.
// The igc AF_XDP crash showed why: a stale "af_xdp_copy verified" record
// outranked the rule and the next automatic switch created an AF_XDP TX path
// that aborted VPP with SIGABRT/SIGSEGV.

// staleModeRecordPath is the append-only evidence log for modes that were
// dropped because the rule library no longer permits them.
func staleModeRecordPath() string {
	if path := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STALE_MODES")); path != "" {
		return path
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "stale-modes.jsonl")
}

type staleModeRecord struct {
	Event     string `json:"event"`
	Interface string `json:"linux_interface,omitempty"`
	PCI       string `json:"pci_address,omitempty"`
	Driver    string `json:"driver,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Source    string `json:"source,omitempty"`
	Reason    string `json:"reason,omitempty"`
	At        string `json:"at"`
}

// recordStaleModeDropped writes both the control-plane log and a durable JSONL
// record. The event name is stable so an operator/tests can prove that a stale
// AF_XDP record was ignored rather than silently removed.
func recordStaleModeDropped(name, pci, driver, mode, source, reason string) {
	record := staleModeRecord{
		Event:     "stale_mode_dropped",
		Interface: strings.TrimSpace(name),
		PCI:       strings.TrimSpace(pci),
		Driver:    strings.TrimSpace(driver),
		Mode:      strings.TrimSpace(mode),
		Source:    strings.TrimSpace(source),
		Reason:    strings.TrimSpace(reason),
		At:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	log.Printf("dataplane rule: stale_mode_dropped interface=%s pci=%s driver=%s mode=%s source=%s reason=%s",
		record.Interface, record.PCI, record.Driver, record.Mode, record.Source, record.Reason)
	path := staleModeRecordPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// portKernelDriver reads the driver from sysfs first and the persisted
// inventory second. A DPDK-bound port has no netdev driver; the ownership
// record restored by recordedKernelDriver keeps the rule match possible.
func portKernelDriver(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if driver, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot(), "class/net", name, "device", "driver")); err == nil {
		base := filepath.Base(driver)
		if base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	if driver := strings.TrimSpace(inventoryDriver(name)); driver != "" {
		return driver
	}
	return strings.TrimSpace(recordedKernelDriver(inventoryPCI(name)))
}

// portVendorDevice reads the PCI ids the rule library matches on.
func portVendorDevice(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	device := filepath.Join(sysfsRoot(), "class/net", name, "device")
	return takeover.ParseDeviceID(
		readTrimmedFile(filepath.Join(device, "vendor")),
		readTrimmedFile(filepath.Join(device, "device")),
	)
}

// takeoverPortModeAllowed is the rule gate used by every httpapi decision path.
func takeoverPortModeAllowed(name, pci, mode string) bool {
	driver := portKernelDriver(name)
	if driver == "" {
		driver = recordedKernelDriver(pci)
	}
	vendorDevice := portVendorDevice(name)
	return takeover.ModeAllowed(driver, vendorDevice, takeover.Mode(mode))
}

func (server *Server) takeoverPortModeAllowed(ctx context.Context, name, pci, mode string) bool {
	facts := server.takeoverFactsForPort(ctx, name)
	return takeover.ModeAllowed(
		firstNonEmptyTrimmed(facts.Driver, portKernelDriver(name), recordedKernelDriver(pci)),
		firstNonEmptyTrimmed(facts.VendorDevice, portVendorDevice(name)),
		takeover.Mode(mode),
	)
}

// dropForbiddenModeRecords removes a port's verification/active entries whose
// mode the rule library forbids. It returns the modes it dropped so the caller
// can journal them. User configuration is never touched: only runtime proofs
// live in these files.
func dropForbiddenModeRecords(name, pci, driver, vendorDevice string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	dropped := []string{}
	if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
		kept := make([]dataplane.DPDKVerification, 0, len(state.Interfaces))
		changed := false
		for _, entry := range state.Interfaces {
			if strings.TrimSpace(entry.LinuxInterface) != name || pciMismatch(pci, entry.PCIAddress) {
				kept = append(kept, entry)
				continue
			}
			mode := takeoverModeFromDriver(entry.Mode)
			if mode == "" || takeover.ModeAllowed(driver, vendorDevice, takeover.Mode(mode)) {
				kept = append(kept, entry)
				continue
			}
			dropped = append(dropped, mode)
			changed = true
		}
		if changed {
			state.Interfaces = kept
			if err := dataplane.SaveDPDKVerification(dataplaneVerificationPath(), state); err != nil {
				log.Printf("dataplane rule: drop forbidden verification for %s: %v", name, err)
			}
		}
	}
	// active.json is the committed generation. It is rewritten by the next
	// successful apply, but a forbidden entry must not be readable as a mode
	// decision in the meantime.
	activePath := envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json")
	if data, err := os.ReadFile(activePath); err == nil {
		var document map[string]any
		if json.Unmarshal(data, &document) == nil {
			if path, ok := document["path"].(map[string]any); ok {
				if raw, ok := path["attachments"].([]any); ok {
					kept := make([]any, 0, len(raw))
					changed := false
					for _, item := range raw {
						attachment, _ := item.(map[string]any)
						if attachment == nil {
							kept = append(kept, item)
							continue
						}
						linuxName, _ := attachment["linux_interface"].(string)
						recordedPCI, _ := attachment["pci_address"].(string)
						if strings.TrimSpace(linuxName) != name || pciMismatch(pci, recordedPCI) {
							kept = append(kept, item)
							continue
						}
						tier, _ := attachment["tier"].(string)
						hook, _ := attachment["hook"].(string)
						mode, _ := attachment["mode"].(string)
						observed := takeoverModeFromAttachment(vpp.DataplaneTier(tier), vpp.NativeHook(hook), vpp.NativeMode(mode))
						if observed == "" || takeover.ModeAllowed(driver, vendorDevice, takeover.Mode(observed)) {
							kept = append(kept, item)
							continue
						}
						dropped = append(dropped, observed)
						changed = true
					}
					if changed {
						path["attachments"] = kept
						if encoded, err := json.MarshalIndent(document, "", "  "); err == nil {
							tmp := activePath + ".tmp"
							if err := os.WriteFile(tmp, append(encoded, '\n'), 0o600); err == nil {
								_ = os.Rename(tmp, activePath)
							}
						}
					}
				}
			}
		}
	}
	return dropped
}

// enforceTakeoverRuleLibrary removes candidate proofs the rule library forbids.
// It runs after preferCommittedNativeCandidates so even a stale verification
// that injected a synthetic proof cannot reach selection.
func (server *Server) enforceTakeoverRuleLibrary(ctx context.Context, nativePath *vpp.NativePathRequest) {
	if nativePath == nil {
		return
	}
	for index := range nativePath.Assignments {
		assignment := &nativePath.Assignments[index]
		name := strings.TrimSpace(assignment.LinuxInterface)
		if name == "" {
			continue
		}
		driver := server.portRuleDriver(ctx, name, assignment)
		vendorDevice := portVendorDevice(name)
		allowed := func(mode string) bool {
			return takeover.ModeAllowed(driver, vendorDevice, takeover.Mode(mode))
		}
		kept := make([]vpp.CapabilityProof, 0, len(assignment.Candidates))
		for _, candidate := range assignment.Candidates {
			mode := takeoverModeFromAttachment(candidate.Tier, candidate.Hook, candidate.Mode)
			if mode == "" || allowed(mode) {
				kept = append(kept, candidate)
				continue
			}
			recordStaleModeDropped(name, firstNonEmptyTrimmed(candidate.PCIAddress, assignment.Proof.PCIAddress), driver, mode, "capability_candidate", "the rule library forbids this mode for the driver")
		}
		if mode := takeoverModeFromAttachment(assignment.Proof.Tier, assignment.Proof.Hook, assignment.Proof.Mode); mode != "" && !allowed(mode) {
			recordStaleModeDropped(name, assignment.Proof.PCIAddress, driver, mode, "assignment_proof", "the rule library forbids this mode for the driver")
			assignment.Proof = vpp.CapabilityProof{}
		}
		if len(kept) == 0 && assignment.Proof.Hook != "" {
			kept = append(kept, assignment.Proof)
		}
		assignment.Candidates = kept
	}
}

func (server *Server) portRuleDriver(ctx context.Context, name string, assignment *vpp.NativeAssignment) string {
	facts := server.takeoverFactsForPort(ctx, name)
	driver := firstNonEmptyTrimmed(facts.Driver, portKernelDriver(name))
	if assignment != nil {
		driver = firstNonEmptyTrimmed(driver,
			assignment.Proof.KernelDriver,
			recordedKernelDriver(assignment.Proof.PCIAddress))
		for _, candidate := range assignment.Candidates {
			if strings.TrimSpace(candidate.KernelDriver) != "" {
				driver = firstNonEmptyTrimmed(driver, candidate.KernelDriver)
				break
			}
		}
	}
	return driver
}

// automaticSwitchBudget implements "at most one automatic mode switch per boot
// per port". The rule-mandated migration of a forbidden live mode uses the same
// budget, so a failed cleanup cannot turn into a replay loop.
var automaticSwitchBudget = struct {
	sync.Mutex
	boot  string
	ports map[string]bool
}{ports: map[string]bool{}}

func currentBootID() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_BOOT_ID")); value != "" {
		return value
	}
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			return value
		}
	}
	return "unknown-boot"
}

func claimAutomaticModeSwitch(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	boot := currentBootID()
	automaticSwitchBudget.Lock()
	defer automaticSwitchBudget.Unlock()
	if automaticSwitchBudget.boot != boot {
		automaticSwitchBudget.boot = boot
		automaticSwitchBudget.ports = map[string]bool{}
	}
	if automaticSwitchBudget.ports[name] {
		return false
	}
	automaticSwitchBudget.ports[name] = true
	return true
}

// resetAutomaticModeSwitchBudget is called only by an explicit ownership/role
// event. It is deliberately not called by apply or by the watcher.
func resetAutomaticModeSwitchBudget(name string) {
	name = strings.TrimSpace(name)
	automaticSwitchBudget.Lock()
	defer automaticSwitchBudget.Unlock()
	delete(automaticSwitchBudget.ports, name)
	// An explicit ownership/role event also re-arms the rule-preferred DPDK
	// attempt: the operator changed the port's role, so a previous automatic
	// verdict (for example "not a configured data interface") no longer holds.
	clearDPDKAttempt(name)
}

func resetAllAutomaticModeSwitchBudgetsForTest() {
	automaticSwitchBudget.Lock()
	defer automaticSwitchBudget.Unlock()
	automaticSwitchBudget.boot = ""
	automaticSwitchBudget.ports = map[string]bool{}
}

// migrateForbiddenTakeoverModes repairs a live attachment whose mode the rule
// library forbids (the igc AF_XDP residue). The migration is deliberately
// conservative:
//
//   - the stale verification/active records are dropped first, so no later
//     planning step can resurrect them;
//   - the target is AF_PACKET, the non-destructive compatibility mode. DPDK is
//     never attempted during a repair: another destructive bind/restart on a
//     device that just crashed VPP is exactly what must not happen;
//   - the existing transaction tears AF_XDP down with
//     `delete interface af_xdp sw_if_index`, detaches the stale XDP program and
//     reads the absence back before creating the AF_PACKET host interface;
//   - one switch per boot per port, and a failed switch is not replayed.
func (server *Server) migrateForbiddenTakeoverModes(ctx context.Context) {
	names := server.takeoverDataInterfaces(ctx)
	if len(names) == 0 {
		return
	}
	vppNames := server.vppInterfaceNames(ctx)
	for _, name := range names {
		snapshot := server.detectTakeoverSnapshot(ctx, name, vppNames)
		mode := strings.TrimSpace(snapshot.CurrentMode)
		if !snapshot.Owned || mode == "" {
			continue
		}
		driver := firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(name))
		vendorDevice := portVendorDevice(name)
		if driver == "" || strings.TrimSpace(snapshot.PCIAddress) == "" {
			// An incomplete snapshot is not evidence that the mode is
			// forbidden. Dropping the records here orphaned a uio-bound port
			// (no netdev, no ownership) after a superseded apply, and the
			// follow-up AF_PACKET migration could not even find the PCI
			// address. Keep the attachment and let the next reconcile repair
			// the evidence instead.
			log.Printf("dataplane rule: %s has an incomplete hardware snapshot (driver=%q pci=%q); keeping %s instead of a destructive repair", name, driver, snapshot.PCIAddress, mode)
			continue
		}
		if takeover.ModeAllowed(driver, vendorDevice, takeover.Mode(mode)) {
			continue
		}
		if dropped := dropForbiddenModeRecords(name, snapshot.PCIAddress, driver, vendorDevice); len(dropped) > 0 {
			log.Printf("dataplane rule: %s dropped stale %s record(s) before migration", name, strings.Join(dropped, ","))
		}
		if !snapshot.ModeConfirmed {
			log.Printf("dataplane rule: %s is recorded as forbidden %s but the live mode is not confirmed; refusing an inferred destructive switch", name, mode)
			continue
		}
		if !claimAutomaticModeSwitch(name) {
			// The one allowed switch was already spent (and failed) this boot.
			// Do not write another stale record every tick or replay the mode.
			continue
		}
		recordStaleModeDropped(name, snapshot.PCIAddress, driver, mode, "live_or_recorded", "the observed mode is forbidden by the static rule library")
		target := string(takeover.ModeAFPacket)
		log.Printf("dataplane rule: migrating %s from forbidden %s to %s", name, mode, target)
		if err := server.applyTakeoverSwitch(ctx, snapshot, target, true); err != nil {
			log.Printf("dataplane rule: migration %s -> %s failed: %v; not retrying this boot", name, target, err)
			dataplaneSwitchRetryAfter[name] = time.Now().Add(switchFailureCooldown(err))
			recordNativeRepair(takeoverTransition{
				Interface:    name,
				Netdev:       firstNonEmptyTrimmed(snapshot.Netdev, name),
				VPPInterface: firstNonEmptyTrimmed(snapshot.VPPInterface, "lyroute-"+name),
				FromMode:     mode,
				ToMode:       target,
			}, "the static rule library forbids "+mode+" and the migration to af_packet failed", snapshot.EvidenceString())
			server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.rule.migrate", "failed", "from="+mode+" to="+target, "")
			continue
		}
		if dropped := dropForbiddenModeRecords(name, snapshot.PCIAddress, driver, vendorDevice); len(dropped) > 0 {
			log.Printf("dataplane rule: %s still had stale %s record(s) after migration; dropped", name, strings.Join(dropped, ","))
		}
		server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.rule.migrate", "success", "from="+mode+" to="+target, "")
	}
}

// snapshotEvidence is a small helper so a repair record can quote the exact
// evidence the decision used.
func (snapshot takeoverSnapshot) EvidenceString() string {
	return strings.Join(snapshot.Evidence, "; ")
}
