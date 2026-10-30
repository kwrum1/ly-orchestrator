package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/applystate"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func newApplyStateTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithApplyStatePath(statePath),
		WithVPPReceiptPath(filepath.Join(dir, "vpp-apply-receipt.json")),
		WithClock(fixedClock()),
	)
	return server, statePath
}

func TestRuntimeApplyStateEndpointUnconfigured(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	cookie := loginAdmin(t, server)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/apply/state", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("unconfigured state status = %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false || body["status"] != "unconfigured" {
		t.Fatalf("unconfigured state body = %#v", body)
	}
}

func TestRuntimeApplyStateEndpointReportsAppliedAndFailedGenerations(t *testing.T) {
	server, statePath := newApplyStateTestServer(t)
	store := applystate.NewStore(statePath)
	store.SetClock(fixedClock())
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	receipt := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-one",
		Status:            applystate.StatusApplied,
		DesiredGeneration: 1,
		AppliedGeneration: 1,
		DesiredHash:       "hash-one",
		Steps: []applystate.Step{{
			Command: "resource:interfaces", Status: "applied", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		}},
	}
	if _, err := store.Commit("txn-one", receipt); err != nil {
		t.Fatal(err)
	}
	cookie := loginAdmin(t, server)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/apply/state", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("state status = %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["desired_generation"].(float64) != 1 || body["applied_generation"].(float64) != 1 {
		t.Fatalf("generation body = %#v", body)
	}
	if body["current_generation"].(float64) != 1 || body["transaction_id"] != "txn-one" {
		t.Fatalf("current generation body = %#v", body)
	}
	if body["status"] != applystate.StatusApplied {
		t.Fatalf("status = %#v", body["status"])
	}

	// A changed desired generation with a failed apply must keep the applied
	// generation and expose the failing transaction.
	if _, err := store.Begin("hash-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(applystate.Failure{TransactionID: "txn-two", Reason: "vppctl command failed", RecordStatus: applystate.ReceiptStatusFailed}); err != nil {
		t.Fatal(err)
	}
	response = authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/apply/state", "", cookie)
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["desired_generation"].(float64) != 2 || body["applied_generation"].(float64) != 1 || body["current_generation"].(float64) != 1 {
		t.Fatalf("failed generation body = %#v", body)
	}
	lastError, ok := body["last_error"].(map[string]any)
	if !ok || lastError["transaction_id"] != "txn-two" {
		t.Fatalf("last_error = %#v", body["last_error"])
	}
	if !strings.Contains(lastError["reason"].(string), "txn-two") {
		t.Fatalf("last_error reason does not identify transaction: %#v", lastError)
	}
}

func TestExternalMalformedReceiptFailsApplyState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	receiptPath := filepath.Join(dir, "receipt.json")
	if err := os.WriteFile(receiptPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(
		WithApplyStatePath(statePath),
		WithApplyReceiptPath(receiptPath),
		WithClock(fixedClock()),
	)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	if begin == nil || begin.State.DesiredGeneration != 1 {
		t.Fatalf("begin = %#v", begin)
	}
	_, err = server.commitRuntimeApplyState("txn-invalid", "hash-one", begin, apply.Result{}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("commit error = %v, want invalid receipt", err)
	}
	state := server.failRuntimeApplyState("txn-invalid", err.Error(), applyReceiptFailureStatus(err))
	if state.AppliedGeneration != 0 || state.ReceiptStatus != applystate.ReceiptStatusInvalid {
		t.Fatalf("state after invalid receipt = %#v", state)
	}
}

