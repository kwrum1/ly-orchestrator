package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type takeoverFixture struct {
	root string
}

func newTakeoverFixture(t *testing.T, iface, pci, driver, carrier string, speed int64, queues bool) takeoverFixture {
	t.Helper()
	root := t.TempDir()
	device := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers", driver)
	netdev := filepath.Join(root, "class/net", iface)
	for _, dir := range []string{device, driverDir, netdev} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driverDir, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
	if queues {
		if err := os.MkdirAll(filepath.Join(netdev, "queues/rx-0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(netdev, "carrier"), []byte(carrier+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if speed > 0 {
		if err := os.WriteFile(filepath.Join(netdev, "speed"), []byte(strings.TrimSpace(jsonNumber(speed))+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", filepath.Join(root, "vpp-native-capabilities.json"))
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	return takeoverFixture{root: root}
}

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (fixture takeoverFixture) writeVerification(t *testing.T, entries string) {
	t.Helper()
	body := `{"schema_version":1,"interfaces":[` + entries + `]}`
	if err := os.WriteFile(filepath.Join(fixture.root, "verification.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture takeoverFixture) writeCapabilities(t *testing.T, driver, pci, reason string) {
	t.Helper()
	body := `{"schema_version":1,"devices":[{"driver":"` + driver + `","pci_address":"` + pci + `","native_unsupported":true,"reason":"` + reason + `"}]}`
	if err := os.WriteFile(filepath.Join(fixture.root, "takeover-capabilities.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInterfaceTakeoverStatusDownLinkIsStillTakenOver(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "vmxnet3", "0", 0, true)
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_packet","link_state":"down","verification":"pending_link","reason":"link down"}`)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})

	if payload["takeover_state"] != "taken_over" {
		t.Fatalf("takeover_state = %v, want taken_over on a down link", payload["takeover_state"])
	}
	if payload["takeover_class"] != "compatibility" {
		t.Fatalf("takeover_class = %v, want compatibility", payload["takeover_class"])
	}
	if payload["takeover_mode"] != "af_packet" {
		t.Fatalf("takeover_mode = %v", payload["takeover_mode"])
	}
	if payload["link_state"] != "down" {
		t.Fatalf("link_state = %v, want down", payload["link_state"])
	}
	if payload["verification"] != "pending_link" {
		t.Fatalf("verification = %v, want pending_link", payload["verification"])
	}
	if payload["management"] != false {
		t.Fatalf("management = %v", payload["management"])
	}
	if payload["pci"] != "0000:13:00.0" {
		t.Fatalf("pci = %v", payload["pci"])
	}
	if payload["reason"] != "" {
		t.Fatalf("reason = %v, want empty for a normal takeover", payload["reason"])
	}
}

func TestInterfaceTakeoverStatusPromotesUpLinkAndDemotesAgain(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "igb", "0", 0, true)
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_xdp_zero_copy","link_state":"down","verification":"pending_link"}`)
	server := &Server{}
	down := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if down["link_state"] != "down" || down["verification"] != "pending_link" || down["takeover_state"] != "taken_over" {
		t.Fatalf("down payload = %#v", down)
	}

	if err := os.WriteFile(filepath.Join(fixture.root, "class/net/eth1/carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_xdp_zero_copy","link_state":"up","verification":"verified","functional_verified":true}`)
	up := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if up["link_state"] != "up" || up["verification"] != "verified" || up["takeover_state"] != "taken_over" {
		t.Fatalf("up payload = %#v", up)
	}

	// Cable down again: the takeover is retained, the proof is weakened.
	if err := os.WriteFile(filepath.Join(fixture.root, "class/net/eth1/carrier"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if again["takeover_state"] != "taken_over" || again["verification"] != "pending_link" || again["link_state"] != "down" {
		t.Fatalf("second down payload = %#v", again)
	}
}

func TestInterfaceTakeoverStatusUnassignedSpareReportsCapabilityWithoutBinding(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth3", "0000:1b:00.0", "igb", "0", 0, true)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth3", map[string]any{}, map[string]bool{})

	if payload["takeover_state"] != "available" {
		t.Fatalf("takeover_state = %v, want available for an unassigned spare", payload["takeover_state"])
	}
	if payload["takeover_mode"] != "af_xdp_copy" {
		t.Fatalf("takeover_mode = %v, want af_xdp_copy", payload["takeover_mode"])
	}
	if payload["takeover_class"] != "native" {
		t.Fatalf("takeover_class = %v", payload["takeover_class"])
	}
	if payload["link_state"] != "down" || payload["verification"] != "pending_link" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["pci"] != "0000:1b:00.0" {
		t.Fatalf("pci = %v", payload["pci"])
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "ownership.json")); !os.IsNotExist(err) {
		t.Fatalf("capability probe created an ownership record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "verification.json")); !os.IsNotExist(err) {
		t.Fatalf("capability probe created a verification record: %v", err)
	}
	driver, err := filepath.EvalSymlinks(filepath.Join(fixture.root, "class/net/eth3/device/driver"))
	if err != nil || filepath.Base(driver) != "igb" {
		t.Fatalf("kernel driver changed: %q, err=%v", driver, err)
	}
}

func TestInterfaceTakeoverStatusRequiresConsentForAFOnly(t *testing.T) {
	newTakeoverFixture(t, "eth4", "0000:13:00.1", "vmxnet3", "0", 0, false)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth4", map[string]any{}, map[string]bool{})

	if payload["takeover_state"] != "consent_required" {
		t.Fatalf("takeover_state = %v, want consent_required", payload["takeover_state"])
	}
	if payload["takeover_mode"] != "af_packet" || payload["takeover_class"] != "compatibility" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["verification"] != "pending_link" {
		t.Fatalf("verification = %v", payload["verification"])
	}
	reason, _ := payload["reason"].(string)
	if !strings.Contains(reason, "AF_PACKET") && !strings.Contains(reason, "兼容模式") {
		t.Fatalf("reason = %q, want a Chinese consent explanation", reason)
	}
}

func TestInterfaceTakeoverStatusManagementNeverTakenOver(t *testing.T) {
	newTakeoverFixture(t, "ens192", "0000:0b:00.0", "vmxnet3", "1", 1000, true)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "ens192", map[string]any{"gateway_role": "management"}, map[string]bool{})

	if payload["management"] != true {
		t.Fatalf("management = %v", payload["management"])
	}
	if payload["takeover_state"] != "unsupported" {
		t.Fatalf("takeover_state = %v, want unsupported for management", payload["takeover_state"])
	}
	if payload["takeover_mode"] != nil || payload["takeover_class"] != nil {
		t.Fatalf("payload = %#v, want null mode/class", payload)
	}
	reason, _ := payload["reason"].(string)
	if reason == "" || !strings.Contains(reason, "管理口") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestInterfaceTakeoverStatusUnsupportedPort(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth9", map[string]any{}, map[string]bool{})

	if payload["takeover_state"] != "unsupported" {
		t.Fatalf("takeover_state = %v", payload["takeover_state"])
	}
	if payload["takeover_mode"] != nil || payload["takeover_class"] != nil {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["verification"] != "failed" {
		t.Fatalf("verification = %v", payload["verification"])
	}
	if reason, _ := payload["reason"].(string); reason == "" {
		t.Fatalf("reason missing")
	}
}

func TestInterfaceTakeoverStatusContractKeysAndNoDriverLeak(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "vmxnet3", "0", 0, true)
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:13:00.0","mode":"af_packet","link_state":"down","verification":"pending_link"}`)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})

	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	expected := []string{"link_speed_mbps", "link_state", "management", "pci", "reason", "takeover_class", "takeover_mode", "takeover_state", "verification"}
	if strings.Join(keys, ",") != strings.Join(expected, ",") {
		t.Fatalf("keys = %v, want %v", keys, expected)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"vmxnet3", "igb", "kernel_driver", "\"driver\"", "kernel", "e1000"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("payload leaked internal driver detail %q: %s", forbidden, encoded)
		}
	}
}

func TestInterfaceTakeoverStatusCrashGuardedPortFallsBackToAF(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth5", "0000:1b:00.0", "vmxnet3", "0", 0, true)
	fixture.writeCapabilities(t, "vmxnet3", "0000:1b:00.0", "set interface state up failed: SIGSEGV")
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth5", map[string]any{}, map[string]bool{})

	if payload["takeover_state"] != "consent_required" {
		t.Fatalf("takeover_state = %v, want consent_required (AF fallback)", payload["takeover_state"])
	}
	if payload["takeover_mode"] != "af_packet" {
		t.Fatalf("takeover_mode = %v", payload["takeover_mode"])
	}
	if payload["link_state"] != "down" {
		t.Fatalf("link_state = %v", payload["link_state"])
	}
}

func TestStripInternalInterfaceFields(t *testing.T) {
	item := map[string]any{
		"id": "eth2", "system_name": "ens256", "pci": "0000:1b:00.0",
		"takeover_state": "taken_over", "takeover_mode": "af_packet",
		"takeover_class": "compatibility", "link_state": "down",
		"verification": "pending_link",
		"driver":       "vmxnet3", "kernel_driver": "vmxnet3", "driver_model": "VMware vmxnet3",
		"active_path": "vpp_native", "work_mode": "vpp_native",
		"vpp_interface": "lyroute-ens256",
		"capability":    map[string]any{"state": "dataplane_locked"},
	}
	stripInternalInterfaceFields(item)
	for _, forbidden := range []string{"driver", "kernel_driver", "driver_model", "active_path", "work_mode", "vpp_interface", "capability"} {
		if _, ok := item[forbidden]; ok {
			t.Fatalf("internal field %q survived sanitization: %#v", forbidden, item)
		}
	}
	for _, required := range []string{"id", "system_name", "pci", "takeover_state", "takeover_mode", "takeover_class", "link_state", "verification"} {
		if _, ok := item[required]; !ok {
			t.Fatalf("public field %q was removed", required)
		}
	}
}
