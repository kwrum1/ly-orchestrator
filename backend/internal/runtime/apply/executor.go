package apply

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/selfheal"
	"ly-route/backend/internal/runtime/vpp"
)

type Phase string

const (
	PhaseValidate    Phase = "validate"
	PhaseCompile     Phase = "compile"
	PhaseSnapshot    Phase = "snapshot"
	PhaseApply       Phase = "apply"
	PhaseHealthCheck Phase = "health-check"
	PhaseReadback    Phase = "readback"
	PhaseCommit      Phase = "commit"
	PhaseRollback    Phase = "rollback"
)

const (
	StatusSuccess  = "success"
	StatusFailure  = "failure"
	StatusRollback = "rollback"
)

type StepFunc func(context.Context, Plan) error

type Clock func() time.Time

type Executor struct {
	Store *persistence.Store
	// BeforeGateway runs the small set of host-side handoff artifacts that
	// must exist before VPP validates service-chain next hops.  The gateway
	// graph itself is still committed atomically immediately afterwards.
	BeforeGateway      StepFunc
	Apply              StepFunc
	Receipt            ReceiptFunc
	HealthCheck        StepFunc
	Readback           ReadbackFunc
	Rollback           StepFunc
	Gateway            GatewayTransactionRunner
	Now                Clock
	ApplyMu            *sync.Mutex
	CapabilityFailures func() []CapabilityFailureEvidence
	// BeforeCommit is the last gate before a generation is persisted. It is
	// where the caller proves that the plan it executed still belongs to the
	// current desired state and that every rendered artifact is live. A
	// failure rolls the whole transaction back instead of writing a committed
	// snapshot that the next apply would treat as authoritative.
	BeforeCommit StepFunc
}

type Request struct {
	// GenerationHash is the identity of the desired generation the apply is
	// committing. It is stored with the snapshot and republished on restart.
	GenerationHash     string
	TransactionID      string
	Actor              string
	Role               string
	Resource           string
	ProxyEgress        proxy.Egress
	FlowIntent         flow.Intent
	SnapshotID         string
	PreviousSnapshotID string
	RollbackID         string
	GatewayPlan        vpp.Plan
	Hooks              *ApplyHooks
	// SourceHash is the canonical hash of the desired configuration this plan
	// was compiled from. It is persisted with the committed generation so a
	// later apply can prove whether its plan is still authoritative instead of
	// replaying an old operation set.
	SourceHash string
	// ArtifactHashes maps every rendered artifact path to the content hash
	// committed in this generation. A no-op/restart check uses it to prove the
	// files on disk are the ones this generation applied.
	ArtifactHashes map[string]string
}

type Plan struct {
	Request       Request
	CompiledProxy proxy.CompiledEgress
	CompiledFlow  flow.CompiledIntent
	SnapshotHash  string
	Previous      PreviousState
	GatewayPlan   vpp.Plan
	// Hooks carry per-transaction service steps into the gateway dependency
	// graph. They are not persisted.
	Hooks *ApplyHooks
}

type PreviousState struct {
	Available    bool
	ProxyEgress  proxy.Egress
	FlowIntent   flow.Intent
	GatewayPlan  *vpp.Plan
	SnapshotHash string
	SourceHash   string
}

type Result struct {
	Plan            Plan
	Events          []persistence.AuditEvent
	Rollback        persistence.RollbackMetadata
	RollbackReceipt RollbackReceipt
	Receipt         ApplyReceipt
	Readback        Readback
	GatewayResult   GatewayTransactionResult
}

