package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func writeDataplaneFakeVPPCTL(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vppctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", path)
	return path
}

func allDataplaneHealth() map[serviceRuntime.ServiceName]serviceRuntime.Health {
	health := map[serviceRuntime.ServiceName]serviceRuntime.Health{}
	for _, service := range []serviceRuntime.ServiceName{
		serviceRuntime.VPP,
		serviceRuntime.SmartDNS,
		serviceRuntime.Dnsmasq,
		serviceRuntime.Xray,
		serviceRuntime.PPPoE,
		serviceRuntime.Nftables,
		serviceRuntime.LinuxRouting,
	} {
		health[service] = serviceRuntime.Health{Service: service, Available: true}
	}
	return health
}

func pinDataplaneHostInventory(t *testing.T, names ...string) {
	t.Helper()
	previous := hostInterfaceInventory
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]any{"id": name, "name": name})
	}
	hostInterfaceInventory = func() []map[string]any { return items }
	t.Cleanup(func() { hostInterfaceInventory = previous })
}

func TestDataplaneReadinessWithoutConfiguredPortsIsReady(t *testing.T) {
	server := &Server{}
	readiness := server.dataplaneReadinessFor(context.Background(), nil)
	if !readiness.Ready() || len(readiness.Configured) != 0 {
		t.Fatalf("readiness without data ports = %#v, want ready", readiness)
	}
	if capability, ok := dataplaneHealthCapability(readiness); ok {
		t.Fatalf("no data ports must not publish a dataplane dependency: %#v", capability)
	}
}

func TestDataplaneReadinessDegradesWhenVPPOwnsNoDataPorts(t *testing.T) {
	writeDataplaneFakeVPPCTL(t, `if [ "$*" = "show interface" ]; then printf 'local0\n'; exit 0; fi
exit 1
`)
	server := &Server{}
	readiness := server.dataplaneReadinessFor(context.Background(), []string{"eth1"})
	if readiness.Ready() {
		t.Fatalf("readiness with zero data ports = %#v, want degraded", readiness)
	}
	if len(readiness.Missing) != 1 || readiness.Missing[0] != "eth1" {
		t.Fatalf("missing = %#v, want [eth1]", readiness.Missing)
	}
	if !strings.Contains(readiness.Reason, "not attached") {
		t.Fatalf("reason = %q", readiness.Reason)
	}
	capability, ok := dataplaneHealthCapability(readiness)
	if !ok || capability.Available || capability.State != "degraded" || capability.Name != "dataplane" {
		t.Fatalf("dataplane health capability = %#v", capability)
	}
	for _, internal := range []string{"probe", "source=", "dataplane_degraded", "runtime_probe"} {
		if strings.Contains(capability.Reason, internal) {
			t.Fatalf("health reason leaks internal code %q: %q", internal, capability.Reason)
		}
	}
}

func TestDataplaneReadinessAcceptsLyrouteAndHardwarePortNames(t *testing.T) {
	writeDataplaneFakeVPPCTL(t, `if [ "$*" = "show interface" ]; then printf 'local0\nlyroute-eth1\nGigabitEthernet0/3/0\n'; exit 0; fi
if [ "$*" = "show hardware-interfaces" ]; then printf 'GigabitEthernet0/3/0  1  up  Intel\n  address 0000:03:00.0\n'; exit 0; fi
exit 1
`)
	state := t.TempDir()
	t.Setenv("LY_ROUTE_DATAPLANE_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, "ownership.json"), []byte(`{"devices":[{"linux_interface":"eth2","pci_address":"0000:03:00.0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// udev-style sysfs is not required: the ownership record's PCI address is
	// enough to find a DPDK port whose VPP name is GigabitEthernet0/3/0.
	server := &Server{}
	readiness := server.dataplaneReadinessFor(context.Background(), []string{"eth1", "eth2"})
	if !readiness.Ready() {
		t.Fatalf("readiness = %#v, want ready", readiness)
	}
	if len(readiness.Realized) != 2 {
		t.Fatalf("realized = %#v", readiness.Realized)
	}
}

func TestWaitForDataplaneReadinessWaitsForRestartingVPP(t *testing.T) {
	pinDataplaneHostInventory(t, "eth1")
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-dataplane-readiness-wait?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "interface", "eth1", map[string]any{"id": "eth1", "interface_id": "eth1", "gateway_role": "lan"}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "vpp-ready")
	t.Setenv("LY_ROUTE_DATAPLANE_READY_MARKER", marker)
	writeDataplaneFakeVPPCTL(t, `if [ "$*" = "show interface" ]; then
  if [ -f "$LY_ROUTE_DATAPLANE_READY_MARKER" ]; then printf 'local0\nlyroute-eth1\n'; else printf 'local0\n'; fi
  exit 0
fi
exit 1
`)
	server := New(WithStore(store), WithClock(fixedClock()))
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("ready"), 0o600)
	}()
	readiness := server.waitForVPPResponsive(ctx, 2*time.Second, 5*time.Millisecond)
	if !readiness.VPPResponding {
		t.Fatalf("VPP responsiveness after restart = %#v, want answering", readiness)
	}
}

