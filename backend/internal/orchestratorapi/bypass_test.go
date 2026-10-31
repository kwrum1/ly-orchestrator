package orchestratorapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type bypassTestController struct {
	state   BypassState
	updates int
	forced  *bool
	config  *BypassConfig
}

func (controller *bypassTestController) BypassStatus(context.Context) (BypassState, error) {
	return controller.state, nil
}

func (controller *bypassTestController) SetBypass(_ context.Context, enabled bool, reason string) (BypassState, error) {
	controller.updates++
	controller.forced = &enabled
	controller.state.Enabled = enabled
	controller.state.Forced = enabled
	controller.state.Reason = reason
	return controller.state, nil
}

func (controller *bypassTestController) SetGroupBypass(_ context.Context, group string, enabled bool, reason string) (BypassState, error) {
	controller.updates++
	controller.forced = &enabled
	controller.state.Enabled = enabled
	controller.state.Forced = enabled
	controller.state.Reason = reason
	controller.state.Groups = []BypassGroupState{{Group: group, Enabled: enabled, Forced: enabled, Reason: reason}}
	return controller.state, nil
}

func (controller *bypassTestController) UpdateBypassConfig(_ context.Context, config BypassConfig) (BypassState, error) {
	controller.config = &config
	controller.state.Config = config
	return controller.state, nil
}

func TestBypassAPIReadsForcesAndUpdatesConfig(t *testing.T) {
	repository, _, _ := newTestHTTPServerWithHandler(t)
	controller := &bypassTestController{state: BypassState{Cause: "none"}}
	handler, err := New(repository, allowAccess{})
	if err != nil {
		t.Fatal(err)
	}
	handler.SetBypassController(controller)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	response := sendRequest(t, server, http.MethodGet, BypassPath, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET bypass: %d %s", response.StatusCode, response.Body)
	}
	var state BypassState
	decodeResponse(t, response.Body, &state)
	if state.Enabled {
		t.Fatalf("unexpected initial bypass: %+v", state)
	}

	response = sendRequest(t, server, http.MethodPost, BypassPath, []byte(`{"enabled":true,"reason":"drill"}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST bypass: %d %s", response.StatusCode, response.Body)
	}
	if controller.forced == nil || !*controller.forced || controller.state.Reason != "drill" {
		t.Fatalf("forced bypass = %#v", controller)
	}

	config := []byte(`{"enabled":true,"interval_seconds":5,"timeout_ms":750,"failure_threshold":2,"recovery_threshold":4,"tcp_targets":["192.168.10.82:80"]}`)
	response = sendRequest(t, server, http.MethodPut, BypassConfigPath, config)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT config: %d %s", response.StatusCode, response.Body)
	}
	if controller.config == nil || controller.config.FailureThreshold != 2 || controller.config.RecoveryThreshold != 4 {
		t.Fatalf("config = %#v", controller.config)
	}

	response = sendRequest(t, server, http.MethodPost, BypassPath, []byte(`{"enabled":false,"reason":"clear"}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST clear: %d %s", response.StatusCode, response.Body)
	}
	if controller.forced == nil || *controller.forced {
		t.Fatalf("clear did not disable the bypass")
	}
	var decoded BypassState
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
