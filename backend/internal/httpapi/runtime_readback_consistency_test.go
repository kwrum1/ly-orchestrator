package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/flow"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// TestRuntimeReadbackPromotesEveryFixtureResourceAfterSettledApply pins the
// Gate A rule: once a runtime transaction has an applied receipt, a degraded
// component (AF_PACKET compatibility) must not leave any configured resource at
// desired_not_applied. It exercises both the runtime/apply shape (degraded) and
// the config/apply shape (committed/running) across every resource type the
// Gate A fixtures configure.
func TestRuntimeReadbackPromotesEveryFixtureResourceAfterSettledApply(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-runtime-readback-consistency?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
	}}
	server := New(WithStore(store), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))

	for _, spec := range []struct {
		resourceType string
		id           string
		body         string
	}{
		{"interface", "wan0", `{"id":"wan0","name":"wan0","interface_id":"wan0","gateway_role":"wan"}`},
		{"interface", "wan1", `{"id":"wan1","name":"wan1","interface_id":"wan1","gateway_role":"wan"}`},
		{"interface", "lan0", `{"id":"lan0","name":"lan0","interface_id":"lan0","gateway_role":"lan","cidr":"192.168.88.1/24"}`},
		{"wan_link", "wan-acc", `{"id":"wan-acc","runtime_state":"desired_not_applied","interface_id":"wan0","enabled":true,"type":"static","gateway":"10.0.1.2","ipv4":{"mode":"static","address":"10.0.0.1/24","gateway":"10.0.1.2"}}`},
		{"route_policy", "route-acc", `{"id":"route-acc","enabled":true,"priority":100,"action":"nat","egress":"wan-acc","match":{"sources":["192.168.88.0/24"],"destinations":["0.0.0.0/0"],"protocols":["any"],"direction":"input"}}`},
		{"wan_group", "group-acc", `{"id":"group-acc","name":"Dual WAN","wan_members":["wan0","wan1"],"mode":"five_tuple"}`},
		{"port_map", "port-acc", `{"id":"port-acc","wan_link":"wan-acc","external_address":"203.0.113.10","external_port":9443,"internal_host":"192.168.88.30","internal_port":9443,"protocol":"tcp","hairpin":false}`},
		{"dhcp_server", "dhcp-acc", `{"id":"dhcp-acc","runtime_state":"desired_not_applied","interface_id":"lan0","subnet":"192.168.88.0/24","pools":["192.168.88.100-192.168.88.199"],"routers":["192.168.88.1"],"name_servers":["192.168.88.1"],"enabled":true}`},
		{"dns_upstream", "dns-acc", `{"id":"dns-acc","runtime_state":"desired_not_applied","enabled":true,"servers":["9.9.9.9"],"wan_egress_id":"wan-acc","interface":"wan0"}`},
		{"traffic_control", "flow-acc", `{"id":"flow-acc","runtime_state":"desired_not_applied","rules":[{"id":"limit-https","granularity":"rule","match":{"sources":["192.168.88.0/24"],"protocols":["tcp"],"dest_ports":["443"],"direction":"both"},"actions":[{"kind":"policer","policer":{"rate_bps":20000000,"burst_bps":2000000}}]}]}`},
	} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(spec.body), &payload); err != nil {
			t.Fatal(err)
		}
		raw, hash, err := persistence.MarshalPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveConfig(ctx, persistence.ConfigDocument{ResourceType: spec.resourceType, ResourceID: spec.id, Payload: raw, PayloadHash: hash, UpdatedAt: fixedClock()().UTC()}); err != nil {
			t.Fatal(err)
		}
	}

	policyConfig, err := server.currentTrafficPolicyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	natConfig, err := server.currentNATConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	flowIntent, err := server.currentFlowIntent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	compiledFlow, err := flow.CompileIntent(flowIntent)
	if err != nil {
		t.Fatal(err)
	}
	plan := vpp.Plan{Policy: policyConfig, NAT: natConfig, Flow: compiledFlow}

	transactionID := "txn-readback-degraded"
	server.runtimeMu.Lock()
	server.lastRuntime = &RuntimeApplyResult{
		Status: "degraded", RuntimeState: "degraded", TransactionID: transactionID,
		Receipt:     apply.ApplyReceipt{TransactionID: transactionID, Capability: "runtime", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()},
		GatewayPlan: &plan,
		GatewayEvidence: []apply.GatewayResourceEvidence{
			{Resource: "routes", TransactionID: transactionID, ApplyReceipt: apply.ApplyReceipt{TransactionID: transactionID, Capability: "routes", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()}, After: vpp.Snapshot{RoutePolicies: policyConfig.RoutePolicies}},
			{Resource: "wan-groups", TransactionID: transactionID, ApplyReceipt: apply.ApplyReceipt{TransactionID: transactionID, Capability: "wan-groups", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()}, After: vpp.Snapshot{WANGroups: policyConfig.WANGroups}},
			{Resource: "port-maps", TransactionID: transactionID, ApplyReceipt: apply.ApplyReceipt{TransactionID: transactionID, Capability: "port-maps", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()}, After: vpp.Snapshot{NAT: natConfig}},
			{Resource: "qos", TransactionID: transactionID, ApplyReceipt: apply.ApplyReceipt{TransactionID: transactionID, Capability: "qos", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()}, After: vpp.Snapshot{QoS: compiledFlow.VPPGroups}},
		},
	}
	server.runtimeMu.Unlock()

	assertConfiguredItemApplied(t, "route policy", "route-acc", server.decorateRoutePolicyRuntimeStates(ctx, mustDesiredItems(t, server, "route_policy")))
	assertConfiguredItemApplied(t, "WAN group", "group-acc", server.decorateWANGroupRuntimeStates(ctx, mustDesiredItems(t, server, "wan_group")))
	assertConfiguredItemApplied(t, "port map", "port-acc", server.decoratePortMapRuntimeStates(ctx, mustDesiredItems(t, server, "port_map")))
	assertConfiguredItemApplied(t, "WAN link", "wan-acc", server.decorateWANLinkRuntimeStates(ctx, mustDesiredItems(t, server, "wan_link")))
	assertConfiguredItemApplied(t, "DHCP server", "dhcp-acc", server.decorateDHCPRuntimeStates(ctx, mustDesiredItems(t, server, "dhcp_server")))
	assertConfiguredItemApplied(t, "DNS upstream", "dns-acc", server.decorateDNSRuntimeCapabilities(ctx, mustDesiredItems(t, server, "dns_upstream")))
	assertConfiguredItemApplied(t, "traffic control", "flow-acc", server.decorateTrafficControlRuntimeStates(ctx, mustDesiredItems(t, server, "traffic_control")))
}

