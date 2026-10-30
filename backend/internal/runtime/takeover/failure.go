package takeover

import "strings"

// FailureClass is the machine-readable reason a takeover attempt failed. It is
// recorded next to the attempt so the next step is a policy decision, not an
// error-string guess.
type FailureClass string

const (
	// FailureUnsupported: the device/driver cannot do the requested mode.
	FailureUnsupported FailureClass = "UNSUPPORTED"
	// FailureEnvironment: EAL, hugepages, permissions, IOMMU, VPP startup.
	FailureEnvironment FailureClass = "ENVIRONMENT"
	// FailureBusy: the device or its address is still held by someone else.
	FailureBusy FailureClass = "BUSY"
	// FailureConfig: startup.conf/PMD/plugin configuration.
	FailureConfig FailureClass = "CONFIG"
	// FailureRX: no receive path.
	FailureRX FailureClass = "RX_FAILED"
	// FailureTX: no transmit path.
	FailureTX FailureClass = "TX_FAILED"
	// FailureForward: attached but not forwarding (carrier, LCP, counters).
	FailureForward FailureClass = "FORWARD_FAILED"
	// FailureCleanup: the failed attempt could not be undone. This is the only
	// class that must stop the whole plan and mark the port needs_repair.
	FailureCleanup FailureClass = "CLEANUP_FAILED"
	// FailureUnknown: anything that does not classify; treated as a normal
	// failure and the plan advances.
	FailureUnknown FailureClass = "UNKNOWN"
)

// Classify maps a stage and an error to a failure class. The stage is the
// takeover phase ("bind", "start", "verify", "cleanup"); the error text is only
// used to refine the class, never to decide safety.
func Classify(stage string, err error) FailureClass {
	stage = strings.ToLower(strings.TrimSpace(stage))
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	switch stage {
	case "cleanup", "release", "rebind", "restore", "rollback":
		return FailureCleanup
	}
	for _, token := range []string{"cleanup", "release", "rebind", "unbind", "restore the previous", "could not be restored", "not back on"} {
		if strings.Contains(text, token) {
			return FailureCleanup
		}
	}
	for _, token := range []string{"device or resource busy", "ebusy", "address already in use", "busy"} {
		if strings.Contains(text, token) {
			return FailureBusy
		}
	}
	for _, token := range []string{"hugepage", "eal", "iommu", "permission", "operation not permitted", "connection refused", "vpp did not become ready", "vpp is not reachable", "no such file"} {
		if strings.Contains(text, token) {
			return FailureEnvironment
		}
	}
	for _, token := range []string{"startup.conf", "invalid pmd", "pmd", "plugin", "configuration", "config"} {
		if strings.Contains(text, token) {
			return FailureConfig
		}
	}
	switch stage {
	case "rx":
		return FailureRX
	case "tx":
		return FailureTX
	case "verify", "forward":
		return FailureForward
	}
	for _, token := range []string{"rx", "receive"} {
		if strings.Contains(text, token) {
			return FailureRX
		}
	}
	for _, token := range []string{"tx poll", "transmit", " tx "} {
		if strings.Contains(text, token) {
			return FailureTX
		}
	}
	for _, token := range []string{"carrier", "forward", "link down", "no traffic", "counter"} {
		if strings.Contains(text, token) {
			return FailureForward
		}
	}
	for _, token := range []string{"not supported", "unsupported", "no pmd", "does not support", "unknown device", "not found"} {
		if strings.Contains(text, token) {
			return FailureUnsupported
		}
	}
	return FailureUnknown
}

// Next is the decision after one failed attempt.
type Next struct {
	// Stop ends the plan. When NeedsRepair is also set the port must not be
	// retried automatically until a human/apply repairs it.
	Stop        bool
	NeedsRepair bool
	// RetrySame asks for a later retry of the same mode (transient, e.g. BUSY).
	RetrySame bool
	// NextMode is the mode to attempt next. Empty means the plan is exhausted.
	NextMode Mode
	// Reason is the operator-facing explanation recorded in the journal.
	Reason string
}

// Advance decides what happens after `failed` failed with class `class`.
//
// CLEANUP_FAILED is fatal: the previous state could not be restored, so trying
// another destructive mode would stack an unrepairable port on top of a broken
// one. Every other class advances to the next candidate; the final candidate is
// AF_PACKET, so a plan can only stop early for cleanup/BUSY.
func Advance(plan Plan, failed Mode, class FailureClass) Next {
	if class == FailureCleanup {
		return Next{
			Stop:        true,
			NeedsRepair: true,
			Reason:      "cleanup failed after " + string(failed) + "; the port is marked needs_repair and no further mode is attempted",
		}
	}
	if class == FailureBusy {
		// BUSY means a previous owner still holds the netdev, a queue or the
		// XDP dispatcher. Replaying the same mode immediately is exactly the
		// loop that produced duplicate lyroute-* interfaces and the VPP
		// SIGABRT on the igc AF_XDP TX path. Stop, mark the port for repair and
		// let a later apply/operator clean the residue before another attempt.
		return Next{
			Stop:        true,
			NeedsRepair: true,
			Reason:      "the device is busy; the plan stops instead of replaying a mode that cannot release the port",
		}
	}
	for index, candidate := range plan.Candidates {
		if candidate.Mode != failed {
			continue
		}
		if index+1 < len(plan.Candidates) {
			return Next{
				NextMode: plan.Candidates[index+1].Mode,
				Reason:   string(failed) + " failed (" + string(class) + "); falling back to " + string(plan.Candidates[index+1].Mode),
			}
		}
	}
	return Next{Stop: true, Reason: "the takeover plan is exhausted after " + string(failed)}
}
