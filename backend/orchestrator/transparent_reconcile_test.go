package orchestrator

import (
	"strings"
	"testing"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

func testPolicyView(defaultKind orchestrator.ActionKind, group string) *orchestrator.PolicyView {
	view := orchestrator.PolicyView{SchemaVersion: orchestrator.PolicySchemaVersion, Default: orchestrator.ActionInput{Kind: defaultKind}}
	if group != "" {
		view.Groups = []orchestrator.PolicyGroupView{{ID: group, Position: 10}}
	}
	return &view
}

func TestTransparentStateMatchesConfigurationUsesGenerationAndDefault(t *testing.T) {
	direct := testPolicyView(orchestrator.ActionDirect, "")
	drop := testPolicyView(orchestrator.ActionDrop, "")
	generation, err := transparentGeneration(orchestrator.TopologyView{}, direct)
	if err != nil {
		t.Fatal(err)
	}
	dropGeneration, err := transparentGeneration(orchestrator.TopologyView{}, drop)
	if err != nil {
		t.Fatal(err)
	}
	if generation == dropGeneration {
		t.Fatal("default action did not change the transparent generation")
	}
	cases := []struct {
		name       string
		observed   vpp.TransparentOrchestratorReadback
		generation string
		defaults   string
		want       bool
	}{
		{"running match", vpp.TransparentOrchestratorReadback{State: "running", Generation: generation, Default: "direct"}, generation, "direct", true},
		{"vpp default drop", vpp.TransparentOrchestratorReadback{State: "running", Generation: generation, Default: "drop"}, generation, "direct", false},
		{"stale generation", vpp.TransparentOrchestratorReadback{State: "running", Generation: dropGeneration, Default: "drop"}, generation, "direct", false},
		{"locked after vpp restart", vpp.TransparentOrchestratorReadback{State: "locked"}, generation, "direct", false},
		{"unknown default tolerated", vpp.TransparentOrchestratorReadback{State: "running", Generation: generation}, generation, "direct", true},
	}
	for _, testCase := range cases {
		if got := transparentStateMatchesConfiguration(testCase.observed, testCase.generation, testCase.defaults); got != testCase.want {
			t.Fatalf("%s: matches = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

func TestTransparentDriftReasonNamesConfigurationAndVPPState(t *testing.T) {
	reason := transparentDriftReason("abcdef0123456789", "direct", vpp.TransparentOrchestratorReadback{
		State:      "running",
		Generation: "9999999999999999",
		Default:    "drop",
	}, nil)
	for _, fragment := range []string{"configured generation abcdef012345", "default direct", "VPP state running", "generation 999999999999", "default drop"} {
		if !strings.Contains(reason, fragment) {
			t.Fatalf("drift reason %q is missing %q", reason, fragment)
		}
	}
	missing := transparentDriftReason("abcdef0123456789", "drop", vpp.TransparentOrchestratorReadback{}, nil)
	if !strings.Contains(missing, "default unknown") {
		t.Fatalf("missing readback reason %q does not name an unknown default", missing)
	}
}

func TestTransparentDefaultActionFallsBackToDirect(t *testing.T) {
	if got := transparentDefaultAction(nil); got != string(orchestrator.ActionDirect) {
		t.Fatalf("nil policy default = %q, want direct", got)
	}
	if got := transparentDefaultAction(testPolicyView(orchestrator.ActionDrop, "")); got != string(orchestrator.ActionDrop) {
		t.Fatalf("drop policy default = %q, want drop", got)
	}
}

func TestTransparentGenerationIsStableForIdenticalConfiguration(t *testing.T) {
	policy := testPolicyView(orchestrator.ActionDirect, "grp-a")
	first, err := transparentGeneration(orchestrator.TopologyView{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	second, err := transparentGeneration(orchestrator.TopologyView{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("generation is not stable: %s != %s", first, second)
	}
}