func TestRuntimeTransactionCommittedAcceptsConfigApplyAndRuntimeApplyShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result RuntimeApplyResult
		want   bool
	}{
		{name: "runtime apply degraded", result: RuntimeApplyResult{Status: "degraded", RuntimeState: "degraded", TransactionID: "txn", Receipt: apply.ApplyReceipt{TransactionID: "txn", Status: apply.ReceiptApplied}}, want: true},
		{name: "config apply committed", result: RuntimeApplyResult{Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning, TransactionID: "txn", Receipt: apply.ApplyReceipt{TransactionID: "txn", Status: apply.ReceiptApplied}}, want: true},
		{name: "failed apply", result: RuntimeApplyResult{Status: "apply_failed", RuntimeState: "degraded", TransactionID: "txn", Receipt: apply.ApplyReceipt{TransactionID: "txn", Status: apply.ReceiptApplied}}, want: false},
		{name: "receipt mismatch", result: RuntimeApplyResult{Status: "degraded", TransactionID: "txn", Receipt: apply.ApplyReceipt{TransactionID: "other", Status: apply.ReceiptApplied}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeTransactionCommitted(tc.result); got != tc.want {
				t.Fatalf("runtimeTransactionCommitted = %t, want %t", got, tc.want)
			}
		})
	}
}

func slug(value string) string {
	out := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			out = append(out, char)
		} else if char >= 'A' && char <= 'Z' {
			out = append(out, char+'a'-'A')
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func mustDesiredItems(t *testing.T, server *Server, resourceType string) []map[string]any {
	t.Helper()
	items, err := server.desiredItems(context.Background(), resourceType)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func assertConfiguredItemApplied(t *testing.T, name, id string, items []map[string]any) {
	t.Helper()
	for _, item := range items {
		if stringField(item, "id") != id {
			continue
		}
		state := stringField(item, "runtime_state")
		if state != "applied" && state != RuntimeStateRunning {
			t.Fatalf("%s item %q runtime_state=%q, want applied/running: %#v", name, id, state, item)
		}
		return
	}
	t.Fatalf("%s item %q missing from %#v", name, id, items)
}