func (e Executor) Run(ctx context.Context, request Request) (Result, error) {
	if e.ApplyMu != nil {
		e.ApplyMu.Lock()
		defer e.ApplyMu.Unlock()
	}
	if e.Store == nil {
		return Result{}, fmt.Errorf("apply executor requires persistence store")
	}
	now := e.now
	events := recorder{request: request, now: now}

	// An empty proxy egress is the deleted/unconfigured state.  It is a valid
	// generation (no proxy steering and no service network), not an invalid
	// egress: the built-in default egress is hidden by a soft-deleted document
	// and must not force every later apply to fail.
	proxyConfigured := strings.TrimSpace(request.ProxyEgress.ID) != ""
	if proxyConfigured {
		if err := proxy.ValidateEgress(request.ProxyEgress); err != nil {
			events.add(PhaseValidate, StatusFailure, "", "", err)
			return e.persistAuditFailure(ctx, events.events, err)
		}
	}
	if err := flow.ValidateIntent(request.FlowIntent); err != nil {
		events.add(PhaseValidate, StatusFailure, "", "", err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	events.add(PhaseValidate, StatusSuccess, "", "", nil)

	var compiledProxy proxy.CompiledEgress
	if proxyConfigured {
		var err error
		compiledProxy, err = proxy.CompileEgress(request.ProxyEgress)
		if err != nil {
			events.add(PhaseCompile, StatusFailure, "", "", err)
			return e.persistAuditFailure(ctx, events.events, err)
		}
	}
	compiledFlow, err := flow.CompileIntent(request.FlowIntent)
	if err != nil {
		events.add(PhaseCompile, StatusFailure, "", "", err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	compiledPayload := struct {
		Proxy proxy.CompiledEgress `json:"proxy"`
		Flow  flow.CompiledIntent  `json:"flow"`
	}{Proxy: compiledProxy, Flow: compiledFlow}
	_, compiledHash, err := persistence.MarshalPayload(compiledPayload)
	if err != nil {
		events.add(PhaseCompile, StatusFailure, "", "", err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	events.add(PhaseCompile, StatusSuccess, "", compiledHash, nil)

	snapshotPayload := SnapshotPayload{
		Proxy:          request.ProxyEgress,
		Flow:           request.FlowIntent,
		GatewayPlan:    &request.GatewayPlan,
		GenerationHash: request.GenerationHash,
		SourceHash:     request.SourceHash,
		ArtifactHashes: cloneArtifactHashes(request.ArtifactHashes),
	}
	snapshotJSON, snapshotHash, err := persistence.MarshalPayload(snapshotPayload)
	if err != nil {
		events.add(PhaseSnapshot, StatusFailure, "", "", err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	events.add(PhaseSnapshot, StatusSuccess, "", snapshotHash, nil)

	previous, err := e.previousState(ctx, request.PreviousSnapshotID)
	if err != nil {
		events.add(PhaseSnapshot, StatusFailure, "", snapshotHash, err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	if e.Gateway != nil && previous.Available && previous.GatewayPlan == nil {
		err := &LegacyGatewayPlanError{SnapshotID: request.PreviousSnapshotID}
		events.add(PhaseSnapshot, StatusFailure, snapshotHash, snapshotHash, err)
		return e.persistAuditFailure(ctx, events.events, err)
	}
	plan := Plan{Request: request, CompiledProxy: compiledProxy, CompiledFlow: compiledFlow, SnapshotHash: snapshotHash, Previous: previous, GatewayPlan: request.GatewayPlan, Hooks: request.Hooks}
	if e.BeforeGateway != nil {
		if err := e.BeforeGateway(ctx, plan); err != nil {
			return e.fail(ctx, plan, events, snapshotHash, PhaseApply, err)
		}
	}
	gatewayResult, err := e.gateway(ctx, plan)
	if err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseApply, err)
	}
	// VPP owns production data interfaces. Apply the gateway graph first so
	// Linux control-plane peers (Dnsmasq, SmartDNS, PPPoE and Xray) can bind to
	// their committed LCP/TAP handoff instead of racing a nonexistent netdev.
	if err := e.apply(ctx, plan); err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseApply, err)
	}
	receipt, err := e.receipt(ctx, plan)
	if err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseApply, err)
	}
	if err := receipt.validate(request, now()); err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseApply, err)
	}
	events.add(PhaseApply, StatusSuccess, snapshotHash, compiledHash, nil)

	if err := e.healthCheck(ctx, plan); err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseHealthCheck, err)
	}
	events.add(PhaseHealthCheck, StatusSuccess, compiledHash, compiledHash, nil)
	readback, err := e.readback(ctx, plan)
	if err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseReadback, err)
	}
	if err := readback.validate(request, now()); err != nil {
		return e.fail(ctx, plan, events, snapshotHash, PhaseReadback, err)
	}
	events.add(PhaseReadback, StatusSuccess, compiledHash, compiledHash, nil)
	// This is the atomic commit gate. Everything before it is rollbackable;
	// nothing after it is allowed to turn the generation into a failure.
	if e.BeforeCommit != nil {
		if err := e.BeforeCommit(ctx, plan); err != nil {
			return e.fail(ctx, plan, events, snapshotHash, PhaseCommit, err)
		}
	}
	snapshotPayload.Receipt = receipt
	snapshotPayload.Readback = readback
	snapshotPayload.GatewayEvidence = gatewayResult.Evidence
	if e.CapabilityFailures != nil {
		snapshotPayload.CapabilityFailures = append([]CapabilityFailureEvidence(nil), e.CapabilityFailures()...)
	}
	snapshotPayload.SelfHealed = selfheal.Items(ctx)
	snapshotJSON, snapshotHash, err = persistence.MarshalPayload(snapshotPayload)
	if err != nil {
		return e.fail(ctx, plan, events, plan.SnapshotHash, PhaseSnapshot, err)
	}
	plan.SnapshotHash = snapshotHash
	for index := range events.events {
		if events.events[index].Action == string(PhaseSnapshot) {
			events.events[index].AfterHash = snapshotHash
			break
		}
	}
	events.add(PhaseCommit, StatusSuccess, snapshotHash, compiledHash, nil)

	record := persistence.ApplyRecord{
		Snapshot:    persistence.RuntimeSnapshot{ID: request.SnapshotID, SourceTransactionID: request.TransactionID, Payload: snapshotJSON, PayloadHash: snapshotHash, CreatedAt: now()},
		AuditEvents: events.events,
	}
	if err := e.Store.SaveApply(ctx, record); err != nil {
		return Result{Plan: plan, Events: events.events}, err
	}
	return Result{Plan: plan, Events: events.events, Receipt: receipt, Readback: readback, GatewayResult: gatewayResult}, nil
}

func cloneArtifactHashes(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for path, hash := range source {
		clone[path] = hash
	}
	return clone
}
