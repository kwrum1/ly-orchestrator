package vpp

import (
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/flow"
)

// ResolveFlowShapingInterfaces binds the abstract LAN/WAN attachment markers
// produced by flow.CompileIntent to concrete VPP interface names. Shaping is
// interface-level and egress-only, so one abstract object may expand into one
// object per LAN (downlink) or WAN (uplink) interface.
//
// It is called while the canonical gateway plan is built, before the plan is
// persisted or diffed. A plan without an interface for a requested direction
// fails closed instead of silently shaping only part of the path.
func ResolveFlowShapingInterfaces(compiled *flow.CompiledIntent, assignments []AddressAssignment) error {
	if compiled == nil {
		return nil
	}
	lan := vppInterfacesForRole(assignments, "lan")
	wan := vppInterfacesForRole(assignments, "wan")
	for groupIndex := range compiled.VPPGroups {
		group := &compiled.VPPGroups[groupIndex]
		if strings.TrimSpace(group.Kind) != "vpp.shape" {
			continue
		}
		objects := make([]flow.VPPObject, 0, len(group.Objects))
		for _, object := range group.Objects {
			if object.Shaping == nil {
				objects = append(objects, object)
				continue
			}
			if len(object.Attachments) == 0 {
				return fmt.Errorf("shape rule %q has no interface attachment", object.RuleID)
			}
			for _, attachment := range object.Attachments {
				switch {
				case strings.Contains(attachment, "$LY_ROUTE_LAN_INTERFACE"):
					if len(lan) == 0 {
						return fmt.Errorf("shape rule %q downlink requires at least one LAN interface", object.RuleID)
					}
					for _, interfaceName := range lan {
						objects = append(objects, resolvedShapeObject(object, attachment, interfaceName, "downlink", "lan"))
					}
				case strings.Contains(attachment, "$LY_ROUTE_WAN_INTERFACE"):
					if len(wan) == 0 {
						return fmt.Errorf("shape rule %q uplink requires a configured WAN interface", object.RuleID)
					}
					for _, interfaceName := range wan {
						objects = append(objects, resolvedShapeObject(object, attachment, interfaceName, "uplink", "wan"))
					}
				default:
					objects = append(objects, object)
				}
			}
		}
		group.Objects = objects
	}
	return nil
}

// ValidateFlowShapingInterfaces reports a missing LAN/WAN role before a
// policy is stored. ResolveFlowShapingInterfaces repeats the check while the
// canonical plan is built so a plan can never half-apply a "both" shaper.
func ValidateFlowShapingInterfaces(compiled flow.CompiledIntent, assignments []AddressAssignment) error {
	lan := vppInterfacesForRole(assignments, "lan")
	wan := vppInterfacesForRole(assignments, "wan")
	for _, group := range compiled.VPPGroups {
		if strings.TrimSpace(group.Kind) != "vpp.shape" {
			continue
		}
		for _, object := range group.Objects {
			for _, attachment := range object.Attachments {
				switch {
				case strings.Contains(attachment, "$LY_ROUTE_LAN_INTERFACE") && len(lan) == 0:
					return fmt.Errorf("shape rule %q downlink requires at least one LAN interface", object.RuleID)
				case strings.Contains(attachment, "$LY_ROUTE_WAN_INTERFACE") && len(wan) == 0:
					return fmt.Errorf("shape rule %q uplink requires a configured WAN interface", object.RuleID)
				}
			}
		}
	}
	return nil
}

func resolvedShapeObject(object flow.VPPObject, attachment, interfaceName, direction, role string) flow.VPPObject {
	marker := "lyroute-$LY_ROUTE_LAN_INTERFACE"
	if role == "wan" {
		marker = "lyroute-$LY_ROUTE_WAN_INTERFACE"
	}
	resolved := object
	resolved.Attachments = []string{strings.ReplaceAll(attachment, marker, interfaceName)}
	if object.Shaping != nil {
		shaping := *object.Shaping
		shaping.Direction = direction
		if shaping.HostIsolation == flow.HostIsolationAuto {
			shaping.HostIsolation = shaping.InterfaceHostIsolation(role)
		}
		resolved.Shaping = &shaping
	}
	return resolved
}

func vppInterfacesForRole(assignments []AddressAssignment, role string) []string {
	result := make([]string, 0, len(assignments))
	seen := map[string]struct{}{}
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), role) {
			continue
		}
		name := strings.TrimSpace(assignment.VPPInterface)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}
