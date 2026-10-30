package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// lastApplyReader is implemented by the filesystem service controller. It
// lets an unchanged component carry the transaction/time of its last real
// apply instead of pretending the current transaction rewrote it.
type lastApplyReader interface {
	LastApply(context.Context, serviceRuntime.ServiceName) (serviceRuntime.LastApplyRecord, error)
}

func runtimeServiceForComponent(name string) serviceRuntime.ServiceName {
	switch name {
	case "smartdns":
		return serviceRuntime.SmartDNS
	case "dnsmasq":
		return serviceRuntime.Dnsmasq
	case "xray":
		return serviceRuntime.Xray
	case "pppoe":
		return serviceRuntime.PPPoE
	case "vpp":
		return serviceRuntime.VPP
	case "nftables_tproxy":
		return serviceRuntime.Nftables
	case "linux_routing":
		return serviceRuntime.LinuxRouting
	case "ipv6-ra":
		return serviceRuntime.IPv6RA
	default:
		return ""
	}
}

func runtimeAppliedServices(result RuntimeApplyResult) map[serviceRuntime.ServiceName]bool {
	applied := make(map[serviceRuntime.ServiceName]bool, len(result.Applied)+1)
	for _, name := range result.Applied {
		applied[serviceRuntime.ServiceName(name)] = true
	}
	// The typed gateway transaction applies VPP itself. Its artifacts are
	// persisted for reboot replay (persist-only), but the generation still
	// changed this transaction and the gateway evidence is authoritative.
	if len(result.GatewayEvidence) > 0 {
		applied[serviceRuntime.VPP] = true
	}
	return applied
}

func (server *Server) attachServiceEvidence(ctx context.Context, components []RuntimeComponentState, artifacts []serviceRuntime.RenderedArtifact) []RuntimeComponentState {
	return server.attachServiceEvidenceForResult(ctx, components, artifacts, server.runtimeEvidence())
}

func (server *Server) attachServiceEvidenceForResult(ctx context.Context, components []RuntimeComponentState, artifacts []serviceRuntime.RenderedArtifact, evidence RuntimeApplyResult) []RuntimeComponentState {
	applied := runtimeAppliedServices(evidence)
	// The status plan can consume most of the request deadline before this
	// evidence pass starts.  Live readback commands are independently bounded
	// by the runner, so detach them from the already-expired request context;
	// otherwise every component is reported degraded solely because the
	// handler reached it late.
	evidenceCtx := context.WithoutCancel(ctx)
	services := map[string]serviceRuntime.ServiceName{
		"smartdns":        serviceRuntime.SmartDNS,
		"dnsmasq":         serviceRuntime.Dnsmasq,
		"xray":            serviceRuntime.Xray,
		"pppoe":           serviceRuntime.PPPoE,
		"vpp":             serviceRuntime.VPP,
		"nftables_tproxy": serviceRuntime.Nftables,
		"linux_routing":   serviceRuntime.LinuxRouting,
		"ipv6-ra":         serviceRuntime.IPv6RA,
	}
	for index := range components {
		service, exists := services[components[index].Name]
		if !exists {
			continue
		}
		component := &components[index]
		appliedThisTransaction := applied[service]
		// VPP is special: the typed gateway transaction (not the service
		// controller) applied it, so gateway evidence proves this transaction.
		if component.Name == "vpp" && appliedThisTransaction && len(evidence.GatewayEvidence) > 0 && component.State == "running" {
			component.TransactionID = evidence.TransactionID
			component.ApplyReceipt = evidence.Receipt
			component.ApplyReceipt.Capability = "vpp"
			component.ReadbackAt = evidence.Readback.Timestamp
			component.Fresh = evidence.Readback.Fresh
			component.Capability = "vpp"
			component.Unchanged = false
			continue
		}
		serviceArtifacts := artifactsForService(artifacts, service)
		if len(serviceArtifacts) == 0 || !appliedThisTransaction {
			if !appliedThisTransaction && component.State == "running" && component.Available {
				server.markComponentUnchanged(evidenceCtx, component, service)
				continue
			}
			if len(serviceArtifacts) == 0 {
				if evidence.Status == RuntimeStatusCommitted {
					component.State = "not_configured"
					component.Available = false
					component.Reason = ""
				}
				component.ApplyReceipt.Status = apply.ReceiptMissing
				component.Fresh = false
				continue
			}
			// The transaction did not apply this service and live health is
			// not proven; keep the degraded state from the health probe.
			component.Fresh = false
			continue
		}
		if len(serviceArtifacts) == 0 {
			// Applied in this transaction but no staged file (for example a
			// gateway-owned VPP graph): retain the gateway evidence branch above.
			continue
		}
		request := RuntimeEvidenceRequest{TransactionID: evidence.TransactionID, Capability: component.Name, Artifacts: serviceArtifacts}
		receipt, receiptErr := server.runtimeReceipt(evidenceCtx, request)
		readback, readbackErr := server.runtimeReadback(evidenceCtx, request)
		if receiptErr != nil || readbackErr != nil {
			component.State = "degraded"
			component.Available = false
			component.Fresh = false
			component.Reason = strings.TrimSpace(fmt.Sprintf("%v %v", receiptErr, readbackErr))
			continue
		}
		component.TransactionID = evidence.TransactionID
		component.ApplyReceipt = receipt
		component.ReadbackAt = readback.Timestamp
		component.Fresh = readback.Fresh
		component.Unchanged = false
	}
	return components
}

// markComponentUnchanged distinguishes "this transaction did not execute the
// service" from "the service is broken". A successful live probe is enough;
// the last apply identity is attached for traceability.
func (server *Server) markComponentUnchanged(ctx context.Context, component *RuntimeComponentState, service serviceRuntime.ServiceName) {
	component.State = "running"
	component.Available = true
	component.Unchanged = true
	component.Capability = component.Name
	component.Fresh = true
	component.Reason = "unchanged; live health verified"
	component.TransactionID = ""
	component.ReadbackAt = time.Time{}
	component.ApplyReceipt = apply.ApplyReceipt{Capability: component.Name, Status: apply.ReceiptApplied}
	if server.services == nil || server.services.Controller == nil {
		return
	}
	reader, ok := server.services.Controller.(lastApplyReader)
	if !ok {
		return
	}
	record, err := reader.LastApply(ctx, service)
	if err != nil {
		return
	}
	if record.TransactionID != "" {
		component.TransactionID = record.TransactionID
		component.ApplyReceipt.TransactionID = record.TransactionID
		component.ApplyReceipt.AppliedAt = record.AppliedAt
		component.ReadbackAt = record.AppliedAt
		component.Reason = fmt.Sprintf("unchanged since transaction %s at %s; live health verified",
			record.TransactionID, record.AppliedAt.UTC().Format(time.RFC3339))
	}
}
