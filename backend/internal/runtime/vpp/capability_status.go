package vpp

import (
	"fmt"
	"strings"
)

// CapabilityStatus is the four-state result of a dataplane capability probe.
//
// The product must never collapse "we could not find out" into "supported".
// Unknown is a first-class answer: every consumer has to take the conservative
// path (keep what already works, never perform a destructive takeover) until a
// probe produces evidence. "Supported with constraints" is the honest state for
// paths that work but carry a documented operational limit (UIO without an
// IOMMU, copy-mode AF_XDP, a minimum-size hugepage pool).
type CapabilityStatus string

const (
	CapabilitySupported                CapabilityStatus = "supported"
	CapabilitySupportedWithConstraints CapabilityStatus = "supported_with_constraints"
	CapabilityUnsupported              CapabilityStatus = "unsupported"
	CapabilityUnknown                  CapabilityStatus = "unknown"
)

// NormalizeCapabilityStatus maps arbitrary report input onto the four states.
// An empty or unrecognised value is unknown, never supported.
func NormalizeCapabilityStatus(value string) CapabilityStatus {
	switch CapabilityStatus(strings.ToLower(strings.TrimSpace(value))) {
	case CapabilitySupported:
		return CapabilitySupported
	case CapabilitySupportedWithConstraints:
		return CapabilitySupportedWithConstraints
	case CapabilityUnsupported:
		return CapabilityUnsupported
	default:
		return CapabilityUnknown
	}
}

// Known reports whether the probe produced usable evidence.
func (status CapabilityStatus) Known() bool {
	switch status {
	case CapabilitySupported, CapabilitySupportedWithConstraints, CapabilityUnsupported:
		return true
	}
	return false
}

// Conservative reports whether the conservative path is mandatory. Unknown is
// conservative; an empty value is treated the same way so a missing field can
// never be read as approval.
func (status CapabilityStatus) Conservative() bool {
	return !status.Known()
}

// DestructiveAllowed reports whether this status may authorise a takeover that
// removes the kernel netdev (VFIO/UIO). Only positive evidence does.
func (status CapabilityStatus) DestructiveAllowed() bool {
	return status == CapabilitySupported || status == CapabilitySupportedWithConstraints
}

// CapabilityFact is a tri-state observation. Known=false means the probe could
// not establish the fact; callers must treat it as unknown, not as false.
type CapabilityFact struct {
	Known  bool   `json:"known"`
	Value  bool   `json:"value"`
	Detail string `json:"detail,omitempty"`
}

// KnownFact records an observed boolean.
func KnownFact(value bool) CapabilityFact {
	return CapabilityFact{Known: true, Value: value}
}

// UnknownFact records that a fact could not be observed.
func UnknownFact(detail string) CapabilityFact {
	return CapabilityFact{Detail: strings.TrimSpace(detail)}
}

// CapabilityEvidence is one named observation behind an assessment. Facts that
// were not observed stay Known=false: a committed mode must not be presented as
// proof that some other transport is available.
type CapabilityEvidence struct {
	Name string `json:"name"`
	CapabilityFact
}

// CapabilityAssessment is the operator-facing result for one hook/mode.
type CapabilityAssessment struct {
	Hook        NativeHook           `json:"hook,omitempty"`
	Mode        NativeMode           `json:"mode,omitempty"`
	Status      CapabilityStatus     `json:"status"`
	Constraints []string             `json:"constraints,omitempty"`
	Reason      string               `json:"reason,omitempty"`
	Source      string               `json:"source,omitempty"`
	Evidence    []CapabilityEvidence `json:"evidence,omitempty"`
}

// Conservative reports whether the assessment requires the conservative path.
func (assessment CapabilityAssessment) Conservative() bool {
	return assessment.Status.Conservative()
}

// AllowsDestructiveTakeover reports whether this assessment may authorise a
// VFIO/UIO bind.
func (assessment CapabilityAssessment) AllowsDestructiveTakeover() bool {
	return assessment.Status.DestructiveAllowed()
}

// DPDKTakeoverEvidence is everything the probe can observe about one DPDK
// candidate. Facts are tri-state so a missing sysfs/vppctl answer is unknown.
type DPDKTakeoverEvidence struct {
	Transport       string
	VFIOAvailable   CapabilityFact
	UIOPCIAvailable CapabilityFact
	DPDKPlugin      CapabilityFact
	Hugepages       CapabilityFact
	HugepagesCount  int
	PCIAddressSafe  CapabilityFact
	IOMMUProtected  CapabilityFact
	IOMMUGroup      string
	// Queues is the RX queue count observed for the candidate. Zero means the
	// probe could not count them and is reported as a constraint, not a fact.
	Queues int
}

