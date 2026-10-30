package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
)

// hungApply blocks regardless of context cancellation, modelling a worker
// stuck in an uninterruptible PID1/VPP call. The gate must still be released
// at the transaction bound.
func hungApply(block <-chan struct{}) func(context.Context, string, http.ResponseWriter) {
	return func(context.Context, string, http.ResponseWriter) {
		<-block
	}
}

func loginAdmin(t *testing.T, server *Server) *http.Cookie {
	t.Helper()
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", login.Code, login.Body.String())
	}
	return login.Result().Cookies()[0]
}

func TestRuntimeApplyTimeoutReleasesGateAndReportsFailure(t *testing.T) {
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithApplyTimeout(80*time.Millisecond),
		withApplyGrace(50*time.Millisecond),
	)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	server.applyWorkOverride = hungApply(block)
	cookie := loginAdmin(t, server)

	started := time.Now()
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	elapsed := time.Since(started)

	if response.Code != http.StatusAccepted {
		t.Fatalf("timed-out apply status = %d, want 202: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{`"status":"apply_failed"`, `"runtime_state":"degraded"`, "bound"} {
		if !strings.Contains(body, required) {
			t.Fatalf("timed-out apply response missing %q: %s", required, body)
		}
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timed-out apply returned after %s; the bound was not enforced", elapsed)
	}
	if !server.runtimeApplyMu.TryLock() {
		t.Fatal("apply mutex is still held after the transaction bound")
	}
	server.runtimeApplyMu.Unlock()
	if state := server.applyState(); state.InProgress {
		t.Fatalf("status still reports an in-progress apply: %+v", state)
	}

	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/status", "", cookie)
	if strings.Contains(status.Body.String(), `"in_progress":true`) {
		t.Fatalf("runtime status still reports an in-progress apply: %s", status.Body.String())
	}

	// The next apply must be accepted rather than rejected forever.
	server.applyWorkOverride = nil
	next := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if next.Code == http.StatusAccepted && strings.Contains(next.Body.String(), `"status":"apply_in_progress"`) {
		t.Fatalf("next apply was rejected as apply_in_progress: %s", next.Body.String())
	}
	if !server.runtimeApplyMu.TryLock() {
		t.Fatal("next apply did not release the apply mutex")
	}
	server.runtimeApplyMu.Unlock()
}

