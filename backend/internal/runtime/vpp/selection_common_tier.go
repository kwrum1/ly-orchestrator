package vpp

import (
	"sort"
	"strings"
	"time"
)

func selectCommonDataplanePath(request NativePathRequest) (NativePath, error) {
	management := strings.TrimSpace(request.ManagementInterface)
	results := append([]PrerequisiteResult(nil), request.ReportPrerequisites...)
	results = append(results,
		prerequisite("management_identified", "", management != "", "management interface is required"),
		prerequisite("data_assignment_present", "", len(request.Assignments) > 0, "at least one explicit data interface assignment is required"),
	)

	assignments := append([]NativeAssignment(nil), request.Assignments...)
	sort.Slice(assignments, func(i, j int) bool {
		return strings.TrimSpace(assignments[i].LinuxInterface) < strings.TrimSpace(assignments[j].LinuxInterface)
	})

	evaluations := make([]CandidateEvaluation, 0)
	selected := make([]CapabilityProof, 0, len(assignments))
	afRequired := make([]string, 0)
	seen := make(map[string]struct{}, len(assignments))
	hardFailures := false
	for _, assignment := range assignments {
		name := strings.TrimSpace(assignment.LinuxInterface)
		_, duplicate := seen[name]
		seen[name] = struct{}{}
		results = append(results,
			prerequisite("interface_assigned", name, name != "", "data interface name is required"),
			prerequisite("interface_name_safe", name, interfaceNameSafe(name), "data interface name contains unsupported characters"),
			prerequisite("assignment_unique", name, !duplicate, "data interface assignment is duplicated"),
			prerequisite("explicit_assignment", name, assignment.Explicit, "automatic interface assignment is forbidden"),
			prerequisite("management_excluded", name, management != "" && (request.ManagementShared || name != management), "management interface is permanently excluded"),
		)

		var best *CapabilityProof
		afCandidateSeen := false
		afCandidateOnlyNeedsConsent := false
		afConsent := request.AFPacketConsent || assignment.AFPacketConsent
		for index := range assignment.Candidates {
			proof := assignment.Candidates[index]
			tier := proofTier(proof)
			reasons := candidateRejectionReasons(proof, request.Now, tier, request.AcceptanceAFPacket, afConsent)
			assessment := AssessCapabilityProof(proof)
			evaluation := CandidateEvaluation{LinuxInterface: name, Tier: tier, Hook: proof.Hook, Mode: proof.Mode, PerformanceScore: proof.PerformanceScore, Eligible: len(reasons) == 0, Capability: assessment.Status, Constraints: assessment.Constraints, Reasons: reasons}
			evaluations = append(evaluations, evaluation)
			if isAFPacketCompatibilityProof(proof) {
				afCandidateSeen = true
				if len(reasons) == 1 && reasons[0] == afConsentRequiredReason {
					afCandidateOnlyNeedsConsent = true
				}
			}
			if !evaluation.Eligible {
				continue
			}
			if best == nil || candidatePreferred(proof, *best) {
				candidate := proof
				best = &candidate
			}
		}
		if best == nil {
			if afCandidateSeen && afCandidateOnlyNeedsConsent {
				afRequired = append(afRequired, name)
			} else {
				hardFailures = true
				results = append(results, prerequisite("runtime_capability_proof", name, false, "no usable high-performance or consented compatibility path was proven for this interface"))
			}
			continue
		}
		selected = append(selected, *best)
		results = append(results, prerequisite("interface_realized", name, true, ""))
	}

	if len(afRequired) > 0 {
		return NativePath{}, &AFFallbackRequiredError{Interfaces: afRequired, Reason: AFPacketDegradedReason}
	}
	for _, result := range results {
		if !result.Passed {
			return NativePath{}, &DataplaneLockedError{Prerequisites: results, Candidates: evaluations}
		}
	}
	if hardFailures || len(selected) == 0 {
		return NativePath{}, &DataplaneLockedError{Prerequisites: results, Candidates: evaluations}
	}

	tier := DataplaneTierNative
	degraded := false
	degradations := []Degradation{smartQoSRetiredDegradation()}
	for _, proof := range selected {
		if proofTier(proof) == DataplaneTierDPDK {
			tier = DataplaneTierDPDK
		}
		if isAFPacketCompatibilityProof(proof) {
			degraded = true
		}
	}
	if degraded {
		degradations = append(degradations, Degradation{Name: "af_packet_fallback", Reason: AFPacketDegradedReason, Impact: "performance"})
	}
	attachments := make([]NativeAttachment, 0, len(selected))
	for index, proof := range selected {
		vppInterface, hardwareInterface := nativeAttachmentVPPNames(proof, strings.TrimSpace(assignments[index].LinuxInterface))
		attachments = append(attachments, proveNativeAttachment(NativeAttachment{
			LinuxInterface:       strings.TrimSpace(assignments[index].LinuxInterface),
			VPPInterface:         vppInterface,
			HardwareVPPInterface: hardwareInterface,
			MACAddress:           strings.TrimSpace(proof.MACAddress),
			Tier:                 proofTier(proof),
			Hook:                 proof.Hook,
			Mode:                 proof.Mode,
			PCIAddress:           strings.TrimSpace(proof.PCIAddress),
			KernelDriver:         strings.TrimSpace(proof.KernelDriver),
			IOMMUGroup:           strings.TrimSpace(proof.IOMMUGroup),
			Verification:         attachmentVerification(proof),
		}))
	}
	results = append(results, prerequisite("common_dataplane_tier", "", true, ""))
	return NativePath{Tier: tier, Degraded: degraded, SmartQoS: false, Attachments: attachments, Prerequisites: results, Candidates: evaluations, Degradations: degradations}, nil
}