// AssessDPDKTakeover classifies one destructive DPDK candidate.
//
// Order matters: missing evidence wins over a negative fact only when the
// missing fact is required, because "unknown" must not silently become either
// "works" or "broken".
func AssessDPDKTakeover(mode NativeMode, evidence DPDKTakeoverEvidence) CapabilityAssessment {
	assessment := CapabilityAssessment{Hook: NativeHookDPDK, Mode: mode, Source: "capability_probe"}
	assessment.Evidence = dpdkEvidence(evidence)
	if mode != NativeModeDPDKVFIO && mode != NativeModeDPDKUIO {
		assessment.Status = CapabilityUnsupported
		assessment.Reason = "DPDK transport must be vfio_pci or uio_pci_generic"
		return assessment
	}
	if transport := strings.TrimSpace(evidence.Transport); transport != "" && dpdkTransportForMode(mode) != transport {
		assessment.Status = CapabilityUnsupported
		assessment.Reason = fmt.Sprintf("transport %q does not match binding mode %q", transport, mode)
		return assessment
	}

	required := []struct {
		name string
		fact CapabilityFact
	}{
		{"DPDK plugin", evidence.DPDKPlugin},
		{"hugepages", evidence.Hugepages},
		{"PCI address", evidence.PCIAddressSafe},
	}
	if mode == NativeModeDPDKVFIO {
		required = append(required,
			struct {
				name string
				fact CapabilityFact
			}{"vfio-pci", evidence.VFIOAvailable},
			struct {
				name string
				fact CapabilityFact
			}{"IOMMU protection", evidence.IOMMUProtected},
		)
	} else {
		required = append(required, struct {
			name string
			fact CapabilityFact
		}{"uio_pci_generic", evidence.UIOPCIAvailable})
	}

	constraints := []string{}
	unknown := []string{}
	for _, item := range required {
		if !item.fact.Known {
			detail := strings.TrimSpace(item.fact.Detail)
			if detail == "" {
				detail = "not observed"
			}
			unknown = append(unknown, item.name+" is unknown: "+detail)
			continue
		}
		if !item.fact.Value {
			assessment.Status = CapabilityUnsupported
			assessment.Reason = item.name + " is unavailable"
			return assessment
		}
	}
	if len(unknown) > 0 {
		assessment.Status = CapabilityUnknown
		assessment.Constraints = unknown
		assessment.Reason = "capability probe is incomplete; the conservative path is required"
		return assessment
	}

	if mode == NativeModeDPDKVFIO {
		if !decimalIdentifierSafe(evidence.IOMMUGroup) {
			assessment.Status = CapabilityUnsupported
			assessment.Reason = "IOMMU group is not a numeric identifier"
			return assessment
		}
	}
	if mode == NativeModeDPDKUIO {
		constraints = append(constraints, "no IOMMU protection (uio_pci_generic)")
	}
	if evidence.Queues > 0 && evidence.Queues < 2 {
		constraints = append(constraints, "single RX queue")
	}
	if evidence.HugepagesCount > 0 && evidence.HugepagesCount < 2*minimumHugepageBaseline() {
		constraints = append(constraints, fmt.Sprintf("hugepage pool at the minimum (%d pages)", evidence.HugepagesCount))
	}
	assessment.Status = CapabilitySupported
	if len(constraints) > 0 {
		assessment.Status = CapabilitySupportedWithConstraints
		assessment.Constraints = constraints
	}
	return assessment
}

// minimumHugepageBaseline mirrors the established 2 MiB hugepage baseline used
// by the httpapi probe gate. It lives here so both packages report the same
// "minimum pool" constraint.
func minimumHugepageBaseline() int { return 256 }

// NativeTakeoverEvidence describes a non-destructive AF_XDP/RDMA candidate.
type NativeTakeoverEvidence struct {
	DriverSupported CapabilityFact
	RuntimeVerified CapabilityFact
	ZeroCopy        CapabilityFact
	XDPProgram      CapabilityFact
	Queues          int
}

