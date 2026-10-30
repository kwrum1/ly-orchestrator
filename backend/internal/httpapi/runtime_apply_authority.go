package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// runtimeDesiredSourceSchema versions the canonical desired source hash. The
// version is part of the hash so a control-plane upgrade that changes how a
// plan is derived cannot reuse an older committed generation as a no-op.
const runtimeDesiredSourceSchema = "ly-route/desired/v2"

type runtimeDesiredSourceDocument struct {
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Payload      json.RawMessage `json:"payload"`
}

type runtimeDesiredSource struct {
	Schema    string                         `json:"schema"`
	Product   string                         `json:"product"`
	Documents []runtimeDesiredSourceDocument `json:"documents"`
}

// runtimeDesiredSourceHash is the canonical identity of the current desired
// configuration. It deliberately hashes the desired documents, not a live VPP
// observation: an unchanged configuration must keep the same hash across
// probes, while any LAN/WAN/DHCP/DNS edit produces a different hash and
// therefore forces a plan recompute.
func (server *Server) runtimeDesiredSourceHash(ctx context.Context) (string, error) {
	source := runtimeDesiredSource{
		Schema:    runtimeDesiredSourceSchema,
		Product:   server.profile.ID().String(),
		Documents: []runtimeDesiredSourceDocument{},
	}
	if server.store != nil {
		types := managedConfigResourceTypes(server.profile)
		sort.Strings(types)
		for _, resourceType := range types {
			if runtimeDesiredSourceExcluded(resourceType) {
				continue
			}
			documents, err := server.store.Configs(ctx, resourceType)
			if err != nil {
				return "", fmt.Errorf("read desired %s documents: %w", resourceType, err)
			}
			sort.Slice(documents, func(left, right int) bool {
				return documents[left].ResourceID < documents[right].ResourceID
			})
			for _, document := range documents {
				payload, err := canonicalDesiredDocument(document.Payload)
				if err != nil {
					return "", fmt.Errorf("canonicalize desired %s/%s: %w", resourceType, document.ResourceID, err)
				}
				source.Documents = append(source.Documents, runtimeDesiredSourceDocument{
					ResourceType: resourceType,
					ResourceID:   document.ResourceID,
					Payload:      payload,
				})
			}
		}
	}
	_, hash, err := persistence.MarshalPayload(source)
	if err != nil {
		return "", fmt.Errorf("hash desired configuration: %w", err)
	}
	return hash, nil
}

// runtimeDesiredSourceExcluded drops resource types that are runtime
// observations rather than desired state. Including a live DHCP lease would
// change the source hash every time a client renewed and defeat the no-op
// path without changing a single dataplane object.
func runtimeDesiredSourceExcluded(resourceType string) bool {
	switch resourceType {
	case "dhcp_lease":
		return true
	default:
		return false
	}
}

// canonicalDesiredDocument drops write-path bookkeeping fields that do not
// describe the desired dataplane/daemon state. Everybody addressing the same
// configuration therefore produces the same source hash regardless of which
// UI/API version stamped the document.
func canonicalDesiredDocument(payload []byte) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		for _, volatile := range []string{"runtime_state", "capabilities", "current", "updated_at", "created_at"} {
			delete(object, volatile)
		}
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

// runtimeArtifactHashes turns the rendered generation into the durable
// path/content-hash inventory persisted with the commit. It is the artifact
// half of the live-vs-desired proof.
func runtimeArtifactHashes(artifacts []serviceRuntime.RenderedArtifact) map[string]string {
	if len(artifacts) == 0 {
		return nil
	}
	hashes := make(map[string]string, len(artifacts))
	for _, artifact := range artifacts {
		path := strings.TrimSpace(artifact.Path)
		if path == "" {
			continue
		}
		hash := strings.TrimSpace(artifact.ContentHash)
		if hash == "" {
			digest := sha256.Sum256([]byte(artifact.Content))
			hash = hex.EncodeToString(digest[:])
		}
		hashes[path] = hash
	}
	if len(hashes) == 0 {
		return nil
	}
	return hashes
}

