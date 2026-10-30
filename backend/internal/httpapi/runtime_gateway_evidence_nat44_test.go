package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/vpp"
)

func TestGatewayPlanResourcesExpectsNAT44ForPortMaps(t *testing.T) {
	plan := vpp.Plan{NAT: nat.CompiledConfig{PortMappings: []nat.PortMapping{{ID: "pm-1"}}}}
	resources := gatewayPlanResources(plan)
	if _, ok := resources["nat44"]; !ok {
		t.Fatalf("port-map generation must expect the nat44 apply receipt: %#v", resources)
	}
	if _, ok := resources["port-maps"]; !ok {
		t.Fatalf("port-map generation must expect the port-maps receipt: %#v", resources)
	}
}

func TestGatewayPlanResourcesOmitsNAT44WithoutMappings(t *testing.T) {
	resources := gatewayPlanResources(vpp.Plan{})
	if _, ok := resources["nat44"]; ok {
		t.Fatalf("empty generation must not expect a nat44 receipt: %#v", resources)
	}
}