// candidatePreferred implements the product's ordered fallback. Breadth wins:
// an interface uses the first usable transport in this order regardless of a
// lower-ranked candidate's benchmark score.
func candidatePreferred(candidate, current CapabilityProof) bool {
	candidateRank := candidatePreference(candidate)
	currentRank := candidatePreference(current)
	if candidateRank != currentRank {
		return candidateRank < currentRank
	}
	if candidate.PerformanceScore != current.PerformanceScore {
		return candidate.PerformanceScore > current.PerformanceScore
	}
	if candidate.Hook != current.Hook {
		return candidate.Hook < current.Hook
	}
	return candidate.Mode < current.Mode
}

func candidatePreference(proof CapabilityProof) int {
	// Ordered fallback: best performance per unit of operational overhead first,
	// universal compatibility last.
	//
	// AF_XDP zero-copy leads because it is the only zero-copy path that keeps
	// the kernel netdev. There is no PCI re-bind, no hugepage reservation and no
	// exclusive device ownership, so a power loss or an interrupted apply cannot
	// leave a black port. It reaches near-DPDK throughput on drivers that
	// implement zero-copy.
	//
	// DPDK follows: VFIO is the highest raw-throughput path and the only one
	// with DMA isolation; UIO is the same PMD with the no-IOMMU compatibility
	// binding. DPDK PMDs cover the widest set of server NICs, so it is the
	// high-performance fallback when AF_XDP zero-copy is unavailable.
	//
	// RDMA DV is a specialised high-performance path for RDMA-capable NICs.
	// AF_XDP copy stays above AF_PACKET because it still avoids the rebind, even
	// though every packet is copied. AF_PACKET is the universal, least invasive
	// compatibility path and is always the last resort.
	if proof.Hook == NativeHookAFXDP && proof.Mode == NativeModeZeroCopy {
		return 0
	}
	if proofTier(proof) == DataplaneTierDPDK || proof.Hook == NativeHookDPDK {
		switch proof.Mode {
		case NativeModeDPDKVFIO:
			return 1
		case NativeModeDPDKUIO:
			return 2
		default:
			return 100
		}
	}
	switch {
	case proof.Hook == NativeHookRDMA && proof.Mode == NativeModeRDMADV:
		return 3
	case proof.Hook == NativeHookAFXDP && proof.Mode == NativeModeCopy:
		return 4
	case isAFPacketCompatibilityProof(proof):
		return 5
	default:
		return 100
	}
}