func TestRuntimeApplyRejectsWithTheInFlightTransaction(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	cookie := loginAdmin(t, server)

	lease, acquired := server.beginApply("runtime-apply", "runtime-inflight-test", time.Minute)
	if !acquired {
		t.Fatal("could not acquire the apply gate for the test")
	}
	defer lease.release()

	started := time.Now()
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if time.Since(started) > time.Second {
		t.Fatal("concurrent apply queued instead of returning immediately")
	}
	if response.Code != http.StatusAccepted {
		t.Fatalf("concurrent apply status = %d, want 202: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{`"status":"apply_in_progress"`, `"transaction_id":"runtime-inflight-test"`, `"in_flight_apply":{"in_progress":true`} {
		if !strings.Contains(body, required) {
			t.Fatalf("concurrent apply response missing %q: %s", required, body)
		}
	}
}

func TestRuntimeApplyRejectsReconcilerWithoutPhantomTransaction(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	cookie := loginAdmin(t, server)

	lease, acquired := server.beginApply("proxy reconcile", "", time.Minute)
	if !acquired {
		t.Fatal("could not acquire the apply gate for the test")
	}
	defer lease.release()

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusAccepted {
		t.Fatalf("concurrent apply status = %d, want 202: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{`"status":"apply_in_progress"`, `"kind":"proxy reconcile"`, `"retry_after_seconds":2`} {
		if !strings.Contains(body, required) {
			t.Fatalf("reconciler rejection missing %q: %s", required, body)
		}
	}
	if strings.Contains(body, `"transaction_id"`) {
		t.Fatalf("reconciler rejection invented a phantom transaction: %s", body)
	}
}

func TestProxyReconcilerSkipsWhileOperatorApplyOwnsGate(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	lease, acquired := server.beginApply("runtime-apply", "runtime-operator", time.Minute)
	if !acquired {
		t.Fatal("could not acquire the apply gate for the test")
	}
	defer lease.release()

	done := make(chan error, 1)
	go func() { done <- server.reconcileProxyHealth(context.Background(), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("skipped reconcile returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy reconciler blocked behind an operator apply instead of skipping its cycle")
	}
	if server.runtimeApplyMu.TryLock() {
		server.runtimeApplyMu.Unlock()
		t.Fatal("reconciler released an apply gate it did not own")
	}
}

func TestConfigApplyTimeoutReleasesGateAndReportsFailure(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-config-apply-timeout-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithApplyTimeout(80*time.Millisecond),
		withApplyGrace(50*time.Millisecond),
	)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	server.applyWorkOverride = hungApply(block)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if response.Code != http.StatusAccepted {
		t.Fatalf("timed-out config apply status = %d, want 202: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{`"status":"apply_failed"`, "bound"} {
		if !strings.Contains(body, required) {
			t.Fatalf("timed-out config apply response missing %q: %s", required, body)
		}
	}
	if !server.runtimeApplyMu.TryLock() {
		t.Fatal("apply mutex is still held after the config apply bound")
	}
	server.runtimeApplyMu.Unlock()

	server.applyWorkOverride = nil
	next := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if next.Code == http.StatusAccepted && strings.Contains(next.Body.String(), `"status":"apply_in_progress"`) {
		t.Fatalf("next config apply was rejected as apply_in_progress: %s", next.Body.String())
	}
}

func TestAsyncConfigApplyStopsReportingInProgressAfterBound(t *testing.T) {
	t.Setenv("LY_ROUTE_CONFIG_APPLY_ASYNC", "1")
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-config-apply-async-timeout-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithApplyTimeout(80*time.Millisecond),
		withApplyGrace(50*time.Millisecond),
	)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	server.applyWorkOverride = hungApply(block)
	cookie := loginAdmin(t, server)

	accepted := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if accepted.Code != http.StatusAccepted || !strings.Contains(accepted.Body.String(), `"status":"apply_in_progress"`) {
		t.Fatalf("async config apply status = %d: %s", accepted.Code, accepted.Body.String())
	}
	var payload struct {
		TransactionID string `json:"transaction_id"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &payload); err != nil || payload.TransactionID == "" {
		t.Fatalf("async config apply returned no transaction id: %s (%v)", accepted.Body.String(), err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/apply/status?transaction_id="+payload.TransactionID, "", cookie)
		if status.Code != http.StatusOK {
			t.Fatalf("apply status = %d: %s", status.Code, status.Body.String())
		}
		if strings.Contains(status.Body.String(), `"status":"apply_failed"`) {
			if !strings.Contains(status.Body.String(), "bound") {
				t.Fatalf("timed-out async status has no bound reason: %s", status.Body.String())
			}
			break
		}
		if strings.Contains(status.Body.String(), `"status":"apply_in_progress"`) && time.Now().After(deadline) {
			t.Fatalf("async apply still reports apply_in_progress after its bound: %s", status.Body.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("async apply never reached a terminal status: %s", status.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if server.configApplyInProgress() {
		t.Fatal("configApplyInProgress still true after the bound")
	}
	if !server.runtimeApplyMu.TryLock() {
		t.Fatal("apply mutex is still held after the async apply bound")
	}
	server.runtimeApplyMu.Unlock()
}

func TestConfigApplyStatusRepairsStaleInProgressState(t *testing.T) {
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithApplyTimeout(50*time.Millisecond),
		withApplyGrace(10*time.Millisecond),
	)
	cookie := loginAdmin(t, server)
	server.setConfigApplyState(ConfigApplyAsyncState{
		TransactionID: "txn-stale", Status: "apply_in_progress", RuntimeState: "applying",
		HTTPStatus: http.StatusAccepted, UpdatedAt: time.Now().Add(-time.Hour),
	})

	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/apply/status?transaction_id=txn-stale", "", cookie)
	if status.Code != http.StatusOK {
		t.Fatalf("apply status = %d: %s", status.Code, status.Body.String())
	}
	body := status.Body.String()
	for _, required := range []string{`"status":"apply_failed"`, `"runtime_state":"degraded"`, "bound"} {
		if !strings.Contains(body, required) {
			t.Fatalf("stale apply status missing %q: %s", required, body)
		}
	}
	if server.configApplyInProgress() {
		t.Fatal("configApplyInProgress still true for a stale transaction")
	}
}
