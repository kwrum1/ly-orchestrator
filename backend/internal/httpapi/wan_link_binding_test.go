package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// A WAN link bound to a port that is not in the device's published inventory
// must be rejected at write time. Accepting it persists an unassignable line and
// the next apply fails closed (dataplane_locked/runtime_capability_proof), which
// is the "NIC binding cannot be saved" symptom operators report.
func TestWANLinkBindingRejectsPortOutsideOperatorInventory(t *testing.T) {
	server, cookie := inventoryTestServer(t)
	ports := observedPorts(t, server, 3)
	// Plan B: a WAN line references the product id; the kernel netdev stays
	// available as the internal system_name attribute.
	wantSecond := server.canonicalInterfaceID(context.Background(), ports[2])

	// Publish exactly the ports this host observed, so the inventory is
	// authoritative for this test without depending on real NIC names.
	root := t.TempDir()
	for index, port := range ports {
		writeSysfsPort(t, root, port, fmt.Sprintf("00:50:56:cc:00:%02d", index))
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", writeInventoryFixture(t, fmt.Sprintf(`{
		"schema_version": 1,
		"management": {"linux_interface": %q, "mac": "00:50:56:cc:00:00", "role": "management"},
		"data_interfaces": [
			{"linux_interface": %q, "mac": "00:50:56:cc:00:01", "role": "data"},
			{"linux_interface": %q, "mac": "00:50:56:cc:00:02", "role": "data"}
		]
	}`, ports[0], ports[1], ports[2])))

	unknown := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		`{"id":"uiacc-wan-unknown","interface_id":"uiacc-no-such-port","wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, cookie)
	if unknown.Code != http.StatusUnprocessableEntity || !strings.Contains(unknown.Body.String(), "not present in this device's port inventory") {
		t.Fatalf("unknown port status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	management := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		fmt.Sprintf(`{"id":"uiacc-wan-mgmt","interface_id":%q,"wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, ports[0]), cookie)
	if management.Code != http.StatusUnprocessableEntity || !strings.Contains(management.Body.String(), "cannot be assigned to a WAN link") {
		t.Fatalf("management port status=%d body=%s", management.Code, management.Body.String())
	}

	accepted := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		fmt.Sprintf(`{"id":"uiacc-wan-real","interface_id":%q,"wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"},"display_name":"stale"}`, ports[1]), cookie)
	if accepted.Code != http.StatusOK {
		t.Fatalf("inventory port rejected: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if body := accepted.Body.String(); strings.Contains(body, `"display_name":"stale"`) {
		t.Fatalf("display_name kept a stale binding: %s", body)
	}
	firstDisplay := jsonField(t, accepted.Body.String(), "display_name")

	// Re-binding an existing line to another inventory port must persist, and the
	// readback must echo the new port (the reported "save does nothing" bug).
	rebound := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/gateway/wan-links/uiacc-wan-real",
		fmt.Sprintf(`{"id":"uiacc-wan-real","interface_id":%q,"wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, ports[2]), cookie)
	if rebound.Code != http.StatusOK {
		t.Fatalf("rebind status=%d body=%s", rebound.Code, rebound.Body.String())
	}
	if body := rebound.Body.String(); !strings.Contains(body, fmt.Sprintf(`"interface_id":%q`, wantSecond)) {
		t.Fatalf("rebind readback did not follow the new port: %s", body)
	}
	readback := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/gateway/wan-links/uiacc-wan-real", "", cookie)
	if body := readback.Body.String(); !strings.Contains(body, fmt.Sprintf(`"interface_id":%q`, wantSecond)) {
		t.Fatalf("readback after rebind: %s", body)
	}
	if secondDisplay := jsonField(t, readback.Body.String(), "display_name"); secondDisplay == "" || secondDisplay == firstDisplay {
		t.Fatalf("display_name did not follow the new port: first=%q second=%q", firstDisplay, secondDisplay)
	}

	// A line rebound to a port outside the inventory is rejected as well, so the
	// stored line cannot be made unassignable through an update.
	badRebind := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/gateway/wan-links/uiacc-wan-real",
		`{"id":"uiacc-wan-real","interface_id":"uiacc-no-such-port","wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, cookie)
	if badRebind.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rebind to unknown port status=%d body=%s", badRebind.Code, badRebind.Body.String())
	}
}

// Without an inventory that describes this host the check must stay out of the
// way: developer and CI hosts cannot know the hardware.
func TestWANLinkBindingWithoutAuthoritativeInventoryKeepsTestPorts(t *testing.T) {
	server, cookie := inventoryTestServer(t)
	ports := observedPorts(t, server, 1)
	root := t.TempDir()
	writeSysfsPort(t, root, ports[0], "00:50:56:bb:00:00")
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	// Stale inventory from another machine: its management port does not exist here.
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", writeInventoryFixture(t, `{
		"schema_version": 1,
		"management": {"linux_interface": "uiacc-absent-mgmt", "role": "management"},
		"data_interfaces": [{"linux_interface": "uiacc-absent-data", "role": "data"}]
	}`))
	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		`{"id":"uiacc-wan-ci","interface_id":"eth7","wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("test host port rejected: status=%d body=%s", created.Code, created.Body.String())
	}
}

func observedPorts(t *testing.T, server *Server, minimum int) []string {
	t.Helper()
	seen := map[string]bool{}
	ports := []string{}
	for _, identity := range server.interfaceIdentities(context.Background()) {
		name := strings.TrimSpace(identity.LinuxInterface)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		ports = append(ports, name)
	}
	if len(ports) < minimum {
		t.Skipf("host exposes %d netdevs, need %d", len(ports), minimum)
	}
	return ports[:minimum]
}

func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	needle := `"` + field + `":"`
	start := strings.Index(body, needle)
	if start < 0 {
		return ""
	}
	rest := body[start+len(needle):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func writeSysfsPort(t *testing.T, root, name, mac string) {
	t.Helper()
	dir := filepath.Join(root, "class", "net", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create sysfs port: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "address"), []byte(mac+"\n"), 0o644); err != nil {
		t.Fatalf("write mac: %v", err)
	}
}

func writeInventoryFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dataplane-inventory.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return path
}

func inventoryTestServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	store, err := persistence.Open(context.Background(), "file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithClock(fixedClock()))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
	}
	return server, login.Result().Cookies()[0]
}