func TestExternalFailedReceiptWithLiveProcessFailsApplyState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	receiptPath := filepath.Join(dir, "receipt.json")
	receipt := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-failed",
		Status:            applystate.StatusFailed,
		DesiredGeneration: 1,
		AppliedGeneration: 0,
		DesiredHash:       "hash-one",
		ProcessAlive:      true,
		Error:             "vpp is still running but the command failed",
	}
	if err := applystate.WriteReceipt(receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	server := New(
		WithApplyStatePath(statePath),
		WithApplyReceiptPath(receiptPath),
		WithClock(fixedClock()),
	)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.commitRuntimeApplyState("txn-failed", "hash-one", begin, apply.Result{}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("commit error = %v, want failed receipt", err)
	}
	state := server.failRuntimeApplyState("txn-failed", err.Error(), applyReceiptFailureStatus(err))
	if state.AppliedGeneration != 0 || state.CurrentGeneration != 0 || state.Status != applystate.StatusFailed {
		t.Fatalf("live process advanced generation: %#v", state)
	}
}

func TestRuntimeTransactionReceiptMarksBestEffortPartial(t *testing.T) {
	receipt := buildRuntimeTransactionReceipt("txn-partial", "hash-one", 3, apply.Result{}, nil, []apply.CapabilityFailureEvidence{{
		Capability: "dns", Reason: "best-effort route failed",
	}}, true)
	if !receipt.Partial || len(receipt.DegradedReasons) == 0 {
		t.Fatalf("partial receipt = %#v", receipt)
	}
	if receipt.DesiredGeneration != 3 || receipt.AppliedGeneration != 3 || receipt.DesiredHash != "hash-one" {
		t.Fatalf("generation binding = %#v", receipt)
	}
	if err := receipt.Validate(applystate.Expectation{TransactionID: "txn-partial", DesiredGeneration: 3, DesiredHash: "hash-one"}); err != nil {
		t.Fatalf("partial receipt did not validate: %v", err)
	}
}

func TestRuntimeApplyGenerationLifecycleThroughServer(t *testing.T) {
	server, _ := newApplyStateTestServer(t)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	if begin.State.DesiredGeneration != 1 || begin.Unchanged {
		t.Fatalf("first begin = %#v", begin)
	}
	state, err := server.commitRuntimeApplyState("txn-one", "hash-one", begin, apply.Result{}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.DesiredGeneration != 1 {
		t.Fatalf("first commit = %#v", state)
	}
	repeat, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.Unchanged || repeat.State.DesiredGeneration != 1 {
		t.Fatalf("repeat begin = %#v", repeat)
	}
	unchanged, err := server.applyGenerationState.MarkUnchanged("txn-two")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != applystate.StatusUnchanged || unchanged.AppliedGeneration != 1 {
		t.Fatalf("unchanged = %#v", unchanged)
	}
	changed, err := server.beginRuntimeApplyGeneration("hash-two", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	if changed.State.DesiredGeneration != 2 || changed.Unchanged {
		t.Fatalf("changed begin = %#v", changed)
	}
	failedState := server.failRuntimeApplyState("txn-three", "vppctl command failed", applystate.ReceiptStatusFailed)
	if failedState.AppliedGeneration != 1 || failedState.DesiredGeneration != 2 || failedState.Status != applystate.StatusFailed {
		t.Fatalf("failed state = %#v", failedState)
	}
	if failedState.LastError == nil || failedState.LastError.TransactionID != "txn-three" {
		t.Fatalf("failed audit = %#v", failedState.LastError)
	}
}

func TestExternalVPReceiptMustEchoCallerTransactionAndGeneration(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	receiptPath := filepath.Join(dir, "receipt.json")
	server := New(
		WithApplyStatePath(statePath),
		WithApplyReceiptPath(receiptPath),
		WithVPPReceiptPath(receiptPath),
		WithClock(fixedClock()),
	)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	good := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-caller",
		Status:            applystate.StatusApplied,
		DesiredGeneration: 1,
		AppliedGeneration: 1,
		DesiredHash:       "hash-one",
		Steps: []applystate.Step{{
			Command: "set interface state lan0 up", Status: "applied", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		}},
	}
	cases := []struct {
		name   string
		mutate func(*applystate.Receipt)
		want   string
	}{
		{
			name: "vpp-apply did not echo caller generation",
			mutate: func(receipt *applystate.Receipt) {
				receipt.DesiredGeneration = 0
				receipt.AppliedGeneration = 0
			},
			want: "desired_generation receipt=0",
		},
		{
			name: "vpp-apply echoed its own transaction",
			mutate: func(receipt *applystate.Receipt) {
				receipt.TransactionID = "vpp-apply-1793305314-6745"
				receipt.ExecutorTransactionID = receipt.TransactionID
			},
			want: "transaction_id",
		},
		{
			name: "vpp-apply echoed a different hash",
			mutate: func(receipt *applystate.Receipt) {
				receipt.DesiredHash = "other-hash"
			},
			want: "desired_hash",
		},
		{
			name: "partial receipt without reasons",
			mutate: func(receipt *applystate.Receipt) {
				receipt.Partial = true
				receipt.DegradedReasons = nil
			},
			want: "partial=true without degraded_reasons",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			receipt := good
			testCase.mutate(&receipt)
			if err := applystate.WriteReceipt(receiptPath, receipt); err != nil {
				t.Fatal(err)
			}
			_, err := server.commitRuntimeApplyState("txn-caller", "hash-one", begin, apply.Result{}, nil, nil, false)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("commit error = %v, want %q", err, testCase.want)
			}
			state := server.failRuntimeApplyState("txn-caller", err.Error(), applyReceiptFailureStatus(err))
			if state.AppliedGeneration != 0 {
				t.Fatalf("invalid receipt advanced generation: %#v", state)
			}
		})
	}
	// A correctly echoed receipt advances the generation.
	if err := applystate.WriteReceipt(receiptPath, good); err != nil {
		t.Fatal(err)
	}
	state, err := server.commitRuntimeApplyState("txn-caller", "hash-one", begin, apply.Result{}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.Status != applystate.StatusApplied {
		t.Fatalf("valid receipt state = %#v", state)
	}
}

