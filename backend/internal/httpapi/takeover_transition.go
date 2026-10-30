package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/takeover"
	"ly-route/backend/internal/runtime/vpp"
)

// Takeover modes are the explicit vocabulary of the interface ownership state
// machine. They are the same strings the API, the journal and the UI use, so a
// recorded decision can be replayed without translating between vocabularies.
const (
	takeoverModeNone          = ""
	takeoverModeAFPacket      = "af_packet"
	takeoverModeAFXDPZeroCopy = "af_xdp_zero_copy"
	takeoverModeAFXDPCopy     = "af_xdp_copy"
	takeoverModeRDMA          = "rdma"
	takeoverModeDPDKUIO       = "dpdk_uio"
	takeoverModeDPDKVFIO      = "dpdk_vfio"
)

// takeoverValidationError is returned when a caller names a mode the product
// does not implement. It is never a hardware verdict.
type takeoverValidationError struct {
	Interface string
	Mode      string
	Reason    string
}

func (err *takeoverValidationError) Error() string {
	return fmt.Sprintf("takeover mode %q for %s is not valid: %s", err.Mode, err.Interface, err.Reason)
}

func takeoverModeValid(mode string) bool {
	switch mode {
	case takeoverModeAFPacket, takeoverModeAFXDPZeroCopy, takeoverModeAFXDPCopy,
		takeoverModeRDMA, takeoverModeDPDKUIO, takeoverModeDPDKVFIO:
		return true
	}
	return false
}

func takeoverModeDestructive(mode string) bool {
	return mode == takeoverModeDPDKUIO || mode == takeoverModeDPDKVFIO
}

func takeoverModeTransport(mode string) string {
	switch mode {
	case takeoverModeDPDKVFIO:
		return "vfio"
	case takeoverModeDPDKUIO:
		return "uio"
	}
	return ""
}

func takeoverModeLabel(mode string) string {
	if mode == takeoverModeNone {
		return "unowned"
	}
	return mode
}

// takeoverModeDriver maps a takeover mode to the kernel driver that implements
// it. Non-destructive modes keep the original kernel driver.
func takeoverModeDriver(mode string) string {
	switch mode {
	case takeoverModeDPDKVFIO:
		return "vfio-pci"
	case takeoverModeDPDKUIO:
		return "uio_pci_generic"
	}
	return ""
}

// takeoverSnapshot is the observed current state of one port. It is evidence,
// not configuration: every runtime handle is re-read before a decision.
type takeoverSnapshot struct {
	LinuxInterface string
	PCIAddress     string
	Netdev         string
	KernelDriver   string
	// BoundDriver is the driver currently holding the PCI device (the original
	// kernel driver or vfio-pci/uio_pci_generic).
	BoundDriver string
	// PreviousDriver is the transport recorded by the last explicit switch.
	PreviousDriver string
	VPPInterface   string
	CurrentMode    string
	Owned          bool
	// ModeConfirmed is false when the current mode was inferred from a VPP
	// interface name only. An unconfirmed mode cannot be rolled back safely.
	ModeConfirmed bool
	// InterfaceNameOnly marks the weak "a VPP interface with the product name
	// exists" signal. It is never enough to report a port as taken over.
	InterfaceNameOnly bool
	// Duplicate marks a live state with more than one interface carrying the
	// product name. It is never a working attachment.
	Duplicate  bool
	SWIfIndex  uint32
	QueueFDs   []int
	XDPProgram string
	Evidence   []string
}