// runtimeGenerationCommitCheck is the executor's last gate. It runs after the
// VPP graph and service artifacts were applied but before the snapshot is
// written, so a stale plan or a missing artifact rolls the whole generation
// back instead of being persisted as committed.
func (server *Server) runtimeGenerationCommitCheck(transactionID, sourceHash string, planned []serviceRuntime.RenderedArtifact, applied *[]serviceRuntime.RenderedArtifact) apply.StepFunc {
	return func(ctx context.Context, plan apply.Plan) error {
		if err := server.verifyRuntimeArtifacts(plan, planned, *applied); err != nil {
			return fmt.Errorf("generation %s: %w", transactionID, err)
		}
		if sourceHash != "" {
			current, err := server.runtimeDesiredSourceHash(ctx)
			if err != nil {
				return fmt.Errorf("verify desired source hash: %w", err)
			}
			if current != "" && current != sourceHash {
				supersededApply.Store(true)
				return fmt.Errorf("desired configuration changed during apply (source %s -> %s); refusing to commit a stale plan",
					shortRuntimeHash(sourceHash), shortRuntimeHash(current))
			}
		}
		if err := server.applyLivePPPoEIPv6(ctx); err != nil {
			return fmt.Errorf("PPPoE IPv6 hot apply: %w", err)
		}
		return nil
	}
}

// verifyRuntimeArtifacts requires every artifact that this generation rendered
// to have been successfully applied by this transaction. The service runtime
// already verifies the on-disk content and daemon readback; this check makes a
// missing/inconsistent artifact a transaction failure instead of "committed
// with degraded capabilities" (which left DHCP/DNS/nft stale in the field).
func (server *Server) verifyRuntimeArtifacts(_ apply.Plan, planned, applied []serviceRuntime.RenderedArtifact) error {
	if len(planned) == 0 {
		return nil
	}
	appliedByPath := make(map[string]serviceRuntime.RenderedArtifact, len(applied))
	for _, artifact := range applied {
		appliedByPath[strings.TrimSpace(artifact.Path)] = artifact
	}
	for _, artifact := range planned {
		if !runtimeArtifactRequired(artifact) {
			continue
		}
		path := strings.TrimSpace(artifact.Path)
		actual, ok := appliedByPath[path]
		if !ok {
			return fmt.Errorf("rendered artifact %s (%s) was not applied by this generation; the previous committed generation is still in force",
				path, artifact.Service)
		}
		if actual.ContentHash != artifact.ContentHash || actual.Content != artifact.Content {
			return fmt.Errorf("rendered artifact %s (%s) content differs from the planned generation; refusing to commit",
				path, artifact.Service)
		}
	}
	return nil
}

// runtimeArtifactRequired lists the daemons/dataplane files whose absence
// makes the promised LAN/DHCP/DNS/NAT state untrue. Xray, PPPoE and IPv6 RA
// are asynchronous/optional capabilities: their failures stay degraded
// evidence (and are still enforced by the proxy graph when steering is
// configured), while a missing dnsmasq/nftables/smartdns/VPP artifact can
// never be reported as a committed generation.
func runtimeArtifactRequired(artifact serviceRuntime.RenderedArtifact) bool {
	switch artifact.Service {
	case serviceRuntime.SmartDNS, serviceRuntime.Dnsmasq, serviceRuntime.Nftables,
		serviceRuntime.VPP, serviceRuntime.LinuxRouting:
		return true
	default:
		return false
	}
}

