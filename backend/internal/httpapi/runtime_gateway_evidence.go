package httpapi

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/vpp"
)

func completeGatewayRuntimeEvidence(result RuntimeApplyResult, now time.Time) error {
	if result.GatewayPlan == nil {
		if len(result.GatewayEvidence) == 0 {
			return nil
		}
		seen := make(map[string]struct{}, len(result.GatewayEvidence))
		for _, evidence := range result.GatewayEvidence {
			if _, duplicate := seen[evidence.Resource]; duplicate {
				return gatewayEvidenceError(apply.GatewayEvidenceIncomplete, evidence.Resource, "resource", "unique", evidence.Resource)
			}
			seen[evidence.Resource] = struct{}{}
		}
		return apply.ValidateGatewayEvidence(result.GatewayEvidence, result.TransactionID, now)
	}
	if err := apply.ValidateGatewayEvidence(result.GatewayEvidence, result.TransactionID, now); err != nil {
		return err
	}
	planned := gatewayPlanResources(*result.GatewayPlan)
	expected := make(map[string]struct{}, len(planned)+len(result.GatewayEvidence))
	for resource := range planned {
		expected[resource] = struct{}{}
	}
	seen := make(map[string]struct{}, len(result.GatewayEvidence))
	for _, evidence := range result.GatewayEvidence {
		if _, duplicate := seen[evidence.Resource]; duplicate {
			return gatewayEvidenceError(apply.GatewayEvidenceIncomplete, evidence.Resource, "resource", "unique", evidence.Resource)
		}
		seen[evidence.Resource] = struct{}{}
		_, resourcePlanned := planned[evidence.Resource]
		if !resourcePlanned {
			// A route-owned resource can be present in committed evidence when
			// the persisted core route list is empty (DNS interception/proxy
			// steering are materialized as supplemental routes). Other extra
			// resources, especially deletion evidence, still fail closed.
			if evidence.Resource == "routes" && !evidence.Deleted {
				expected[evidence.Resource] = struct{}{}
				continue
			}
			if !evidence.Deleted {
				// The aggregate security generation is always emitted, even when it
				// retires nothing, so an empty generation legitimately produces an
				// "acls" row while the persisted plan lists no ACL objects. Accept
				// exactly that shape; every other unplanned row (including a
				// deletion row that claims Delivered=false) keeps failing closed.
				if emptySecurityGenerationEvidence(*result.GatewayPlan, evidence) {
					expected[evidence.Resource] = struct{}{}
					continue
				}
				return gatewayEvidenceError(apply.GatewayEvidenceIncomplete, evidence.Resource, "deleted", "true for resource absent from persisted plan", "false")
			}
			if err := proveDeletedGatewayResource(evidence); err != nil {
				return err
			}
		} else {
			if err := proveDesiredGatewayResource(evidence, *result.GatewayPlan); err != nil {
				return err
			}
		}
		expected[evidence.Resource] = struct{}{}
	}
	for resource := range expected {
		if _, present := seen[resource]; !present {
			return gatewayEvidenceError(apply.GatewayEvidenceIncomplete, resource, "resource", "persisted evidence entry", "missing")
		}
	}
	return nil
}

// emptySecurityGenerationEvidence reports the one legitimate "unplanned but
// committed" row: the always-emitted security generation committed an empty
// ACL state and the plan has no security objects either.
func emptySecurityGenerationEvidence(desired vpp.Plan, evidence apply.GatewayResourceEvidence) bool {
	if evidence.Resource != "acls" {
		return false
	}
	if len(evidence.After.ACLs) != 0 {
		return false
	}
	return len(desired.Policy.SecurityACLs) == 0 && len(desired.Security.ACLs) == 0 &&
		len(desired.Security.MACIP) == 0 && len(desired.Security.AttackRules) == 0
}

