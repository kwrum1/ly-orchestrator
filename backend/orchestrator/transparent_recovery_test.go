package orchestrator

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

// The reconciler compares the plugin readback with the generation derived from
// the persisted configuration (topology + policy), not with the last
// in-memory apply.  A data plane that still enforces an older policy must be
// detected even when its generation string was the one this process committed.
func TestTransparentStateMatchesPersistedConfiguration(t *testing.T) {
	cases := []struct {
		name       string
		observed   vpp.TransparentOrchestratorReadback
		generation string
		defaults   string
		want       bool
	}{
		{name: "matching running generation", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-a", Default: "direct"}, generation: "gen-a", defaults: "direct", want: true},
		{name: "locked plugin", observed: vpp.TransparentOrchestratorReadback{State: "locked"}, generation: "gen-a", defaults: "direct", want: false},
		{name: "generation drift", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-b", Default: "drop"}, generation: "gen-a", defaults: "direct", want: false},
		{name: "same generation wrong default", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-a", Default: "drop"}, generation: "gen-a", defaults: "direct", want: false},
		{name: "default not reported", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-a"}, generation: "gen-a", defaults: "direct", want: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := transparentStateMatchesConfiguration(test.observed, test.generation, test.defaults); got != test.want {
				t.Fatalf("matches configuration = %t, want %t", got, test.want)
			}
		})
	}
}