func isAFPacketCompatibilityProof(proof CapabilityProof) bool {
	return proof.AcceptanceOnly &&
		(proof.Hook == NativeHookAFPacket && proof.Mode == NativeModeAFPacket ||
			proof.Hook == NativeHookTapBridge && proof.Mode == NativeModeTapBridge)
}

func proofTier(proof CapabilityProof) DataplaneTier {
	if proof.Tier != "" {
		return proof.Tier
	}
	if proof.Hook == NativeHookDPDK {
		return DataplaneTierDPDK
	}
	return DataplaneTierNative
}

// dpdkTransportForMode maps the DPDK binding mode to the operator-facing
// transport name. The proof records it explicitly so reports can say whether
// the NIC was taken over through IOMMU-protected VFIO or the no-IOMMU UIO
// fallback without re-deriving the binding from the mode.
func dpdkTransportForMode(mode NativeMode) string {
	switch mode {
	case NativeModeDPDKVFIO:
		return "vfio"
	case NativeModeDPDKUIO:
		return "uio"
	default:
		return ""
	}
}

func proofTransport(proof CapabilityProof) string {
	if value := strings.ToLower(strings.TrimSpace(proof.Transport)); value != "" {
		return value
	}
	return dpdkTransportForMode(proof.Mode)
}

func selectedVPPInterface(proof CapabilityProof, linuxInterface string) string {
	if value := strings.TrimSpace(proof.VPPInterface); value != "" {
		return value
	}
	return "lyroute-" + linuxInterface
}

// nativeAttachmentVPPNames enforces one product naming contract for every VPP
// port. A DPDK PMD attaches its device under a hardware name such as
// GigabitEthernet0/3/0; the product renames it to lyroute-<linux> during the
// dataplane transaction and addresses it only by that product name afterwards.
// The observed hardware name is carried separately so the rename can be
// replayed, but it never leaks into routes, NAT, status or evidence.
func nativeAttachmentVPPNames(proof CapabilityProof, linuxInterface string) (string, string) {
	linuxInterface = strings.TrimSpace(linuxInterface)
	if proofTier(proof) != DataplaneTierDPDK {
		return selectedVPPInterface(proof, linuxInterface), ""
	}
	product := "lyroute-" + linuxInterface
	hardware := strings.TrimSpace(proof.VPPInterface)
	if hardware == product {
		hardware = ""
	}
	return product, hardware
}

const afConsentRequiredReason = "AF_PACKET compatibility mode requires explicit operator consent"