func TestFreshUnboundVPPApplyReceiptCannotBeHiddenBySyntheticSuccess(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	vppReceiptPath := filepath.Join(dir, "vpp-apply-receipt.json")
	server := New(
		WithApplyStatePath(statePath),
		WithVPPReceiptPath(vppReceiptPath),
		WithClock(fixedClock()),
	)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-test")
	if err != nil {
		t.Fatal(err)
	}
	receipt := applystate.Receipt{
		SchemaVersion:         applystate.SchemaVersion,
		TransactionID:         "vpp-apply-1793305314-6745",
		ExecutorTransactionID: "vpp-apply-1793305314-6745",
		Status:                applystate.StatusApplied,
		DesiredGeneration:     0,
		AppliedGeneration:     0,
		DesiredHash:           "",
		Partial:               true,
		DegradedReasons:       []string{"ABF policy needs repair"},
		Steps: []applystate.Step{{
			Command: "abf policy add id 16469", Status: "needs-repair", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
			Reason:       "ABF policy needs repair",
		}},
	}
	if err := applystate.WriteReceipt(vppReceiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(vppReceiptPath, begin.State.UpdatedAt, begin.State.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	_, err = server.commitRuntimeApplyState("runtime-caller", "hash-one", begin, apply.Result{}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "transaction_id") {
		t.Fatalf("commit error = %v, want transaction mismatch", err)
	}
	state := server.failRuntimeApplyState("runtime-caller", err.Error(), applyReceiptFailureStatus(err))
	if state.AppliedGeneration != 0 {
		t.Fatalf("unbound fresh receipt advanced generation: %#v", state)
	}
}

func TestVPPHelperReceiptRequiredWhenAnAppliedArtifactRanTheHelper(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "apply-state.json")
	vppReceiptPath := filepath.Join(dir, "vpp-apply-receipt.json")
	server := New(
		WithApplyStatePath(statePath),
		WithVPPReceiptPath(vppReceiptPath),
		WithClock(fixedClock()),
	)
	begin, err := server.beginRuntimeApplyGeneration("hash-one", "txn-helper")
	if err != nil {
		t.Fatal(err)
	}
	applied := []serviceRuntime.RenderedArtifact{{
		Service: serviceRuntime.VPP,
		Path:    "/var/lib/ly-route/vpp/operations.json",
	}}
	if _, err := server.commitRuntimeApplyState("txn-helper", "hash-one", begin, apply.Result{}, applied, nil, false); err == nil || !strings.Contains(err.Error(), "receipt is missing") {
		t.Fatalf("missing helper receipt error = %v", err)
	}
	state := server.failRuntimeApplyState("txn-helper", "missing helper receipt", applyReceiptFailureStatus(applystate.ErrReceiptMissing))
	if state.AppliedGeneration != 0 {
		t.Fatalf("missing helper receipt advanced generation: %#v", state)
	}
}

func TestRuntimeTransactionReceiptDoesNotMarkDegradedWithoutConcreteFailure(t *testing.T) {
	// A component being non-running is not a partial transaction on its own
	// (for example QoS without its plugin). No failures means no partial flag.
	receipt := buildRuntimeTransactionReceipt("txn-clean", "hash-one", 1, apply.Result{}, nil, nil, true)
	if receipt.Partial || len(receipt.DegradedReasons) != 0 {
		t.Fatalf("receipt invented a degraded reason: %#v", receipt)
	}
}

func TestRuntimeConcreteDegradedReasonsIgnoreUnsupportedCapabilities(t *testing.T) {
	components := []RuntimeComponentState{
		{Name: "qos", State: "unsupported", Reason: "qos plugin is not configured"},
		{Name: "vpp", State: "running", Available: true},
	}
	if reasons := runtimeConcreteDegradedReasons(components, nil, nil); len(reasons) != 0 {
		t.Fatalf("unsupported capability counted as partial: %#v", reasons)
	}

	components = append(components, RuntimeComponentState{Name: "dnsmasq", State: "degraded", Reason: "drop-in is missing"})
	reasons := runtimeConcreteDegradedReasons(components, nil, nil)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "dnsmasq") || !strings.Contains(reasons[0], "drop-in is missing") {
		t.Fatalf("degraded component reason = %#v", reasons)
	}

	steps := []applystate.Step{{
		Command: "set interface state lan0 up", Status: "needs-repair", Attempts: 1,
		Reason:       "interface remained down",
		Compensation: &applystate.Compensation{Status: "not-required"},
	}}
	reasons = runtimeConcreteDegradedReasons(nil, steps, nil)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "lan0") || !strings.Contains(reasons[0], "remained down") {
		t.Fatalf("needs-repair step reason = %#v", reasons)
	}
}

