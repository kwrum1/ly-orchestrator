package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

func installIdentityFixture(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_STATE_DIR", root)
	previous := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{
			{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0", "mac": "00:50:56:3a:14:01"},
			{"id": "ens224", "name": "ens224", "pci": "0000:13:00.0", "mac": "00:50:56:3a:14:02"},
			{"id": "ens256", "name": "ens256", "pci": "0000:1b:00.0", "mac": "00:50:56:3a:14:03"},
		}
	}
	t.Cleanup(func() { hostInterfaceInventory = previous })
}

func TestInterfaceIdentityStableAcrossAdoption(t *testing.T) {
	installIdentityFixture(t)
	server := &Server{}
	ctx := context.Background()

	if got := server.canonicalInterfaceID(ctx, "ens224"); got != "eth1" {
		t.Fatalf("ens224 = %q, want eth1", got)
	}
	if got := server.canonicalInterfaceID(ctx, "ens256"); got != "eth2" {
		t.Fatalf("ens256 = %q, want eth2", got)
	}
	if got := server.canonicalInterfaceID(ctx, "0000:1b:00.0"); got != "eth2" {
		t.Fatalf("PCI = %q, want eth2", got)
	}

	// Simulate a DPDK takeover: the two data netdevs disappear, but the
	// persisted inventory still carries their PCI addresses.
	inventory := `{"schema_version":1,"source":"test","management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","mac":"00:50:56:3a:14:02"},{"linux_interface":"ens256","pci":"0000:1b:00.0","mac":"00:50:56:3a:14:03"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"), "inventory.json"), []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	ownership := `{"schema_version":1,"devices":[
		{"pci_address":"0000:13:00.0","linux_interface":"ens224","mac_address":"00:50:56:3a:14:02","kernel_driver":"vmxnet3","transport":"uio","created_by_product":true},
		{"pci_address":"0000:1b:00.0","linux_interface":"ens256","mac_address":"00:50:56:3a:14:03","kernel_driver":"vmxnet3","transport":"uio","created_by_product":true}
	]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"), "ownership.json"), []byte(ownership), 0o600); err != nil {
		t.Fatal(err)
	}
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0"}}
	}
	if got := server.canonicalInterfaceID(ctx, "ens224"); got != "eth1" {
		t.Fatalf("post-adoption ens224 = %q, want eth1", got)
	}
	if got := server.canonicalInterfaceID(ctx, "eth2"); got != "eth2" {
		t.Fatalf("product id changed after adoption: %q", got)
	}
	if got := server.canonicalInterfaceID(ctx, "0000:13:00.0"); got != "eth1" {
		t.Fatalf("post-adoption PCI = %q, want eth1", got)
	}
}

func TestDataplanePolicyCanonicalizesLinuxNamesAndMatchesBothForms(t *testing.T) {
	installIdentityFixture(t)
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-identity-policy?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithClock(fixedClock()))

	if err := server.saveDataplanePolicy(ctx, DataplanePolicy{AFPacketFallback: true, Ports: []string{"ens224", "ens256"}}); err != nil {
		t.Fatal(err)
	}
	policy := server.dataplanePolicy(ctx)
	if strings.Join(policy.Ports, ",") != "eth1,eth2" {
		t.Fatalf("persisted ports = %#v, want canonical eth1,eth2", policy.Ports)
	}
	for _, port := range []string{"eth1", "ens224", "0000:13:00.0"} {
		if !server.allowsAFPacketFor(ctx, policy, port) {
			t.Fatalf("consent did not match %q", port)
		}
	}
	if server.allowsAFPacketFor(ctx, policy, "eth2") && !server.allowsAFPacketFor(ctx, policy, "ens256") {
		t.Fatal("inconsistent matching for ens256/eth2")
	}
	if server.allowsAFPacketFor(ctx, policy, "eth3") {
		t.Fatal("unconsented port matched")
	}
	if got := strings.Join(server.canonicalAFRequiredInterfaces(ctx, []string{"ens224", "ens256"}), ","); got != "eth1,eth2" {
		t.Fatalf("AF error ids = %q, want eth1,eth2", got)
	}
}

