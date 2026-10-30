package vpp

import "strings"

// AugmentObservationWithCommittedProofs fills in capability proofs for ports
// the live report does not cover, using the last committed dataplane
// generation.
//
// A live capability report only proves the ports the probe could attach in this
// cycle. An address-only change on an already attached port (for example
// editing a WAN next hop) must not turn into dataplane_locked just because the
// probe missed that port: the committed generation stays authoritative until
// the next transaction replaces it. Live proofs are never overridden, so a
// genuine capability loss on a port the probe did cover still fails closed.
func AugmentObservationWithCommittedProofs(observation Observation, committedStatePath string, interfaces []string) Observation {
	if len(interfaces) == 0 {
		return observation
	}
	covered := make(map[string]bool, len(observation.Report.Proofs))
	for _, proof := range observation.Report.Proofs {
		if name := strings.TrimSpace(proof.LinuxInterface); name != "" {
			covered[name] = true
		}
	}
	missing := make([]string, 0, len(interfaces))
	for _, raw := range interfaces {
		name := strings.TrimSpace(raw)
		if name == "" || covered[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return observation
	}
	committed, ok := committedCapabilityReport(committedStatePath)
	if !ok {
		return observation
	}
	byName := make(map[string]InterfaceProof, len(committed.Proofs))
	for _, proof := range committed.Proofs {
		byName[strings.TrimSpace(proof.LinuxInterface)] = proof
	}
	added := make([]string, 0, len(missing))
	proofs := append([]InterfaceProof(nil), observation.Report.Proofs...)
	for _, name := range missing {
		proof, found := byName[name]
		if !found {
			continue
		}
		proofs = append(proofs, proof)
		added = append(added, name)
	}
	if len(added) == 0 {
		return observation
	}
	observation.Report.Proofs = proofs
	note := "reusing the committed attachment proof for " + strings.Join(added, ",") + " because the live probe did not report those ports"
	if strings.TrimSpace(observation.Reason) == "" {
		observation.Reason = note
	} else {
		observation.Reason += "; " + note
	}
	return observation
}