func proveDesiredGatewayResource(evidence apply.GatewayResourceEvidence, desired vpp.Plan) error {
	// The matcher is shared with the apply transaction. A readback that does
	// not satisfy the persisted plan fails the transaction before it can be
	// committed, so this restart/commit check sees the same semantics instead
	// of accepting a generation that never converged.
	matched := vpp.GatewaySnapshotMatchesResource(evidence.Resource, evidence.After, desired)
	if matched {
		owner := vpp.SupplementalOwner(evidence.Resource)
		if owner == vpp.SupplementalInterfaces || owner == vpp.SupplementalRoutes || owner == vpp.SupplementalQoS || owner == vpp.SupplementalServiceNetworks {
			evidencePlan := desired
			// A committed gateway transaction has already completed the privileged
			// DPDK bind/restart/readback phase. Reconstruct supplemental operations
			// in that prepared state instead of re-triggering the pre-apply gate.
			// The persisted plan's native capability proof has also legitimately
			// expired by the time a restart validates the committed evidence, so
			// derive the operations declaratively instead of failing with
			// dataplane_locked (which surfaced as snapshot_mismatch on restart).
			evidencePlan.DataplanePrepared = true
			evidencePlan.AllowUnprovenOperations = true
			if err := vpp.ValidateCommittedSupplementalReadback(evidencePlan, owner, evidence.SupplementalReadback, evidence.Readback.Timestamp); err != nil {
				return gatewayEvidenceError(apply.GatewayEvidenceSnapshotMismatch, evidence.Resource, "supplemental_readback", "persisted desired operation payloads", err.Error())
			}
		}
		return nil
	}
	return gatewayEvidenceError(apply.GatewayEvidenceSnapshotMismatch, evidence.Resource, "after", "persisted desired resource payload", "different live payload")
}

func equalGatewaySlice[T any](actual, desired []T) bool {
	if len(actual) == 0 && len(desired) == 0 {
		return true
	}
	actualJSON, actualErr := json.Marshal(actual)
	desiredJSON, desiredErr := json.Marshal(desired)
	return actualErr == nil && desiredErr == nil && bytes.Equal(actualJSON, desiredJSON)
}

func equalGatewaySliceUnordered[T any](actual, desired []T) bool {
	if len(actual) != len(desired) {
		return false
	}
	canonical := func(items []T) ([]string, bool) {
		values := make([]string, 0, len(items))
		for _, item := range items {
			encoded, err := json.Marshal(item)
			if err != nil {
				return nil, false
			}
			values = append(values, string(encoded))
		}
		sort.Strings(values)
		return values, true
	}
	actualValues, actualOK := canonical(actual)
	desiredValues, desiredOK := canonical(desired)
	if !actualOK || !desiredOK {
		return false
	}
	for index := range actualValues {
		if actualValues[index] != desiredValues[index] {
			return false
		}
	}
	return true
}

func proveDeletedGatewayResource(evidence apply.GatewayResourceEvidence) error {
	if evidence.Resource == "service-networks" {
		// Service networks are supplemental DNS/proxy TAP+NAT operations, not
		// rows in the typed VPP snapshot. The after readback is the typed
		// after state: Apply returns only the operations that remain in the
		// desired generation, so a deleted row must have no active
		// service-network operation left. Looking for a snapshot payload here
		// always produced "deleted expected=true, actual=false".
		for _, item := range evidence.SupplementalReadback {
			if item.Name == "vpp.dns-service.network" || item.Name == "vpp.proxy-service.network" {
				return gatewayEvidenceError(apply.GatewayEvidenceSnapshotMismatch, evidence.Resource, "supplemental_readback", "no active service network", item.Name)
			}
		}
		return nil
	}
	beforePresent, afterPresent := gatewaySnapshotPayloadPresence(evidence.Resource, evidence.Before), gatewaySnapshotPayloadPresence(evidence.Resource, evidence.After)
	if beforePresent && !afterPresent {
		return nil
	}
	return gatewayEvidenceError(apply.GatewayEvidenceSnapshotMismatch, evidence.Resource, "deleted", "typed prior payload present and after payload absent", "prior or after payload is invalid")
}