func TestColdStartAFConsentUsesProductIDsAndSinglePlanPasses(t *testing.T) {
	installIdentityFixture(t)
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-coldstart-af-consent?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := fixedClock()()
	for _, item := range []struct {
		id   string
		role string
	}{
		{"ens224", "wan"},
		{"ens256", "lan"},
	} {
		document := configDocument(t, "interface", item.id, map[string]any{
			"id": item.id, "interface_id": item.id, "gateway_role": item.role,
		}, now)
		if err := store.SaveConfig(ctx, document); err != nil {
			t.Fatal(err)
		}
	}

	proofPath := filepath.Join(t.TempDir(), "capabilities.json")
	proof := `{"management_interface":"ens192","proofs":[
		{"linux_interface":"ens224","proof":{"tier":"vpp_native","hook":"af_packet","mode":"linux_packet_socket","source":"runtime_probe","runtime_verified":true,"native":true,"high_performance":false,"acceptance_only":true,"observed_at":"2026-06-06T11:59:00Z","valid_until":"2026-06-06T12:01:00Z","performance_score":10,"mac_address":"00:50:56:3a:14:02"}},
		{"linux_interface":"ens256","proof":{"tier":"vpp_native","hook":"af_packet","mode":"linux_packet_socket","source":"runtime_probe","runtime_verified":true,"native":true,"high_performance":false,"acceptance_only":true,"observed_at":"2026-06-06T11:59:00Z","valid_until":"2026-06-06T12:01:00Z","performance_score":10,"mac_address":"00:50:56:3a:14:03"}}
	]}`
	if err := os.WriteFile(proofPath, []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", proofPath)
	server := New(WithStore(store), WithClock(fixedClock()), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	cookie := loginAdmin(t, server)

	interfaces := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/interfaces", "", cookie)
	for _, expected := range []string{`"id":"eth0"`, `"id":"eth1"`, `"id":"eth2"`} {
		if !strings.Contains(interfaces.Body.String(), expected) {
			t.Fatalf("interfaces missing %s: %s", expected, interfaces.Body.String())
		}
	}
	for _, forbidden := range []string{`"id":"ens224"`, `"id":"ens256"`} {
		if strings.Contains(interfaces.Body.String(), forbidden) {
			t.Fatalf("interfaces leaked kernel id %s: %s", forbidden, interfaces.Body.String())
		}
	}

	// Cold start, no adoption, no consent: the plan must still name product ids.
	plan, err := server.buildRuntimePlan(ctx, "coldstart-no-consent")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DataplaneState != "af_fallback_required" {
		t.Fatalf("dataplane state = %q, want af_fallback_required: %#v", plan.DataplaneState, plan.SelectedNativePath)
	}
	if got := strings.Join(plan.AFRequiredInterfaces, ","); got != "eth1,eth2" {
		t.Fatalf("AF required = %q, want eth1,eth2", got)
	}
	for _, port := range plan.AFRequiredInterfaces {
		if strings.HasPrefix(port, "ens") || strings.HasPrefix(port, "enp") {
			t.Fatalf("kernel name in AF payload: %#v", plan.AFRequiredInterfaces)
		}
	}

	// The 409 payload must contain product ids only.
	denied := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), `"ports":["eth1","eth2"]`) {
		t.Fatalf("cold-start apply = %d %s", denied.Code, denied.Body.String())
	}
	if strings.Contains(denied.Body.String(), "ens224") || strings.Contains(denied.Body.String(), "ens256") {
		t.Fatalf("409 leaked kernel names: %s", denied.Body.String())
	}

	// Consent saved with product ids must pass the same gate on the first apply.
	consent := authenticatedJSONRequest(t, server, http.MethodPut, "/api/v1/dataplane/policy", `{"af_packet_fallback":true,"ports":["eth1","eth2"]}`, cookie)
	if consent.Code != http.StatusOK {
		t.Fatalf("consent = %d %s", consent.Code, consent.Body.String())
	}
	allowed, err := server.buildRuntimePlan(ctx, "coldstart-consented")
	if err != nil {
		t.Fatal(err)
	}
	if allowed.DataplaneState == "af_fallback_required" {
		t.Fatalf("consent did not clear the gate: %#v", allowed.AFRequiredInterfaces)
	}
	if len(allowed.SelectedNativePath.Attachments) != 2 || !allowed.SelectedNativePath.Degraded {
		t.Fatalf("selected path = %#v, want two consented AF_PACKET attachments and degraded", allowed.SelectedNativePath)
	}

	// Linux-name consent is still accepted after adoption and is persisted canonically.
	if err := server.saveDataplanePolicy(ctx, DataplanePolicy{AFPacketFallback: true, Ports: []string{"ens256"}}); err != nil {
		t.Fatal(err)
	}
	policy := server.dataplanePolicy(ctx)
	if strings.Join(policy.Ports, ",") != "eth2" {
		t.Fatalf("Linux-name consent persisted as %#v, want eth2", policy.Ports)
	}
	if !server.allowsAFPacketFor(ctx, policy, "ens256") || !server.allowsAFPacketFor(ctx, policy, "eth2") {
		t.Fatal("Linux-name consent did not match the physical port after canonicalization")
	}
	if server.allowsAFPacketFor(ctx, policy, "eth1") || server.allowsAFPacketFor(ctx, policy, "ens224") {
		t.Fatal("unconsented port matched after Linux-name canonicalization")
	}
}