// tryNoopRuntimeApply returns the last committed generation only when all of
// the following are true:
//   - the persisted generation carries a desired source hash,
//   - that hash equals the current desired hash,
//   - the VPP readback and every rendered artifact still match it.
//
// A legacy snapshot without a source hash, a changed desired generation, or
// any live drift falls through to a full plan rebuild. This is the explicit
// anti-wedge rule: an old operations.json/transaction is never replayed.
func (server *Server) tryNoopRuntimeApply(ctx context.Context, sourceHash string) (*RuntimeApplyResult, bool) {
	if server.store == nil || strings.TrimSpace(sourceHash) == "" {
		return nil, false
	}
	snapshots, err := server.store.RuntimeSnapshots(ctx)
	if err != nil || len(snapshots) == 0 {
		return nil, false
	}
	for _, snapshot := range snapshots {
		if !runtimeSnapshotRollbackValid(snapshot, server.now().UTC()) {
			continue
		}
		result, err := runtimeResultFromSnapshot(snapshot)
		if err != nil {
			continue
		}
		if result.Status != RuntimeStatusCommitted || result.RuntimeState != RuntimeStateRunning {
			continue
		}
		persisted := strings.TrimSpace(result.SourceHash)
		if persisted == "" {
			// Legacy evidence cannot prove which desired generation it came
			// from. Recompute once; the next commit stores the source hash.
			log.Printf("runtime apply: latest committed generation has no source hash; rebuilding the plan from desired state")
			return nil, false
		}
		if persisted != sourceHash {
			log.Printf("runtime apply: desired source changed (%s -> %s); rebuilding the plan instead of replaying the old transaction",
				shortRuntimeHash(persisted), shortRuntimeHash(sourceHash))
			return nil, false
		}
		if err := server.verifyCommittedGenerationLive(ctx, result); err != nil {
			log.Printf("runtime apply: live state drifted from the committed generation (%v); rebuilding the plan", err)
			return nil, false
		}
		return &result, true
	}
	return nil, false
}

// verifyCommittedGenerationLive proves that the committed generation is still
// the live one. It is used by the no-op path and by startup reconciliation.
func (server *Server) verifyCommittedGenerationLive(ctx context.Context, result RuntimeApplyResult) error {
	if server.runtimeGenerationVerifier != nil {
		return server.runtimeGenerationVerifier(ctx, result)
	}
	if result.GatewayPlan == nil {
		return nil
	}
	if len(result.ArtifactHashes) == 0 {
		return fmt.Errorf("committed generation has no rendered artifact inventory")
	}
	for path, expected := range result.ArtifactHashes {
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("artifact %s is missing: %w", path, err)
		}
		digest := sha256.Sum256(content)
		if actual := hex.EncodeToString(digest[:]); actual != expected {
			return fmt.Errorf("artifact %s content changed since commit", path)
		}
	}
	if !server.vppReachable(ctx) {
		return fmt.Errorf("VPP is not reachable; the live generation cannot be verified")
	}
	adapter := vpp.Adapter{Client: vpp.NewProductionVPPCTLClient(liveVPPBinary())}
	desired := *result.GatewayPlan
	request := vpp.GatewayDiffSnapshotRequest("verify-"+result.TransactionID, desired, desired)
	live, err := adapter.Snapshot(ctx, request)
	if err != nil {
		return fmt.Errorf("live VPP readback: %w", err)
	}
	for resource := range gatewayPlanResources(desired) {
		if resource == "service-networks" {
			// Supplemental DNS/proxy handoffs are not rows in the typed
			// snapshot; they are validated below from their operation readback.
			continue
		}
		if !vpp.GatewaySnapshotMatchesResource(resource, live, desired) {
			return fmt.Errorf("%s live state differs from the committed plan: %s",
				resource, vpp.DescribeGatewaySnapshotMismatch(resource, live, desired))
		}
	}
	prepared := desired
	prepared.DataplanePrepared = true
	prepared.AllowUnprovenOperations = true
	for _, owner := range []vpp.SupplementalOwner{
		vpp.SupplementalInterfaces,
		vpp.SupplementalServiceNetworks,
		vpp.SupplementalRoutes,
		vpp.SupplementalSecurity,
		vpp.SupplementalQoS,
	} {
		if !vpp.HasSupplementalOperations(prepared, owner) {
			continue
		}
		readback, err := adapter.ReadbackSupplemental(ctx, prepared, owner)
		if err != nil {
			return fmt.Errorf("%s supplemental readback: %w", owner, err)
		}
		if err := vpp.ValidateCommittedSupplementalReadback(prepared, owner, readback, server.now().UTC()); err != nil {
			return fmt.Errorf("%s supplemental readback differs: %w", owner, err)
		}
	}
	return nil
}

func shortRuntimeHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