func TestWaitForDataplaneReadinessFailsBoundedWhenVPPStaysAbsent(t *testing.T) {
	pinDataplaneHostInventory(t, "eth1")
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-dataplane-readiness-timeout?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "interface", "eth1", map[string]any{"id": "eth1", "interface_id": "eth1", "gateway_role": "lan"}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	writeDataplaneFakeVPPCTL(t, `exit 1
`)
	server := New(WithStore(store), WithClock(fixedClock()))
	started := time.Now()
	readiness := server.waitForVPPResponsive(ctx, 50*time.Millisecond, 5*time.Millisecond)
	if readiness.VPPResponding {
		t.Fatalf("responsiveness = %#v, want VPP down", readiness)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("readiness wait was not bounded: %s", elapsed)
	}
}

func TestHealthDegradesWhenConfiguredDataPortIsNotRealized(t *testing.T) {
	pinDataplaneHostInventory(t, "eth1")
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-health-dataplane?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "interface", "eth1", map[string]any{"id": "eth1", "interface_id": "eth1", "gateway_role": "lan"}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	writeDataplaneFakeVPPCTL(t, `if [ "$*" = "show interface" ]; then printf 'local0\n'; exit 0; fi
exit 1
`)
	controller := &httpServiceController{health: allDataplaneHealth()}
	server := New(
		WithStore(store),
		WithClock(fixedClock()),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/health", "", login.Result().Cookies()[0])
	body := response.Body.String()
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"degraded":true`) {
		t.Fatalf("health did not degrade while VPP owned no data ports: %s", body)
	}
	if !strings.Contains(body, `"name":"dataplane"`) || !strings.Contains(body, `"available":false`) {
		t.Fatalf("health is missing the dataplane dependency: %s", body)
	}
	if !strings.Contains(body, "not attached") {
		t.Fatalf("health dataplane reason is not operator-facing: %s", body)
	}
}

func TestHealthIsOKWhenConfiguredDataPortIsRealized(t *testing.T) {
	pinDataplaneHostInventory(t, "eth1")
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-health-dataplane-ready?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "interface", "eth1", map[string]any{"id": "eth1", "interface_id": "eth1", "gateway_role": "lan"}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	writeDataplaneFakeVPPCTL(t, `if [ "$*" = "show interface" ]; then printf 'local0\nlyroute-eth1\n'; exit 0; fi
exit 1
`)
	controller := &httpServiceController{health: allDataplaneHealth()}
	server := New(
		WithStore(store),
		WithClock(fixedClock()),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/health", "", login.Result().Cookies()[0])
	body := response.Body.String()
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"degraded":false`) {
		t.Fatalf("health did not report ok with a realized data port: %s", body)
	}
	if !strings.Contains(body, `"name":"dataplane"`) || !strings.Contains(body, `"available":true`) {
		t.Fatalf("health is missing the available dataplane dependency: %s", body)
	}
}

func TestDataplaneTakeoverExpectedOnlyUsesCommittedActiveState(t *testing.T) {
	ctx := context.Background()
	pinDataplaneHostInventory(t, "eth1")
	store, err := persistence.Open(ctx, "file:httpapi-dataplane-takeover-expected?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "interface", "eth1", map[string]any{"id": "eth1", "interface_id": "eth1", "gateway_role": "lan"}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	t.Setenv("LY_ROUTE_DATAPLANE_STATE_DIR", stateDir)
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(stateDir, "active.json"))
	server := New(WithStore(store), WithClock(fixedClock()))
	if server.dataplaneTakeoverExpected(ctx) {
		t.Fatal("an ownership/verification record without a committed active state must not trigger the apply readiness wait")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "active.json"), []byte(`{"path":{"tier":"vpp_native","attachments":[{"linux_interface":"eth1"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !server.dataplaneTakeoverExpected(ctx) {
		t.Fatal("a committed active dataplane must trigger the apply readiness wait")
	}
}

func TestHealthIsOKWithoutConfiguredDataPorts(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-health-no-dataplane?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller := &httpServiceController{health: allDataplaneHealth()}
	server := New(
		WithStore(store),
		WithClock(fixedClock()),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/health", "", login.Result().Cookies()[0])
	body := response.Body.String()
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"degraded":false`) {
		t.Fatalf("health without data ports did not report ok: %s", body)
	}
	if strings.Contains(body, `"name":"dataplane"`) {
		t.Fatalf("health without data ports must not publish a dataplane dependency: %s", body)
	}
}
