package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/vpp"
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
	t.Setenv("LY_ROUTE_VPP_VERSION", "test-vpp")
	t.Setenv("LY_ROUTE_VPP_RUNTIME_ID", "test-runtime")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")
	environment := (&Server{}).capabilityEnvironmentFor(pci, driver)
	body := fmt.Sprintf(`{"schema_version":1,"devices":[{"driver":%q,"pci_address":%q,"native_unsupported":true,"reason":%q,"environment_fingerprint":%q,"valid_until":%q}]}`,
		driver, pci, reason, environment.Fingerprint(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
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
	// igb is explicitly forbidden from AF_XDP by the rule library: an
	// unassigned spare is reported with the permitted AF_PACKET fallback.
	if payload["takeover_mode"] != "af_packet" {
		t.Fatalf("takeover_mode = %v, want af_packet", payload["takeover_mode"])
	}
	if payload["takeover_class"] != "compatibility" {
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

func TestInterfaceTakeoverStatusCompatibilityPortIsAvailableWithoutConsent(t *testing.T) {
	newTakeoverFixture(t, "eth4", "0000:13:00.1", "vmxnet3", "0", 0, false)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth4", map[string]any{}, map[string]bool{})

	// The compatibility path is the automatic last resort; the status reports it
	// as the planned mode instead of asking for consent.
	if payload["takeover_state"] != "available" {
		t.Fatalf("takeover_state = %v, want available", payload["takeover_state"])
	}
	if payload["takeover_mode"] != "af_packet" || payload["takeover_class"] != "compatibility" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["takeover_display"] != "compatibility" {
		t.Fatalf("takeover_display = %v, want compatibility", payload["takeover_display"])
	}
	if payload["verification"] != "pending_link" {
		t.Fatalf("verification = %v", payload["verification"])
	}
}

func TestInterfaceTakeoverStatusManagementNeverTakenOver(t *testing.T) {
	newTakeoverFixture(t, "ens192", "0000:0b:00.0", "vmxnet3", "1", 1000, true)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "ens192", map[string]any{"gateway_role": "management"}, map[string]bool{})

	if payload["management"] != true {
		t.Fatalf("management = %v", payload["management"])
	}
	if payload["takeover_state"] != "management" {
		t.Fatalf("takeover_state = %v, want management", payload["takeover_state"])
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
	expected := []string{"capability_constraints", "link_speed_capability_mbps", "link_speed_mbps", "link_state", "management", "needs_repair", "pci", "reason", "stale", "takeover_automatic", "takeover_capability", "takeover_class", "takeover_display", "takeover_mode", "takeover_state", "verification"}
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

	// The guard removes the native/DPDK candidates; the compatibility path is
	// still selected automatically, without a consent gate.
	if payload["takeover_state"] != "available" {
		t.Fatalf("takeover_state = %v, want available (AF fallback)", payload["takeover_state"])
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

func TestTakeoverStatusTreatsMismatchedVerificationAsStale(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	// The NIC was replaced: the persisted proof belongs to another device.
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:1b:00.0","mode":"af_packet","link_state":"up","verification":"verified"}`)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{})
	if payload["takeover_state"] != "stale" {
		t.Fatalf("takeover_state = %v, want stale for a PCI mismatch", payload["takeover_state"])
	}
	if payload["stale"] != true || payload["takeover_mode"] != nil {
		t.Fatalf("payload = %#v, want stale without a claimed mode", payload)
	}
	if payload["takeover_capability"] != string(vpp.CapabilityUnknown) {
		t.Fatalf("capability = %v, want unknown", payload["takeover_capability"])
	}
}

func TestTakeoverStatusInterfaceNameAloneIsNotTakenOver(t *testing.T) {
	newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	server := &Server{}
	payload := server.interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if payload["takeover_state"] != "stale" {
		t.Fatalf("takeover_state = %v, want stale for an interface-name-only signal", payload["takeover_state"])
	}
	if payload["stale"] != true {
		t.Fatalf("payload = %#v, want stale=true", payload)
	}
}

func TestCommittedUIOAssessmentDoesNotFabricateIOMMUFacts(t *testing.T) {
	assessment := committedTakeoverAssessment(takeoverModeDPDKUIO, "0000:13:00.0")
	if assessment.Status != vpp.CapabilitySupportedWithConstraints {
		t.Fatalf("status = %q", assessment.Status)
	}
	if !containsString(assessment.Constraints, "no IOMMU protection (uio_pci_generic)") {
		t.Fatalf("constraints = %#v", assessment.Constraints)
	}
	for _, item := range assessment.Evidence {
		if item.Name == "iommu_protected" || item.Name == "vfio_pci" || item.Name == "uio_pci_generic" {
			if item.Known {
				t.Fatalf("committed state fabricated %s=%v", item.Name, item.Value)
			}
		}
	}
}

func TestTakeoverStatusExposesAutomaticProbeOutcome(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(fixture.root, "takeover-state.json"))
	now := time.Now().UTC()
	if err := recordTakeoverTransition(takeoverTransition{
		SchemaVersion: 1, Interface: "eth1",
		FromMode: takeoverModeAFPacket, ToMode: takeoverModeDPDKUIO,
		Capability: string(vpp.CapabilityUnknown), CapabilityUnprovenProbe: true,
		ProbeRequired: true, ProbeResult: "failed",
		Status: takeoverStatusFailed, RollbackResult: takeoverRollbackRestored,
		StartedAt: now, FinishedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	payload := (&Server{}).interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{})
	if payload["capability_unproven_probe"] != true || payload["probe_result"] != "failed" {
		t.Fatalf("payload = %#v, want the probe flags", payload)
	}
	summary, ok := payload["takeover_transition"].(map[string]any)
	if !ok || summary["capability_unproven_probe"] != true || summary["probe_result"] != "failed" {
		t.Fatalf("summary = %#v", payload["takeover_transition"])
	}
}

func writeFakeVPPHardware(t *testing.T, fixture takeoverFixture, body string) {
	t.Helper()
	script := filepath.Join(fixture.root, "vppctl")
	content := "#!/bin/sh\ncat <<'EOF'\n" + strings.TrimSpace(body) + "\nEOF\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", script)
}

func writePCICapability(t *testing.T, fixture takeoverFixture, pci, vendor, device string) {
	t.Helper()
	devicePath := filepath.Join(fixture.root, "bus/pci/devices", pci)
	if err := os.MkdirAll(devicePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devicePath, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devicePath, "device"), []byte(device+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

const j1900HardwareDown = `GigabitEthernet2/0/0               1    down  GigabitEthernet2/0/0
  Link speed: unknown
  Intel e1000
    carrier down
    pci: device 8086:1539 subsystem 8086:0000 address 0000:02:00.00 numa 0`

func TestInterfaceTakeoverStatusReadsDPDKCapabilityWhenCarrierDown(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:02:00.0", "uio_pci_generic", "0", 0, false)
	writePCICapability(t, fixture, "0000:02:00.0", "0x8086", "0x1539")
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:02:00.0","mode":"uio","link_state":"down","verification":"pending_link"}`)
	writeFakeVPPHardware(t, fixture, j1900HardwareDown)

	payload := (&Server{}).interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if payload["takeover_mode"] != "dpdk_uio" {
		t.Fatalf("takeover_mode = %v, want dpdk_uio", payload["takeover_mode"])
	}
	if payload["link_state"] != "down" {
		t.Fatalf("link_state = %v, want down from VPP carrier", payload["link_state"])
	}
	if payload["link_speed_mbps"] != int64(0) {
		t.Fatalf("link_speed_mbps = %v, want 0 while VPP reports unknown", payload["link_speed_mbps"])
	}
	if payload["link_speed_capability_mbps"] != int64(1000) {
		t.Fatalf("link_speed_capability_mbps = %v, want 1000 for 8086:1539", payload["link_speed_capability_mbps"])
	}
}

func TestInterfaceTakeoverStatusReadsDPDKLinkSpeedFromVPP(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:02:00.0", "uio_pci_generic", "0", 0, false)
	writePCICapability(t, fixture, "0000:02:00.0", "0x8086", "0x1539")
	fixture.writeVerification(t, `{"linux_interface":"eth1","pci_address":"0000:02:00.0","mode":"uio","link_state":"down","verification":"pending_link"}`)
	writeFakeVPPHardware(t, fixture, `GigabitEthernet2/0/0               1    up    GigabitEthernet2/0/0
  Link speed: 1 Gbps
  Intel e1000
    carrier up
    pci: device 8086:1539 subsystem 8086:0000 address 0000:02:00.00 numa 0`)

	payload := (&Server{}).interfaceTakeoverStatus(context.Background(), "eth1", map[string]any{}, map[string]bool{"lyroute-eth1": true})
	if payload["link_state"] != "up" {
		t.Fatalf("link_state = %v, want up from VPP carrier", payload["link_state"])
	}
	if payload["link_speed_mbps"] != int64(1000) {
		t.Fatalf("link_speed_mbps = %v, want 1000 from VPP Link speed", payload["link_speed_mbps"])
	}
}

func TestParseVPPHardwareLinksIndexesNameAndPCI(t *testing.T) {
	byName, byPCI := parseVPPHardwareLinks(j1900HardwareDown + `

GigabitEthernet3/0/0               2    up    GigabitEthernet3/0/0
  Link speed: 2.5 Gbps
    carrier up
    pci: device 8086:15f3 subsystem 8086:0000 address 0000:03:00.00 numa 0`)

	first, ok := byName["GigabitEthernet2/0/0"]
	if !ok || first.Carrier != "down" || first.SpeedMbps != 0 || first.PCI != "2:0.0" {
		t.Fatalf("first block = %#v, ok=%v", first, ok)
	}
	second, ok := byName["GigabitEthernet3/0/0"]
	if !ok || second.Carrier != "up" || second.SpeedMbps != 2500 {
		t.Fatalf("second block = %#v, ok=%v", second, ok)
	}
	if got := byPCI["3:0.0"]; got != "GigabitEthernet3/0/0" {
		t.Fatalf("byPCI[3:0.0] = %q", got)
	}
}

func TestLinkSpeedCapabilityFromPCIIDsAndDriver(t *testing.T) {
	cases := []struct {
		vendor, device, driver string
		want                   int64
	}{
		{"0x8086", "0x1539", "", 1000},
		{"8086", "15f3", "", 2500},
		{"8086", "10fb", "", 10000},
		{"", "", "ixgbe", 10000},
		{"", "", "igc", 2500},
	}
	for _, item := range cases {
		got := linkSpeedCapabilityForIDs(item.vendor, item.device)
		if got == 0 {
			got = linkSpeedCapabilityForDriver(item.driver)
		}
		if got != item.want {
			t.Fatalf("capability(%q:%q,%q) = %d, want %d", item.vendor, item.device, item.driver, got, item.want)
		}
	}
}

func TestParseVPPHardwareLinksIgnoresEthernetAddress(t *testing.T) {
	// VPP prints "Ethernet address" above the "pci:" line. A parser that
	// matches the bare word "address" indexes the port by MAC and every DPDK
	// port silently loses live carrier/speed telemetry.
	output := `GigabitEthernet2/0/0               1    down  GigabitEthernet2/0/0
  Link speed: unknown
  Ethernet address 00:e2:69:2a:fb:58
  Intel e1000
    carrier down
    pci: device 8086:1539 subsystem 8086:0000 address 0000:02:00.00 numa 0`
	byName, byPCI := parseVPPHardwareLinks(output)
	snapshot, ok := byName["GigabitEthernet2/0/0"]
	if !ok || snapshot.PCI != "2:0.0" || snapshot.Carrier != "down" {
		t.Fatalf("block = %#v, ok=%v", snapshot, ok)
	}
	if got := byPCI["2:0.0"]; got != "GigabitEthernet2/0/0" {
		t.Fatalf("byPCI[2:0.0] = %q", got)
	}
	if _, wrong := byPCI["00:e2:69:2a:fb:58"]; wrong {
		t.Fatalf("byPCI indexed the MAC address instead of the PCI slot")
	}
}
