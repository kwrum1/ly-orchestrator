package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/flow"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func openAuthorityStore(t *testing.T) *persistence.Store {
	t.Helper()
	ctx := context.Background()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	store, err := persistence.Open(ctx, "file:httpapi-authority-"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func saveAuthorityLAN(t *testing.T, store *persistence.Store, cidr string) {
	t.Helper()
	document := configDocument(t, "interface", "eth3", map[string]any{
		"id": "eth3", "interface_id": "eth3", "gateway_role": "lan", "cidr": cidr,
	}, fixedClock()())
	if err := store.SaveConfig(context.Background(), document); err != nil {
		t.Fatal(err)
	}
}

func saveAuthoritySnapshot(t *testing.T, store *persistence.Store, transactionID, sourceHash string, createdAt time.Time) {
	t.Helper()
	gatewayPlan := vpp.Plan{}
	result := apply.SnapshotPayload{
		Flow:        flow.NewIntent("default", []flow.Rule{flow.NewRule("classify-default", flow.RuleGranularity, flow.Classify("best-effort"))}),
		GatewayPlan: &gatewayPlan,
		SourceHash:  sourceHash,
		Receipt:     apply.ApplyReceipt{TransactionID: transactionID, Capability: "/api/v1/runtime/apply", Status: apply.ReceiptApplied, AppliedAt: createdAt},
		Readback:    apply.Readback{TransactionID: transactionID, Capability: "/api/v1/runtime/apply", Timestamp: createdAt, Fresh: true},
	}
	payload, hash, err := persistence.MarshalPayload(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveApply(context.Background(), persistence.ApplyRecord{Snapshot: persistence.RuntimeSnapshot{
		ID:                  "snapshot-" + transactionID,
		SourceTransactionID: transactionID,
		Payload:             payload,
		PayloadHash:         hash,
		CreatedAt:           createdAt,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDesiredSourceHashIsCanonicalAndTracksLANAddress(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	server := New(WithStore(store), WithClock(fixedClock()))

	empty, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty != repeated {
		t.Fatalf("source hash is not stable: %s != %s", empty, repeated)
	}

	saveAuthorityLAN(t, store, "192.168.88.10/24")
	first, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == empty {
		t.Fatal("LAN document did not change the desired source hash")
	}

	// Write-path bookkeeping must not create a new generation.
	saveAuthorityLAN(t, store, "192.168.88.10/24")
	if same, _ := server.runtimeDesiredSourceHash(ctx); same != first {
		t.Fatalf("unchanged desired state produced a different source hash: %s != %s", same, first)
	}

	saveAuthorityLAN(t, store, "10.1.18.25/24")
	changed, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("changing the LAN address did not change the desired source hash")
	}
}

func saveOrchestratorPolicyGroups(t *testing.T, store *persistence.Store, groups string) {
	t.Helper()
	payload := json.RawMessage(`{"schema_version":1,"ip_objects":[],"policy_groups":` + groups + `,"default":{"kind":"direct"}}`)
	raw, hash, err := persistence.MarshalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	document := persistence.ConfigDocument{
		ResourceType: "orchestrator_policy",
		ResourceID:   "active",
		Payload:      raw,
		PayloadHash:  hash,
		UpdatedAt:    fixedClock()(),
	}
	if err := store.ReplaceConfigsForTypes(context.Background(), []string{"orchestrator_policy"}, []persistence.ConfigDocument{document}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeApplyOrderOnlyPolicyChangeInvalidatesNoop(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenForProduct(ctx, "file:httpapi-orchestrator-policy-order?mode=memory&cache=shared", product.Orchestrator().ID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := newServer(product.Orchestrator(), WithStore(store), WithClock(fixedClock()))
	now := fixedClock()()

	forward := `[{"id":"allow","position":10,"rules":[{"id":"allow-all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"direct"}}]},{"id":"block","position":20,"rules":[{"id":"block-all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"drop"}}]}]`
	reordered := `[{"id":"block","position":10,"rules":[{"id":"block-all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"drop"}}]},{"id":"allow","position":20,"rules":[{"id":"allow-all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"direct"}}]}]`

	saveOrchestratorPolicyGroups(t, store, forward)
	forwardSource, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-forward", forwardSource, now)
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }
	if noop, ok := server.tryNoopRuntimeApply(ctx, forwardSource); !ok || noop.TransactionID != "txn-forward" {
		t.Fatalf("unchanged policy was not reusable: %#v, %v", noop, ok)
	}

	// Reversing only the collection order must produce a new desired
	// generation, so the previous transaction can never satisfy apply.
	saveOrchestratorPolicyGroups(t, store, reordered)
	reorderedSource, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedSource == forwardSource {
		t.Fatal("order-only policy change kept the desired source hash")
	}
	if noop, ok := server.tryNoopRuntimeApply(ctx, reorderedSource); ok {
		t.Fatalf("order-only policy change replayed the old transaction: %#v", noop)
	}
	saveAuthoritySnapshot(t, store, "txn-reordered", reorderedSource, now.Add(time.Minute))
	if noop, ok := server.tryNoopRuntimeApply(ctx, reorderedSource); !ok || noop.TransactionID != "txn-reordered" {
		t.Fatalf("reordered policy did not generate its own apply: %#v, %v", noop, ok)
	}
}

func TestTryNoopRuntimeApplyRefusesOldDesiredAndLiveDrift(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	server := New(WithStore(store), WithClock(fixedClock()))
	now := fixedClock()()

	saveAuthorityLAN(t, store, "192.168.88.10/24")
	firstSource, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-old", firstSource, now)
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }

	noop, ok := server.tryNoopRuntimeApply(ctx, firstSource)
	if !ok || noop.TransactionID != "txn-old" || noop.Status != RuntimeStatusCommitted {
		t.Fatalf("no-op generation = %#v, %v; want committed txn-old", noop, ok)
	}

	// A desired edit must never resolve to the old transaction.
	saveAuthorityLAN(t, store, "10.1.18.25/24")
	changedSource, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := server.tryNoopRuntimeApply(ctx, changedSource); ok {
		t.Fatal("old transaction was replayed after the desired LAN address changed")
	}

	// A live drift with an unchanged desired generation must also rebuild.
	saveAuthoritySnapshot(t, store, "txn-new", changedSource, now.Add(time.Minute))
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error {
		return errors.New("VPP address differs from plan")
	}
	if _, ok := server.tryNoopRuntimeApply(ctx, changedSource); ok {
		t.Fatal("no-op accepted a live generation that drifted from the plan")
	}
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }
	if noop, ok := server.tryNoopRuntimeApply(ctx, changedSource); !ok || noop.TransactionID != "txn-new" {
		t.Fatalf("consistent generation was not reusable: %#v, %v", noop, ok)
	}
}

func TestTryNoopRuntimeApplySelfHealsWithoutOperationsOrSnapshots(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	server := New(WithStore(store), WithClock(fixedClock()))
	saveAuthorityLAN(t, store, "192.168.88.10/24")
	source, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := server.tryNoopRuntimeApply(ctx, source); ok {
		t.Fatal("no-op was accepted without any committed snapshot")
	}
	artifacts, err := serviceRuntime.RenderVPPOperations(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "/var/lib/ly-route/vpp/operations.json" {
		t.Fatalf("empty VPP generation = %#v, want operations.json", artifacts)
	}
	if !strings.Contains(artifacts[0].Content, `"operations": []`) {
		t.Fatalf("empty VPP generation content = %q", artifacts[0].Content)
	}
	hashes := runtimeArtifactHashes(artifacts)
	if hashes["/var/lib/ly-route/vpp/operations.json"] == "" {
		t.Fatal("empty VPP generation was not recorded in the artifact inventory")
	}
}

func TestRuntimeGenerationCommitCheckRejectsMissingArtifactAndStaleSource(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	server := New(WithStore(store), WithClock(fixedClock()))
	saveAuthorityLAN(t, store, "192.168.88.10/24")
	source, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	planned := []serviceRuntime.RenderedArtifact{
		serviceRuntime.NewArtifact(serviceRuntime.Dnsmasq, "/etc/dnsmasq.d/ly-route-dhcp.conf", "desired", "restart"),
		serviceRuntime.NewArtifact(serviceRuntime.Nftables, "/etc/nftables.conf", "flush ruleset", "reload"),
	}
	var applied []serviceRuntime.RenderedArtifact
	check := server.runtimeGenerationCommitCheck("txn-authority", source, planned, &applied)
	if err := check(ctx, apply.Plan{}); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("missing artifact check = %v, want not-applied failure", err)
	}

	applied = append([]serviceRuntime.RenderedArtifact(nil), planned...)
	if err := check(ctx, apply.Plan{}); err != nil {
		t.Fatalf("consistent artifacts were rejected: %v", err)
	}

	// A desired edit while the transaction is executing must fail closed.
	saveAuthorityLAN(t, store, "10.1.18.25/24")
	if err := check(ctx, apply.Plan{}); err == nil || !strings.Contains(err.Error(), "desired configuration changed during apply") {
		t.Fatalf("stale source check = %v, want changed-during-apply failure", err)
	}
}

func TestReconcileRuntimeDegradesWhenDesiredChangedSinceCommit(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	server := New(WithStore(store), WithClock(fixedClock()))
	saveAuthorityLAN(t, store, "192.168.88.10/24")
	source, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-committed", source, fixedClock()())
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }

	server.reconcileRuntime(ctx)
	if got := server.runtimeEvidence(); got.Status != RuntimeStatusCommitted {
		t.Fatalf("matching desired generation reconciled as %#v, want committed", got)
	}

	saveAuthorityLAN(t, store, "10.1.18.25/24")
	server.reconcileRuntime(ctx)
	// The committed generation stays authoritative; desired drift is display
	// state, not a replacement apply result.
	got := server.runtimeEvidence()
	if got.TransactionID != "txn-committed" || got.Status != RuntimeStatusCommitted {
		t.Fatalf("startup drift replaced the committed generation: %#v", got)
	}
	notice := server.lastReconciliationNotice()
	if notice == nil || !strings.Contains(notice.Reason, "desired configuration changed") {
		t.Fatalf("reconcile notice = %#v", notice)
	}
	status := request(t, server, http.MethodGet, "/api/v1/runtime/status")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"degraded"`) || !strings.Contains(status.Body.String(), `"last_reconcile"`) || !strings.Contains(status.Body.String(), `"transaction_id":"txn-committed"`) {
		t.Fatalf("status did not separate committed generation from reconcile notice: %d %s", status.Code, status.Body.String())
	}
}

func newAuthorityApplyServer(t *testing.T, store *persistence.Store, gateway apply.GatewayTransactionRunner) *Server {
	t.Helper()
	saveExplicitDataInterface(t, store, "eth3")
	useVPPProof(t, "eth0")
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.VPP:      {Service: serviceRuntime.VPP, Available: true},
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
	}}
	return New(WithStore(store), WithClock(fixedClock()), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithGatewayTransaction(gateway))
}

func TestRuntimeApplyAfterReconcileRebuildsAndCommitsNewGeneration(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	saveAuthorityLAN(t, store, "192.0.2.2/24")
	seed := New(WithStore(store), WithClock(fixedClock()))
	oldSource, err := seed.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-old", oldSource, fixedClock()().Add(-time.Minute))

	server := newAuthorityApplyServer(t, store, &traceGatewayTransaction{trace: &[]string{}})
	// The desired generation changes after the old commit, then the control
	// plane restarts and marks the drift. That notice must not become the
	// apply result or suppress the rebuild.
	saveAuthorityLAN(t, store, "10.1.18.25/24")
	server.reconcileRuntime(ctx)
	if got := server.runtimeEvidence(); got.TransactionID != "txn-old" || got.Status != RuntimeStatusCommitted {
		t.Fatalf("reconcile replaced committed generation: %#v", got)
	}
	if server.lastReconciliationNotice() == nil {
		t.Fatal("missing reconcile notice for changed desired state")
	}
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	txn := extractTransactionID(t, response.Body.String())
	if txn == "" || txn == "txn-old" || strings.Contains(response.Body.String(), `"status":"reconciled_degraded"`) {
		t.Fatalf("apply returned the old reconciled generation: %d %s", response.Code, response.Body.String())
	}
	snapshots, err := store.RuntimeSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].SourceTransactionID != txn {
		t.Fatalf("new generation was not committed: %#v", snapshots)
	}
	var payload apply.SnapshotPayload
	if err := json.Unmarshal(snapshots[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	wantSource, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if payload.SourceHash != wantSource || payload.SourceHash == oldSource {
		t.Fatalf("committed source hash = %q, want %q (old %q)", payload.SourceHash, wantSource, oldSource)
	}

	// A second apply with the same desired and a verified live generation is a
	// no-op, but must still answer committed.
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }
	second := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"status":"committed"`) || !strings.Contains(second.Body.String(), `"noop":true`) || extractTransactionID(t, second.Body.String()) != txn {
		t.Fatalf("second apply was not a committed no-op: %d %s", second.Code, second.Body.String())
	}
}

func TestRuntimeApplyFailureAfterReconcileReportsItsOwnReason(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	saveAuthorityLAN(t, store, "192.0.2.2/24")
	seed := New(WithStore(store), WithClock(fixedClock()))
	oldSource, err := seed.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-old", oldSource, fixedClock()().Add(-time.Minute))
	server := newAuthorityApplyServer(t, store, failingGatewayTransaction{})
	saveAuthorityLAN(t, store, "10.1.18.25/24")
	server.reconcileRuntime(ctx)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"apply_failed"`) || !strings.Contains(response.Body.String(), "middle gateway resource failed") {
		t.Fatalf("apply failure response = %d %s", response.Code, response.Body.String())
	}
	status := request(t, server, http.MethodGet, "/api/v1/runtime/status")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), "middle gateway resource failed") || strings.Contains(status.Body.String(), "desired configuration changed since") {
		t.Fatalf("status still exposed the reconcile notice instead of the failed apply: %d %s", status.Code, status.Body.String())
	}
	if strings.Contains(status.Body.String(), `"transaction_id":"txn-old","status":"degraded"`) {
		t.Fatalf("status replaced the committed generation with the failed attempt: %s", status.Body.String())
	}
}

func TestRuntimeApplyPlanFailureAfterReconcileReturnsCompileFailed(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	saveAuthorityLAN(t, store, "192.0.2.2/24")
	seed := New(WithStore(store), WithClock(fixedClock()))
	oldSource, err := seed.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-old", oldSource, fixedClock()().Add(-time.Minute))
	server := newAuthorityApplyServer(t, store, &traceGatewayTransaction{trace: &[]string{}})
	// The route source is outside every configured LAN, so plan compilation
	// must fail instead of reusing the old committed generation.
	document := configDocument(t, "route_policy", "bad-route", map[string]any{
		"id": "bad-route", "priority": float64(100), "action": "route",
		"match": map[string]any{"sources": []any{"10.0.0.0/24"}, "destinations": []any{"0.0.0.0/0"}},
	}, fixedClock()())
	if err := store.SaveConfig(ctx, document); err != nil {
		t.Fatal(err)
	}
	server.reconcileRuntime(ctx)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"compile_failed"`) || !strings.Contains(response.Body.String(), "route") {
		t.Fatalf("plan failure response = %d %s", response.Code, response.Body.String())
	}
	status := request(t, server, http.MethodGet, "/api/v1/runtime/status")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), "route") || strings.Contains(status.Body.String(), "desired configuration changed since") {
		t.Fatalf("status lost the compile failure reason: %d %s", status.Code, status.Body.String())
	}
}

func TestRuntimeApplyNoopReturnsCommittedWithoutRebuilding(t *testing.T) {
	ctx := context.Background()
	store := openAuthorityStore(t)
	saveAuthorityLAN(t, store, "192.168.88.10/24")
	server := New(WithStore(store), WithClock(fixedClock()), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	source, err := server.runtimeDesiredSourceHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveAuthoritySnapshot(t, store, "txn-noop", source, fixedClock()())
	server.runtimeGenerationVerifier = func(context.Context, RuntimeApplyResult) error { return nil }
	cookie := loginAdmin(t, server)

	started := time.Now()
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("no-op apply took %s, want an immediate committed response", elapsed)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("no-op apply = %d %s", response.Code, response.Body.String())
	}
	for _, required := range []string{`"status":"committed"`, `"noop":true`, `"transaction_id":"txn-noop"`, `"source_hash":"` + source + `"`} {
		if !strings.Contains(response.Body.String(), required) {
			t.Fatalf("no-op response missing %q: %s", required, response.Body.String())
		}
	}
}