func gatewaySnapshotPayloadPresence(resource string, snapshot vpp.Snapshot) bool {
	switch resource {
	case "interfaces":
		return len(snapshot.Interfaces) > 0
	case "bonds":
		return len(snapshot.Bonds) > 0
	case "wan-groups":
		return len(snapshot.WANGroups) > 0
	case "routes":
		return len(snapshot.RoutePolicies) > 0
	case "acls":
		return len(snapshot.ACLs) > 0
	case "qos":
		return len(snapshot.QoS) > 0
	case "nat44":
		return len(snapshot.NAT.StaticMappings) > 0
	case "port-maps":
		return len(snapshot.NAT.PortMappings) > 0
	default:
		return false
	}
}

func gatewayPlanResources(plan vpp.Plan) map[string]struct{} {
	resources := make(map[string]struct{})
	// Resource inventory is evaluated after the privileged dataplane phase. The
	// persisted plan intentionally does not carry that transient flag, so use a
	// prepared copy only for discovering supplemental-only resource ownership.
	prepared := plan
	prepared.DataplanePrepared = true
	hasSupplemental := func(owner vpp.SupplementalOwner) bool {
		return vpp.HasSupplementalOperations(prepared, owner)
	}
	// Native attachments (the DPDK/AF_XDP data ports) own an interfaces
	// evidence row even when the persisted plan carries no LCP interface
	// objects. Rebuilding the supplemental operations can fail after a restart
	// because the native capability proof has expired, so the persisted
	// assignment list is the authoritative declaration here.
	if len(plan.Interfaces) > 0 || len(plan.NativePath.Assignments) > 0 || hasSupplemental(vpp.SupplementalInterfaces) {
		resources["interfaces"] = struct{}{}
	}
	if len(plan.Bonds) > 0 {
		resources["bonds"] = struct{}{}
	}
	if len(plan.Policy.WANGroups) > 0 {
		resources["wan-groups"] = struct{}{}
	}
	// DNS interception and proxy steering are route-owned VPP operations even
	// when the persisted core route-policy list is empty.
	if len(plan.Policy.RoutePolicies) > 0 || hasSupplemental(vpp.SupplementalRoutes) {
		resources["routes"] = struct{}{}
	}
	// The aggregate security generation owns the ACL resource even when the
	// legacy per-ACL list is empty. Derive ownership from the persisted plan
	// fields first: rebuilding the operation list can legitimately fail (for
	// example a dataplane tier proof that is only valid while the privileged
	// transaction is running), and restart evidence must not degrade because a
	// best-effort rebuild was locked.
	if len(plan.Policy.SecurityACLs) > 0 || len(plan.Security.ACLs) > 0 || len(plan.Security.MACIP) > 0 || len(plan.Security.AttackRules) > 0 || hasSupplemental(vpp.SupplementalSecurity) {
		resources["acls"] = struct{}{}
	}
	// SmartQoSAssignments is deliberately not consulted: built-in Smart QoS is
	// retired and no longer owns VPP operations, so legacy plans that only
	// carried bandwidth hints must not expect a QoS evidence row.
	if len(plan.Flow.VPPGroups) > 0 || hasSupplemental(vpp.SupplementalQoS) {
		resources["qos"] = struct{}{}
	}
	// NAT44 is initialized for port mappings, DNS service networks, NAT route
	// actions and proxy steering even when the static-mapping list is empty.
	// BuildOperations emits the same vpp.nat44.initialize operation, so the
	// evidence inventory must not treat that row as an unexpected resource (it
	// used to fail every apply that had a port-map but no static mapping).
	if vpp.PlanRequiresNAT(plan) {
		resources["nat44"] = struct{}{}
	}
	if len(plan.NAT.PortMappings) > 0 {
		resources["port-maps"] = struct{}{}
	}
	if len(plan.DNSServiceNetworks) > 0 || len(plan.Proxy.VPPSteering) > 0 || hasSupplemental(vpp.SupplementalServiceNetworks) {
		resources["service-networks"] = struct{}{}
	}
	return resources
}

func gatewayEvidenceError(code apply.GatewayEvidenceValidationCode, resource, field, expected, actual string) error {
	return &apply.GatewayEvidenceValidationError{Code: code, Resource: resource, Field: field, Expected: expected, Actual: actual}
}