func TestUnchangedReevaluationClearsInheritedPartial(t *testing.T) {
	store := applystate.NewStore(filepath.Join(t.TempDir(), "state", "apply-state.json"))
	store.SetClock(fixedClock())
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	partialReceipt := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-partial",
		Status:            applystate.StatusApplied,
		DesiredGeneration: 1,
		AppliedGeneration: 1,
		DesiredHash:       "hash-one",
		Partial:           true,
		DegradedReasons:   []string{"dnsmasq: drop-in is missing"},
		Steps: []applystate.Step{{
			Command: "artifact:etc/dnsmasq.d", Status: "applied", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		}},
	}
	state, err := store.Commit("txn-partial", partialReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Partial || len(state.DegradedReasons) == 0 {
		t.Fatalf("partial commit = %#v", state)
	}

	repeat, err := store.Begin("hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.Unchanged {
		t.Fatalf("repeat begin = %#v, want unchanged", repeat)
	}
	state, err = store.MarkUnchangedEvaluated("txn-unchanged", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Partial || len(state.DegradedReasons) != 0 {
		t.Fatalf("unchanged inherited degradation: %#v", state)
	}
	if state.Status != applystate.StatusUnchanged || state.AppliedGeneration != 1 {
		t.Fatalf("unchanged state = %#v", state)
	}

	// A live degradation that still exists on the unchanged path is reported.
	state, err = store.MarkUnchangedEvaluated("txn-unchanged-2", true, []string{"vpp: not running"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Partial || len(state.DegradedReasons) != 1 {
		t.Fatalf("live degradation was dropped: %#v", state)
	}
}