func candidateRejectionReasons(proof CapabilityProof, now time.Time, tier DataplaneTier, acceptanceAFPacket, afConsent bool) []string {
	reasons := make([]string, 0)
	acceptancePath := acceptanceAFPacketProof(acceptanceAFPacket || afConsent, proof)
	// The four-state capability result is authoritative when present. Unknown
	// must take the conservative path instead of being rounded up to support.
	switch NormalizeCapabilityStatus(string(proof.CapabilityStatus)) {
	case CapabilityUnsupported:
		if proof.CapabilityStatus != "" {
			reason := strings.TrimSpace(proof.CapabilityReason)
			if reason == "" {
				reason = "capability probe reported the candidate as unsupported"
			}
			reasons = append(reasons, reason)
			return reasons
		}
	case CapabilityUnknown:
		if proof.CapabilityStatus != "" {
			reason := strings.TrimSpace(proof.CapabilityReason)
			if reason == "" {
				reason = "capability probe returned unknown; the conservative path is required"
			}
			reasons = append(reasons, reason)
			return reasons
		}
	}
	runtimeProof := isRuntimeCapabilityProof(proof) && proof.RuntimeVerified && !proof.ObservedAt.IsZero()
	if !runtimeProof {
		reasons = append(reasons, "runtime capability proof is required")
	}
	if now.IsZero() || !runtimeProof || proof.ValidUntil.Before(now) || proof.ObservedAt.After(now) {
		reasons = append(reasons, "runtime capability proof is missing or stale")
	}
	lifetime := proof.ValidUntil.Sub(proof.ObservedAt)
	if !runtimeProof || lifetime <= 0 {
		reasons = append(reasons, "runtime capability proof validity must end after observation")
	}
	if isAFPacketCompatibilityProof(proof) {
		if !afConsent && !acceptanceAFPacket {
			reasons = append(reasons, afConsentRequiredReason)
		}
		if !hardwareAddressSafe(proof.MACAddress) {
			reasons = append(reasons, "AF_PACKET candidate has no safe Linux interface MAC address")
		}
		// AF_PACKET is a consented compatibility fallback. It is never
		// high-performance, and it is never a silent bypass: the selection path
		// marks the resulting generation degraded.
		return reasons
	}
	if !acceptedNativePerformance(proof) && !acceptancePath {
		reasons = append(reasons, "proof does not establish high-performance mode")
	}
	if NativeProbeDriverUnsupported(proof.Hook, proof.KernelDriver) {
		reasons = append(reasons, "AF_XDP/RDMA is not supported on kernel driver "+strings.TrimSpace(proof.KernelDriver)+" (persisted read-only verdict)")
	}
	switch tier {
	case DataplaneTierNative:
		if (!approvedNativeMode(proof.Hook, proof.Mode) && !acceptancePath) || !proof.Native {
			reasons = append(reasons, "candidate is not an approved VPP-native high-performance path")
		}
	case DataplaneTierDPDK:
		if proof.Hook != NativeHookDPDK || proof.Mode != NativeModeDPDKVFIO && proof.Mode != NativeModeDPDKUIO || proof.Native {
			reasons = append(reasons, "candidate is not an approved VPP DPDK PCI path")
		}
		if DPDKTransportOverride() == "uio" && proof.Mode == NativeModeDPDKVFIO {
			reasons = append(reasons, "VFIO is disabled by LY_ROUTE_DPDK_TRANSPORT=uio")
		}
		if expectedTransport := dpdkTransportForMode(proof.Mode); expectedTransport != "" {
			if recorded := strings.ToLower(strings.TrimSpace(proof.Transport)); recorded != "" && recorded != expectedTransport {
				reasons = append(reasons, "DPDK transport does not match the PCI binding mode")
			}
		}
		if !pciAddressSafe(proof.PCIAddress) {
			reasons = append(reasons, "DPDK candidate has no safe PCI address")
		}
		if strings.TrimSpace(proof.KernelDriver) == "" {
			reasons = append(reasons, "DPDK candidate has no current kernel driver")
		}
		if proof.Verification == "failed" {
			reasons = append(reasons, "DPDK candidate takeover verification failed")
		} else if proof.Verification == "" && proof.FunctionalMethod != "" && !proof.FunctionalVerified {
			reasons = append(reasons, "DPDK candidate was not functionally verified")
		}
		switch proof.Mode {
		case NativeModeDPDKVFIO:
			if !decimalIdentifierSafe(proof.IOMMUGroup) || !proof.IOMMUProtected {
				reasons = append(reasons, "DPDK candidate is not protected by an IOMMU group")
			}
			if !proof.VFIOAvailable {
				reasons = append(reasons, "vfio-pci is unavailable")
			}
		case NativeModeDPDKUIO:
			if !proof.UIOPCIAvailable {
				reasons = append(reasons, "uio_pci_generic is unavailable")
			}
			if strings.TrimSpace(proof.IOMMUGroup) != "none" {
				reasons = append(reasons, "UIO candidate unexpectedly reports an IOMMU group")
			}
		}
		if !proof.HugepagesAvailable {
			reasons = append(reasons, "VPP hugepages are unavailable")
		}
		if !proof.DPDKPluginAvailable {
			reasons = append(reasons, "VPP DPDK plugin is unavailable")
		}
	default:
		reasons = append(reasons, "unknown dataplane tier")
	}
	// Smart QoS availability is intentionally not part of candidate rejection:
	// the feature is retired and must never lock a forwarding candidate.
	return reasons
}

func pciAddressSafe(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 12 || value[4] != ':' || value[7] != ':' || value[10] != '.' {
		return false
	}
	for index, char := range value {
		if index == 4 || index == 7 || index == 10 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func decimalIdentifierSafe(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
