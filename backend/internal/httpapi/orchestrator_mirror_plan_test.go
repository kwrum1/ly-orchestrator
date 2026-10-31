package httpapi

import (
	"context"
	"testing"

	"ly-route/backend/internal/product"
)

func TestOrchestratorRuntimePlanCompilesPortMirror(t *testing.T) {
	// The 镜像口 page only edits the interface document; apply must compile the
	// SPAN session into the Orchestrator generation. Before this test the
	// Orchestrator plan never read the mirror fields, so `vppctl show interface
	// span` stayed empty even though the API and the page looked configured.
	server, _, _ := productTestServer(t, product.Orchestrator())
	seedInterfaceContractDocument(t, server, "eth2", map[string]any{
		"id":           "eth2",
		"interface_id": "eth2",
		"mirror":       true,
		"mirror_sources": []any{
			map[string]any{"interface": "eth1", "direction": "both"},
		},
	})

	plan, err := server.buildRuntimePlan(context.Background(), "orchestrator-mirror-plan")
	if err != nil {
		t.Fatalf("build Orchestrator runtime plan: %v", err)
	}
	mirror := plan.GatewayPlan.Mirror
	if mirror == nil {
		t.Fatal("Orchestrator plan did not compile the desired mirror session")
	}
	if mirror.LinuxInterface == "" || mirror.Destination != "lyroute-"+mirror.LinuxInterface {
		t.Fatalf("mirror destination = %#v, want lyroute-<linux name>", mirror)
	}
	if len(mirror.Sources) != 1 || mirror.Sources[0].LinuxInterface == "" || mirror.Sources[0].Interface != "lyroute-"+mirror.Sources[0].LinuxInterface || mirror.Sources[0].Direction != "both" {
		t.Fatalf("mirror sources = %#v, want one source with direction both", mirror.Sources)
	}
}