// AssessNativeTakeover classifies one VPP-native candidate. Unknown runtime
// evidence is never promoted to a support claim: the port stays on its current
// path until the probe succeeds.
func AssessNativeTakeover(mode NativeMode, evidence NativeTakeoverEvidence) CapabilityAssessment {
	assessment := CapabilityAssessment{Hook: NativeHookAFXDP, Mode: mode, Source: "capability_probe"}
	if mode == NativeModeRDMADV {
		assessment.Hook = NativeHookRDMA
	}
	assessment.Evidence = []CapabilityEvidence{
		{Name: "driver", CapabilityFact: evidence.DriverSupported},
		{Name: "runtime_probe", CapabilityFact: evidence.RuntimeVerified},
		{Name: "zero_copy", CapabilityFact: evidence.ZeroCopy},
		{Name: "xdp_program", CapabilityFact: evidence.XDPProgram},
	}
	unknown := []string{}
	for _, item := range []struct {
		name string
		fact CapabilityFact
	}{
		{"kernel driver", evidence.DriverSupported},
		{"runtime probe", evidence.RuntimeVerified},
	} {
		if !item.fact.Known {
			detail := strings.TrimSpace(item.fact.Detail)
			if detail == "" {
				detail = "not observed"
			}
			unknown = append(unknown, item.name+" is unknown: "+detail)
			continue
		}
		if !item.fact.Value {
			assessment.Status = CapabilityUnsupported
			assessment.Reason = item.name + " is not supported"
			return assessment
		}
	}
	if len(unknown) > 0 {
		assessment.Status = CapabilityUnknown
		assessment.Constraints = unknown
		assessment.Reason = "capability probe is incomplete; the conservative path is required"
		return assessment
	}
	constraints := []string{}
	if evidence.Queues > 0 && evidence.Queues < 2 {
		constraints = append(constraints, "single RX queue")
	}
	if evidence.ZeroCopy.Known && !evidence.ZeroCopy.Value {
		constraints = append(constraints, "copy mode (zero-copy is unavailable)")
	} else if !evidence.ZeroCopy.Known {
		constraints = append(constraints, "zero-copy support is not proven")
	}
	if evidence.XDPProgram.Known && !evidence.XDPProgram.Value {
		constraints = append(constraints, "XDP program is not loaded")
	}
	assessment.Status = CapabilitySupported
	if len(constraints) > 0 {
		assessment.Status = CapabilitySupportedWithConstraints
		assessment.Constraints = constraints
	}
	return assessment
}

// AssessCapabilityProof derives a four-state assessment from a persisted
// capability proof. Older reports carry booleans only: a proof that was not
// runtime verified is unknown, and a verified proof with a failed required fact
// is unsupported.
func AssessCapabilityProof(proof CapabilityProof) CapabilityAssessment {
	if proof.CapabilityStatus != "" {
		return CapabilityAssessment{
			Hook:        proof.Hook,
			Mode:        proof.Mode,
			Status:      NormalizeCapabilityStatus(string(proof.CapabilityStatus)),
			Constraints: append([]string(nil), proof.CapabilityConstraints...),
			Reason:      strings.TrimSpace(proof.CapabilityReason),
			Source:      "capability_report",
		}
	}
	if !proof.RuntimeVerified || proof.ObservedAt.IsZero() {
		return CapabilityAssessment{
			Hook:   proof.Hook,
			Mode:   proof.Mode,
			Status: CapabilityUnknown,
			Reason: "runtime capability proof is missing",
			Source: "capability_report",
		}
	}
	if proofTier(proof) == DataplaneTierDPDK {
		evidence := DPDKTakeoverEvidence{
			Transport:       proofTransport(proof),
			VFIOAvailable:   KnownFact(proof.VFIOAvailable),
			UIOPCIAvailable: KnownFact(proof.UIOPCIAvailable),
			DPDKPlugin:      KnownFact(proof.DPDKPluginAvailable),
			Hugepages:       KnownFact(proof.HugepagesAvailable),
			HugepagesCount:  0,
			PCIAddressSafe:  KnownFact(pciAddressSafe(proof.PCIAddress)),
			IOMMUProtected:  KnownFact(proof.IOMMUProtected),
			IOMMUGroup:      proof.IOMMUGroup,
		}
		return AssessDPDKTakeover(proof.Mode, evidence)
	}
	driverSupported, driverKnown := NativeProbeDriverVerdict(proof.Hook, proof.KernelDriver)
	driverFact := UnknownFact("driver verdict is not recorded")
	if driverKnown {
		driverFact = KnownFact(driverSupported)
	}
	return AssessNativeTakeover(proof.Mode, NativeTakeoverEvidence{
		DriverSupported: driverFact,
		RuntimeVerified: KnownFact(true),
		ZeroCopy:        KnownFact(proof.Mode == NativeModeZeroCopy),
		XDPProgram:      KnownFact(true),
	})
}

// dpdkEvidence lists the observations the DPDK classifier used. Unknown facts
// are kept as unknown so a consumer cannot mistake a missing probe for support.
func dpdkEvidence(evidence DPDKTakeoverEvidence) []CapabilityEvidence {
	return []CapabilityEvidence{
		{Name: "vfio_pci", CapabilityFact: evidence.VFIOAvailable},
		{Name: "uio_pci_generic", CapabilityFact: evidence.UIOPCIAvailable},
		{Name: "dpdk_plugin", CapabilityFact: evidence.DPDKPlugin},
		{Name: "hugepages", CapabilityFact: evidence.Hugepages},
		{Name: "pci_address", CapabilityFact: evidence.PCIAddressSafe},
		{Name: "iommu_protected", CapabilityFact: evidence.IOMMUProtected},
	}
}
