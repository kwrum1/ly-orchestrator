package orchestrator

import (
	"errors"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestTransparentRecoveryRequired(t *testing.T) {
	cases := []struct {
		name      string
		observed  vpp.TransparentOrchestratorReadback
		err       error
		committed string
		want      bool
	}{
		{name: "matching running generation", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-a"}, committed: "gen-a", want: false},
		{name: "locked plugin", observed: vpp.TransparentOrchestratorReadback{State: "locked"}, committed: "gen-a", want: true},
		{name: "generation drift", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-b"}, committed: "gen-a", want: true},
		{name: "uncommitted runtime", observed: vpp.TransparentOrchestratorReadback{State: "running", Generation: "gen-a"}, want: true},
		{name: "vpp readback failure", err: errors.New("vppctl unavailable"), committed: "gen-a", want: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := transparentRecoveryRequired(test.observed, test.err, test.committed); got != test.want {
				t.Fatalf("recovery required = %t, want %t", got, test.want)
			}
		})
	}
}
