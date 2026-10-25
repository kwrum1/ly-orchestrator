package httpapi

import (
	"context"
	"strings"

	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// decorateDNSRuntimeCapabilities replaces the legacy factory default
// "SmartDNS process manager not wired" marker with the live service state.
// The defaults remain useful for an unconfigured appliance, but once the
// process manager exists the readback must reflect smartdns health.
func (server *Server) decorateDNSRuntimeCapabilities(ctx context.Context, items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return items
	}
	capability := server.serviceCapability(ctx, serviceRuntime.SmartDNS, "smartdns", "SmartDNS service runtime is not configured")
	for _, item := range items {
		item["degraded"] = !capability.Available
		if capability.Available {
			delete(item, "degraded_reason")
		} else {
			reason := strings.TrimSpace(capability.Reason)
			if reason == "" {
				reason = "SmartDNS service runtime is unavailable"
			}
			item["degraded_reason"] = reason
		}
	}
	return items
}

// decorateDNSUpstreamRuntimeStates replaces the create-time
// "desired_not_applied" placeholder with the committed apply state. A DNS
// upstream is not an apply owner of its own: a WAN-pinned upstream is applied
// as a supplemental VPP DNS service network, and the SmartDNS artifact is
// applied by the services phase. The item must therefore be promoted from the
// committed evidence, not from a field the apply never rewrites.
func (server *Server) decorateDNSUpstreamRuntimeStates(ctx context.Context, items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return items
	}
	result := server.runtimeEvidence()
	committed := runtimeTransactionCommitted(result)
	if !committed {
		return items
	}
	smartdnsRunning := false
	for _, component := range server.statusHealthComponents(ctx) {
		if component.Name == "smartdns" && component.State == "running" && component.Available {
			smartdnsRunning = true
			break
		}
	}
	if !smartdnsRunning {
		return items
	}
	networks := appliedDNSServiceNetworks(result)
	for _, item := range items {
		if !desiredEnabled(item) || stringField(item, "runtime_state") != "desired_not_applied" {
			continue
		}
		id := strings.TrimSpace(stringField(item, "id"))
		if id == "" {
			continue
		}
		wanID := strings.TrimSpace(stringField(item, "wan_egress_id"))
		if wanID != "" {
			network, ok := networks[id]
			if !ok {
				continue
			}
			expected, err := vpp.DNSServiceNetworkForUpstreamID(id, wanID, dnsServiceResolverServers(stringSliceField(item, "servers"), dnsUpstreamBootstrapServers(item)))
			if err != nil {
				continue
			}
			// The in-process plan carries the full network. After a restart only
			// the durable supplemental evidence survives; the TAP identity still
			// encodes upstream id + WAN egress, and the resolver set is verified
			// whenever the plan is available.
			if strings.TrimSpace(network.VPPInterface) != "" && network.VPPInterface != expected.VPPInterface {
				continue
			}
			if len(network.ResolverServers) > 0 && !dnsResolverServersCovered(expected.ResolverServers, network.ResolverServers) {
				continue
			}
		}
		item["runtime_state"] = "applied"
	}
	return items
}

// appliedDNSServiceNetworks reconstructs the committed per-upstream service
// networks. The runtime plan is not persisted in the runtime result, so a
// restart falls back to the durable supplemental readback recorded for the
// service-networks resource.
func appliedDNSServiceNetworks(result RuntimeApplyResult) map[string]vpp.DNSServiceNetwork {
	networks := map[string]vpp.DNSServiceNetwork{}
	if result.GatewayPlan != nil {
		for _, network := range result.GatewayPlan.DNSServiceNetworks {
			if id := strings.TrimSpace(network.UpstreamID); id != "" {
				networks[id] = network
			}
		}
		return networks
	}
	for _, evidence := range result.GatewayEvidence {
		if evidence.Resource != "service-networks" {
			continue
		}
		if evidence.TransactionID != result.TransactionID ||
			evidence.ApplyReceipt.Status != apply.ReceiptApplied ||
			evidence.ApplyReceipt.TransactionID != result.TransactionID {
			continue
		}
		for _, readback := range evidence.SupplementalReadback {
			if readback.Name != "vpp.dns-service.network" {
				continue
			}
			id := strings.TrimSpace(readback.Resource)
			if id == "" {
				continue
			}
			if _, exists := networks[id]; !exists {
				networks[id] = vpp.DNSServiceNetwork{UpstreamID: id}
			}
		}
	}
	return networks
}

func dnsUpstreamBootstrapServers(item map[string]any) []string {
	servers := stringSliceField(item, "bootstrap_servers")
	if len(servers) > 0 {
		return servers
	}
	profile := firstStringField(item, "bootstrap_profile", "bootstrap_dns_profile")
	if profile == "" {
		return nil
	}
	return serviceRuntime.BuiltinDNSBootstrap(profile).BootstrapServers
}

func dnsResolverServersCovered(expected, applied []string) bool {
	if len(expected) == 0 {
		return true
	}
	present := make(map[string]struct{}, len(applied))
	for _, server := range applied {
		present[strings.TrimSpace(server)] = struct{}{}
	}
	for _, server := range expected {
		if _, ok := present[strings.TrimSpace(server)]; !ok {
			return false
		}
	}
	return true
}