// takeoverStep is one forward action of a transition.
type takeoverStep struct {
	Kind        string `json:"kind"`
	Mode        string `json:"mode,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Destructive bool   `json:"destructive,omitempty"`
}

// takeoverTransition is the immutable plan plus its execution record. It is
// persisted verbatim, including the rollback outcome, so an operator can see
// what happened even when the apply that produced it failed.
type takeoverTransition struct {
	SchemaVersion int    `json:"schema_version"`
	Interface     string `json:"interface"`
	PCIAddress    string `json:"pci_address,omitempty"`
	Netdev        string `json:"netdev,omitempty"`
	KernelDriver  string `json:"kernel_driver,omitempty"`
	VPPInterface  string `json:"vpp_interface,omitempty"`
	FromMode      string `json:"from_mode"`
	ToMode        string `json:"to_mode"`
	FromDriver    string `json:"from_driver,omitempty"`
	ToDriver      string `json:"to_driver,omitempty"`
	SWIfIndex     uint32 `json:"sw_if_index,omitempty"`
	QueueFDs      []int  `json:"queue_fds,omitempty"`
	XDPProgram    string `json:"xdp_program,omitempty"`
	Capability    string `json:"capability"`
	// LCPHostInterface is the Linux tap that shared the port (lylan-<netdev>).
	// Deleting the VPP interface deletes the pair, so the switch recreates it
	// after the new attachment exists and records the name it restored.
	LCPHostInterface string `json:"lcp_host_interface,omitempty"`
	// InterfaceAddresses/MTU are the interface-level L3 state VPP destroys with
	// the interface. They are captured before the teardown and replayed after.
	InterfaceAddresses []string `json:"interface_addresses,omitempty"`
	MTU                int      `json:"mtu,omitempty"`
	// Commands is the exact privileged command sequence the steps ran. It makes
	// a failed switch replayable without reading the control-plane logs.
	Commands []string `json:"commands,omitempty"`
	// NeedsRepair marks a switch that could not be undone and requires an
	// operator (or a later apply) to repair the port. It is never set on a
	// switch that converged or rolled back.
	NeedsRepair bool `json:"needs_repair,omitempty"`
	// LinkWasUp records the netdev carrier before the teardown. A switch that
	// brings a working link down is not applied: the port must be at least as
	// usable as it was.
	LinkWasUp      bool           `json:"link_was_up,omitempty"`
	Constraints    []string       `json:"constraints,omitempty"`
	Steps          []takeoverStep `json:"steps,omitempty"`
	Rollback       []takeoverStep `json:"rollback,omitempty"`
	RollbackOK     bool           `json:"rollback_guaranteed"`
	RollbackReason string         `json:"rollback_reason,omitempty"`
	Confirmed      bool           `json:"confirmed,omitempty"`
	// CapabilityUnprovenProbe marks the automatic decision to run a one-shot
	// transactional DPDK probe because no runtime proof exists yet. It replaces
	// the old operator `confirmed` consent: the appliance always makes this
	// decision itself.
	CapabilityUnprovenProbe bool `json:"capability_unproven_probe,omitempty"`
	// ProbeRequired/ProbeResult describe that transactional probe: pending,
	// verified or failed.
	ProbeRequired bool   `json:"probe_required,omitempty"`
	ProbeResult   string `json:"probe_result,omitempty"`
	// ConfirmationRequired is true when the operator must explicitly accept a
	// switch that may leave the port offline.
	ConfirmationRequired bool      `json:"confirmation_required,omitempty"`
	Status               string    `json:"status"`
	Reason               string    `json:"reason,omitempty"`
	RollbackResult       string    `json:"rollback_result,omitempty"`
	RollbackDetail       string    `json:"rollback_detail,omitempty"`
	Failures             []string  `json:"failures,omitempty"`
	StartedAt            time.Time `json:"started_at"`
	FinishedAt           time.Time `json:"finished_at,omitempty"`
}

const (
	takeoverStatusPlanned        = "planned"
	takeoverStatusNoChange       = "no_change"
	takeoverStatusKept           = "kept"
	takeoverStatusRejected       = "rejected"
	takeoverStatusApplied        = "applied"
	takeoverStatusFailed         = "failed"
	takeoverStatusRolledBack     = "rolled_back"
	takeoverStatusRollbackFailed = "rollback_failed"

	takeoverRollbackNotNeeded     = "not_needed"
	takeoverRollbackRestored      = "restored"
	takeoverRollbackRestoreFailed = "restore_failed"
	takeoverRollbackNotAttempted  = "not_attempted"
)

// planTakeoverTransition decides what a switch would do without touching the
// device. A current mode that differs from the desired mode is only changed
// when the operator asked for it explicitly; otherwise the transition records
// the deliberate decision to keep the current mode.
func planTakeoverTransition(snapshot takeoverSnapshot, desired string, explicit, confirmed bool, capability vpp.CapabilityAssessment, now time.Time) (takeoverTransition, error) {
	transition := takeoverTransition{
		SchemaVersion:  1,
		Interface:      strings.TrimSpace(snapshot.LinuxInterface),
		PCIAddress:     strings.TrimSpace(snapshot.PCIAddress),
		Netdev:         strings.TrimSpace(snapshot.Netdev),
		KernelDriver:   strings.TrimSpace(snapshot.KernelDriver),
		VPPInterface:   strings.TrimSpace(snapshot.VPPInterface),
		FromMode:       strings.TrimSpace(snapshot.CurrentMode),
		ToMode:         strings.TrimSpace(desired),
		FromDriver:     firstNonEmptyTrimmed(snapshot.BoundDriver, takeoverModeDriver(snapshot.CurrentMode), snapshot.KernelDriver),
		ToDriver:       firstNonEmptyTrimmed(takeoverModeDriver(desired), snapshot.KernelDriver),
		Capability:     string(capability.Status),
		Constraints:    append([]string(nil), capability.Constraints...),
		StartedAt:      now.UTC(),
		RollbackResult: takeoverRollbackNotAttempted,
	}
	if transition.Interface == "" {
		return takeoverTransition{}, &takeoverValidationError{Reason: "interface name is required"}
	}
	if transition.ToMode == "" {
		return takeoverTransition{}, &takeoverValidationError{Interface: transition.Interface, Reason: "a target takeover mode is required"}
	}
	if !takeoverModeValid(transition.ToMode) {
		return takeoverTransition{}, &takeoverValidationError{Interface: transition.Interface, Mode: transition.ToMode, Reason: "unknown takeover mode"}
	}
	// The static rule library is the final authority. A stale capability probe,
	// verification record or live residue cannot authorize a mode the device
	// family forbids (igb/igc/e1000e/r8169 AF_XDP). The rejected decision is
	// returned to the caller so it is journaled instead of retried.
	if driver := firstNonEmptyTrimmed(portKernelDriver(transition.Interface), snapshot.KernelDriver, recordedKernelDriver(snapshot.PCIAddress), snapshot.BoundDriver); !takeover.ModeAllowed(driver, portVendorDevice(transition.Interface), takeover.Mode(transition.ToMode)) {
		transition.Status = takeoverStatusRejected
		transition.RollbackResult = takeoverRollbackNotNeeded
		transition.RollbackOK = true
		transition.FinishedAt = now.UTC()
		transition.Reason = "the static rule library forbids " + transition.ToMode + " for driver " + firstNonEmptyTrimmed(driver, "unknown")
		recordStaleModeDropped(transition.Interface, transition.PCIAddress, driver, transition.ToMode, "switch_request", "the static rule library forbids this mode")
		return transition, &takeoverValidationError{Interface: transition.Interface, Mode: transition.ToMode, Reason: transition.Reason}
	}
	if transition.FromMode == transition.ToMode {
		transition.Status = takeoverStatusNoChange
		transition.RollbackResult = takeoverRollbackNotNeeded
		transition.RollbackOK = true
		transition.FinishedAt = now.UTC()
		transition.Reason = "the port already uses the requested mode"
		return transition, nil
	}
	if transition.FromMode != "" && !explicit {
		// This replaces the old implicit "VPP already owns the interface, so
		// skip binding" branch with an explicit, auditable decision.
		transition.Status = takeoverStatusKept
		transition.RollbackResult = takeoverRollbackNotNeeded
		transition.RollbackOK = true
		transition.FinishedAt = now.UTC()
		transition.Reason = "explicit mode switch was not requested; the current mode is retained"
		return transition, nil
	}
	if takeoverModeDestructive(transition.ToMode) && !capability.AllowsDestructiveTakeover() {
		// The takeover decision is automatic: there is no operator confirmation
		// and no capability gate that can block a first bind. Without a runtime
		// proof the engine performs exactly one transactional attempt and the
		// normal rollback/fallback applies if it fails.
		status := capability.Status
		if status == "" {
			status = vpp.CapabilityUnknown
		}
		if status == vpp.CapabilityUnknown {
			transition.CapabilityUnprovenProbe = true
			transition.ProbeRequired = true
			transition.ProbeResult = "pending"
		}
	}

	step := func(kind, mode, detail string) takeoverStep {
		return takeoverStep{Kind: kind, Mode: mode, Detail: detail, Destructive: takeoverModeDestructive(mode)}
	}
	// A switch between two non-destructive modes (AF_PACKET <-> AF_XDP) is a
	// live, verified replace: the bind step tears the old attachment down,
	// creates the requested one and reads it back, all without restarting VPP.
	// There is deliberately no separate detach step: the teardown has to be
	// recoverable in one place so a failed create can rebuild the old mode.
	nativeInPlace := transition.FromMode != "" &&
		!takeoverModeDestructive(transition.FromMode) &&
		!takeoverModeDestructive(transition.ToMode)
	steps := []takeoverStep{}
	rollback := []takeoverStep{}
	if transition.FromMode != "" && !nativeInPlace {
		steps = append(steps, step("detach", transition.FromMode, "detach the current "+takeoverModeLabel(transition.FromMode)+" attachment"))
		rollback = append(rollback, step("restore", transition.FromMode, "restore the previous "+takeoverModeLabel(transition.FromMode)+" attachment"))
	}
	steps = append(steps, step("bind", transition.ToMode, "bind or create the requested "+takeoverModeLabel(transition.ToMode)+" attachment"))
	if nativeInPlace {
		// Undoing the bind restores the previous attachment, which is the same
		// operation as rebuilding it: remove the target, create the old mode,
		// read it back.
		rollback = append([]takeoverStep{step("restore", transition.FromMode, "restore the previous "+takeoverModeLabel(transition.FromMode)+" attachment")}, rollback...)
	} else {
		rollback = append([]takeoverStep{step("remove", transition.ToMode, "detach the requested "+takeoverModeLabel(transition.ToMode)+" attachment")}, rollback...)
	}
	steps = append(steps, step("verify", transition.ToMode, "verify the requested attachment with a VPP readback"))
	steps = append(steps, step("record", transition.ToMode, "persist the transition and its rollback result"))

	transition.Steps = steps
	transition.Rollback = rollback
	transition.RollbackOK, transition.RollbackReason = takeoverRollbackGuarantee(snapshot, transition)
	if !transition.RollbackOK {
		// Informational only. The engine still attempts the switch and rolls
		// back on failure; a non-guaranteed rollback is recorded for diagnostics
		// instead of asking an operator to confirm.
		transition.ConfirmationRequired = true
	}
	transition.Confirmed = true
	transition.Status = takeoverStatusPlanned
	return transition, nil
}

// takeoverRollbackGuarantee answers the operator question "if this switch
// fails, will the port come back exactly as it is now?". It only says yes when
// the original mode is confirmed and every handle needed to rebuild it is
// known. Anything else must ask for explicit consent.
func takeoverRollbackGuarantee(snapshot takeoverSnapshot, transition takeoverTransition) (bool, string) {
	if transition.FromMode != "" && !snapshot.ModeConfirmed {
		return false, "the current mode was inferred from VPP state only and cannot be rebuilt deterministically"
	}
	if takeoverModeDestructive(transition.ToMode) && strings.TrimSpace(snapshot.KernelDriver) == "" {
		return false, "the original kernel driver is unknown, so a failed destructive bind cannot be rolled back"
	}
	if transition.FromMode == "" {
		if strings.TrimSpace(snapshot.KernelDriver) == "" {
			return false, "the original kernel driver is unknown, so the port cannot be guaranteed to return to Linux"
		}
		return true, ""
	}
	if takeoverModeDestructive(transition.FromMode) && strings.TrimSpace(snapshot.KernelDriver) == "" {
		return false, "the original kernel driver is unknown, so the DPDK binding cannot be restored"
	}
	if takeoverModeDestructive(transition.ToMode) && strings.TrimSpace(snapshot.PCIAddress) == "" {
		return false, "the PCI address is unknown, so the destructive bind cannot be reversed"
	}
	if strings.TrimSpace(snapshot.VPPInterface) == "" {
		return false, "the VPP interface identity is unknown, so the previous attachment cannot be recreated"
	}
	return true, ""
}

// takeoverStepResult carries the runtime handles a step observed. Only real
// readbacks fill them; a fake executor in tests can fill them directly.
type takeoverStepResult struct {
	SWIfIndex  uint32
	QueueFDs   []int
	XDPProgram string
	Output     string
}

// takeoverExecutor performs the side effects of a transition. Keeping it behind
// an interface is what makes the state machine testable without VPP or DPDK.
type takeoverExecutor interface {
	Execute(ctx context.Context, transition *takeoverTransition, step takeoverStep) (takeoverStepResult, error)
	Rollback(ctx context.Context, transition *takeoverTransition, step takeoverStep) error
	// Recover returns VPP to a usable state after a rollback. Rollback is not
	// complete while VPP is stopped: the next apply must find a running
	// dataplane, not a dead process.
	Recover(ctx context.Context, transition *takeoverTransition) error
}

// executeTakeoverTransition runs the forward steps and rolls back in reverse
// dependency order on the first failure. The transition always records what was
// attempted, what was rolled back and whether the rollback succeeded.
func executeTakeoverTransition(ctx context.Context, transition *takeoverTransition, executor takeoverExecutor) error {
	if transition == nil || executor == nil {
		return errors.New("takeover transition and executor are required")
	}
	if transition.Status == takeoverStatusNoChange || transition.Status == takeoverStatusKept {
		return nil
	}
	transition.Status = takeoverStatusPlanned
	applied := make([]takeoverStep, 0, len(transition.Steps))
	var stepErrs []error
	for _, step := range transition.Steps {
		result, err := executor.Execute(ctx, transition, step)
		if err != nil {
			transition.Status = takeoverStatusFailed
			transition.Failures = append(transition.Failures, step.Kind+": "+err.Error())
			if transition.ProbeRequired {
				transition.ProbeResult = "failed"
			}
			// A native bind is one teardown+build operation: it may already have
			// deleted the previous attachment when the create failed, so it has
			// to be undone even though it never reported success. A destructive
			// bind is undone by the detach/restore steps that came before it.
			if step.Kind == "bind" && !takeoverModeDestructive(step.Mode) {
				applied = append(applied, step)
			}
			rollbackErr := rollbackTakeoverTransition(ctx, transition, executor, applied)
			stepErrs = append(stepErrs, fmt.Errorf("%s %s: %w", step.Kind, takeoverModeLabel(step.Mode), err))
			if rollbackErr != nil {
				stepErrs = append(stepErrs, rollbackErr)
			}
			transition.FinishedAt = time.Now().UTC()
			return errors.Join(stepErrs...)
		}
		mergeTakeoverStepResult(transition, step, result)
		applied = append(applied, step)
	}
	transition.Status = takeoverStatusApplied
	transition.RollbackResult = takeoverRollbackNotNeeded
	if transition.ProbeRequired {
		transition.ProbeResult = "verified"
	}
	transition.FinishedAt = time.Now().UTC()
	return nil
}

func mergeTakeoverStepResult(transition *takeoverTransition, step takeoverStep, result takeoverStepResult) {
	if step.Kind != "verify" && step.Kind != "record" {
		return
	}
	if result.SWIfIndex != 0 {
		transition.SWIfIndex = result.SWIfIndex
	}
	if len(result.QueueFDs) > 0 {
		transition.QueueFDs = append([]int(nil), result.QueueFDs...)
	}
	if value := strings.TrimSpace(result.XDPProgram); value != "" {
		transition.XDPProgram = value
	}
}

func rollbackTakeoverTransition(ctx context.Context, transition *takeoverTransition, executor takeoverExecutor, applied []takeoverStep) error {
	transition.RollbackResult = takeoverRollbackNotAttempted
	failures := []string{}
	for index := len(applied) - 1; index >= 0; index-- {
		step := applied[index]
		if step.Kind == "verify" || step.Kind == "record" {
			continue
		}
		if err := executor.Rollback(ctx, transition, step); err != nil {
			failures = append(failures, step.Kind+": "+err.Error())
		}
	}
	// A rollback that leaves VPP stopped is not a rollback: make the process
	// usable again before reporting the outcome. Recover is attempted even
	// when an undo failed, so a partial restore still gets a dataplane.
	recoverErr := executor.Recover(ctx, transition)
	if recoverErr != nil {
		failures = append(failures, "recover: "+recoverErr.Error())
	}
	if len(failures) == 0 {
		transition.RollbackResult = takeoverRollbackRestored
		transition.RollbackDetail = "the previous attachment was restored and VPP is running"
		return nil
	}
	transition.RollbackResult = takeoverRollbackRestoreFailed
	transition.Status = takeoverStatusRollbackFailed
	transition.RollbackDetail = strings.Join(failures, "; ")
	transition.Failures = append(transition.Failures, failures...)
	return fmt.Errorf("takeover rollback failed: %s", transition.RollbackDetail)
}

// takeoverKeptTransition records the deliberate decision to keep the current
// mode when no switch was requested.
func takeoverKeptTransition(snapshot takeoverSnapshot, now time.Time) takeoverTransition {
	return takeoverTransition{
		SchemaVersion:  1,
		Interface:      snapshot.LinuxInterface,
		PCIAddress:     snapshot.PCIAddress,
		Netdev:         snapshot.Netdev,
		KernelDriver:   snapshot.KernelDriver,
		VPPInterface:   snapshot.VPPInterface,
		FromMode:       snapshot.CurrentMode,
		ToMode:         snapshot.CurrentMode,
		FromDriver:     firstNonEmptyTrimmed(snapshot.BoundDriver, takeoverModeDriver(snapshot.CurrentMode), snapshot.KernelDriver),
		ToDriver:       firstNonEmptyTrimmed(snapshot.BoundDriver, takeoverModeDriver(snapshot.CurrentMode), snapshot.KernelDriver),
		SWIfIndex:      snapshot.SWIfIndex,
		QueueFDs:       append([]int(nil), snapshot.QueueFDs...),
		XDPProgram:     snapshot.XDPProgram,
		RollbackOK:     true,
		Status:         takeoverStatusKept,
		Reason:         "explicit mode switch was not requested; the current mode is retained",
		RollbackResult: takeoverRollbackNotNeeded,
		StartedAt:      now.UTC(),
		FinishedAt:     now.UTC(),
	}
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// takeoverStatePath is the journal the state machine writes. It is deliberately
// separate from ownership.json (recorded intent) and active.json (committed
// generation): the journal records decisions and their rollback outcome.
func takeoverStatePath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_TAKEOVER_STATE")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "takeover-state.json")
}

type takeoverJournal struct {
	SchemaVersion int                  `json:"schema_version"`
	UpdatedAt     time.Time            `json:"updated_at,omitempty"`
	Last          *takeoverTransition  `json:"last,omitempty"`
	History       []takeoverTransition `json:"history,omitempty"`
}

const takeoverJournalLimit = 32

var takeoverJournalMu sync.Mutex

func loadTakeoverJournal(path string) (takeoverJournal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return takeoverJournal{}, nil
	}
	if err != nil {
		return takeoverJournal{}, err
	}
	var journal takeoverJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return takeoverJournal{}, err
	}
	return journal, nil
}

func saveTakeoverJournal(path string, journal takeoverJournal) error {
	journal.SchemaVersion = 1
	journal.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// recordTakeoverTransition appends a decision to the journal. It runs under a
// process-wide lock so two concurrent applies cannot interleave history.
func recordTakeoverTransition(transition takeoverTransition) error {
	if strings.TrimSpace(transition.Interface) == "" {
		return nil
	}
	takeoverJournalMu.Lock()
	defer takeoverJournalMu.Unlock()
	path := takeoverStatePath()
	journal, err := loadTakeoverJournal(path)
	if err != nil {
		log.Printf("takeover journal is unreadable; starting a new one: %v", err)
		journal = takeoverJournal{}
	}
	record := transition
	journal.Last = &record
	journal.History = append([]takeoverTransition{record}, journal.History...)
	if len(journal.History) > takeoverJournalLimit {
		journal.History = journal.History[:takeoverJournalLimit]
	}
	return saveTakeoverJournal(path, journal)
}

// lastTakeoverTransition returns the most recent decision for a port.
func lastTakeoverTransition(name string) (takeoverTransition, bool) {
	journal, err := loadTakeoverJournal(takeoverStatePath())
	if err != nil {
		return takeoverTransition{}, false
	}
	name = strings.TrimSpace(name)
	for _, transition := range journal.History {
		if strings.TrimSpace(transition.Interface) == name {
			return transition, true
		}
	}
	if journal.Last != nil && strings.TrimSpace(journal.Last.Interface) == name {
		return *journal.Last, true
	}
	return takeoverTransition{}, false
}

func takeoverTransitionSummary(transition takeoverTransition) map[string]any {
	summary := map[string]any{
		"from_mode":       nullableString(transition.FromMode),
		"to_mode":         nullableString(transition.ToMode),
		"status":          transition.Status,
		"rollback_result": nullableString(transition.RollbackResult),
		"recorded_at":     transition.FinishedAt,
	}
	if transition.RollbackReason != "" {
		summary["rollback_reason"] = transition.RollbackReason
	}
	if !transition.RollbackOK {
		summary["rollback_guaranteed"] = false
	}
	if transition.CapabilityUnprovenProbe {
		summary["capability_unproven_probe"] = true
	}
	if transition.ProbeResult != "" {
		summary["probe_result"] = transition.ProbeResult
	}
	if transition.NeedsRepair {
		summary["needs_repair"] = true
	}
	if transition.LCPHostInterface != "" {
		summary["lcp_host_interface"] = transition.LCPHostInterface
	}
	if len(transition.Commands) > 0 {
		summary["commands"] = transition.Commands
	}
	return summary
}

// parseVPPInterfaceIndex reads the interface index from `show interface <name>`.
func parseVPPInterfaceIndex(output, name string) uint32 {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != strings.TrimSpace(name) {
			continue
		}
		index, err := strconv.ParseUint(fields[1], 10, 32)
		if err == nil {
			return uint32(index)
		}
	}
	return 0
}

// parseAFXDPQueueFDs extracts queue file descriptors from `show af_xdp`.
// VPP prints one row per queue; the fd is the last numeric field. Parsing is
// intentionally tolerant: an unrecognised line is ignored, never fatal.
func parseAFXDPQueueFDs(output string) []int {
	fds := []int{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(line)
		if !strings.Contains(line, "fd") && !strings.Contains(line, "queue") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		last := fields[len(fields)-1]
		value, err := strconv.Atoi(last)
		if err != nil || value < 0 {
			continue
		}
		fds = append(fds, value)
	}
	return fds
}

// parseXDPProgramID extracts the attached XDP program id from `ip -details
// link show`. An unloaded netdev yields an empty string.
func parseXDPProgramID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != "prog/xdp" || index+2 >= len(fields) || fields[index+1] != "id" {
				continue
			}
			if _, err := strconv.Atoi(fields[index+2]); err != nil {
				continue
			}
			return "id:" + fields[index+2]
		}
	}
	return ""
}
