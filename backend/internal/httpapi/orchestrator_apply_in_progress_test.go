package httpapi

import (
	"context"
	"errors"
	"testing"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/product"
)

// A read that arrives while an apply holds the dataplane mutex is a transient
// state. Reporting it as degraded made every configuration change look like a
// broken appliance for the whole duration of the apply.
func TestApplyInProgressIsNotDegraded(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	profile, err := product.ParseProfile(product.Orchestrator().ID().String())
	if err != nil {
		t.Fatal(err)
	}
	server.profile = profile
	// No repository: the unconfigured-topology branch is skipped and the status
	// path goes straight to the runtime evidence provider.
	server.orchestratorRepository = nil
	server.orchestratorRuntime = &healthTransparentRuntime{
		err: errors.Join(orchestratorapi.ErrTransparentApplyInProgress, errors.New("(started 12s ago)")),
	}

	components, handled := server.orchestratorStatusComponents(context.Background())
	if !handled {
		t.Fatal("orchestrator status was not handled")
	}
	found := false
	for _, component := range components {
		if component.Name != "vpp" {
			continue
		}
		found = true
		if component.State != "applying" {
			t.Fatalf("vpp state = %q, want applying", component.State)
		}
		if !component.Available {
			t.Fatal("an in-progress apply must stay available so health is not degraded")
		}
	}
	if !found {
		t.Fatal("no vpp component reported")
	}
	if got := server.runtimeEvidence().Status; got != "applying" {
		t.Fatalf("last_apply.status = %q, want applying", got)
	}
	if capability := server.orchestratorRuntimeCapability(context.Background()); capability != nil {
		t.Fatalf("health capability during apply = %+v, want nil", capability)
	}
}
