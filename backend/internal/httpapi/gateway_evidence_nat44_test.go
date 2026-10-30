package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/vpp"
)

func TestGatewayPlanResourcesOwnsNAT44ForPortMapsWithoutStaticMappings(t *testing.T) {
	plan := vpp.Plan{
		NAT: nat.CompiledConfig{
			Behavior:     nat.BehaviorEndpointDependent,
			PortMappings: []nat.PortMapping{{ID: "acc-portmap"}},
		},
	}
	resources := gatewayPlanResources(plan)
	if _, ok := resources["nat44"]; !ok {
		t.Fatalf("resources = %#v, want nat44 owned by a port-map-only plan", resources)
	}
	if _, ok := resources["port-maps"]; !ok {
		t.Fatalf("resources = %#v, want port-maps owned", resources)
	}
}

func TestGatewayPlanResourcesDoesNotInventNAT44WithoutOperations(t *testing.T) {
	resources := gatewayPlanResources(vpp.Plan{})
	if _, ok := resources["nat44"]; ok {
		t.Fatalf("resources = %#v, empty plan must not own nat44", resources)
	}
}
