package httpapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/vpp"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeTestPCIDevice(t *testing.T, root, pci, driver string) {
	t.Helper()
	device := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers", driver)
	for _, directory := range []string{device, driverDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driverDir, filepath.Join(device, "driver")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}

func makeTestNetdev(t *testing.T, root, name, pci, carrier string) {
	t.Helper()
	device := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers/vmxnet3")
	for _, directory := range []string{device, driverDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driverDir, filepath.Join(device, "driver")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	netdev := filepath.Join(root, "class/net", name)
	if err := os.MkdirAll(netdev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "carrier"), []byte(carrier+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func useTestInterfaces(t *testing.T, items []map[string]any) {
	t.Helper()
	original := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return items
	}
	t.Cleanup(func() { hostInterfaceInventory = original })
}

func TestVerifyDPDKTakeoversGuardsAllBoundPortsOnConnectionRefused(t *testing.T) {
	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.json")
	writeTestFile(t, inventoryPath, `{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","kernel_driver":"vmxnet3"},{"linux_interface":"ens256","pci":"0000:1b:00.0","kernel_driver":"vmxnet3"}]}`)
	makeTestPCIDevice(t, root, "0000:13:00.0", "uio_pci_generic")
	makeTestPCIDevice(t, root, "0000:1b:00.0", "uio_pci_generic")

	vppctl := filepath.Join(root, "vppctl")
	writeTestFile(t, vppctl, "#!/bin/sh\nprintf '%s\\n' 'connect: Connection refused' >&2\nexit 1\n")
	if err := os.Chmod(vppctl, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))

	server := &Server{}
	verified, failures := server.verifyDPDKTakeovers(context.Background(), []string{"ens224", "ens256"})
	if len(verified) != 0 {
		t.Fatalf("verified = %#v, want none while VPP is down", verified)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %#v, want a guard event for both bound ports", failures)
	}
	for _, failure := range failures {
		if !failure.Crashed {
			t.Fatalf("failure %#v was not marked as a crash", failure)
		}
		if !vppDownSignature(failure.Reason) {
			t.Fatalf("reason %q does not explain the dead VPP", failure.Reason)
		}
	}
	if !server.recordTakeoverCrashGuard(failures) {
		t.Fatal("crash guard was not recorded")
	}
	state, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "takeover-capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pci := range []string{"0000:13:00.0", "0000:1b:00.0"} {
		capability, unsupported := state.NativeUnsupported("vmxnet3", pci)
		if !unsupported {
			t.Fatalf("%s was not crash-guarded: %#v", pci, state)
		}
		if strings.TrimSpace(capability.Reason) == "" {
			t.Fatalf("%s guard has no reason: %#v", pci, capability)
		}
		if capability.Driver != "vmxnet3" {
			t.Fatalf("%s guard driver = %q, want vmxnet3", pci, capability.Driver)
		}
	}
}

func TestVerifyDPDKTakeoversGuardsOnlyThePortThatKilledVPP(t *testing.T) {
	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.json")
	writeTestFile(t, inventoryPath, `{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","kernel_driver":"vmxnet3"},{"linux_interface":"ens256","pci":"0000:1b:00.0","kernel_driver":"vmxnet3"}]}`)
	makeTestPCIDevice(t, root, "0000:13:00.0", "uio_pci_generic")
	makeTestPCIDevice(t, root, "0000:1b:00.0", "uio_pci_generic")

	vppctl := filepath.Join(root, "vppctl")
	writeTestFile(t, vppctl, `#!/bin/sh
case "$*" in
  "show version") printf '%s\n' 'vpp v26.06-release' ;;
  "show hardware-interfaces") cat <<'OUT'
              Name                Idx   Link  Hardware
GigabitEthernet13/0/0              1    down  GigabitEthernet13/0/0
  pci: device 15ad:07b0 address 0000:13:00.00
GigabitEthernet1b/0/0              2    down  GigabitEthernet1b/0/0
  pci: device 15ad:07b0 address 0000:1b:00.00
OUT
    ;;
  "show hardware-interfaces GigabitEthernet13/0/0"|"show hardware-interfaces GigabitEthernet1b/0/0")
    printf '%s\n' '  Link speed: 10.000000 Gbps' '    carrier down'
    ;;
  "show interface GigabitEthernet13/0/0"|"show interface GigabitEthernet1b/0/0")
    printf '%s\n' "${3} 1 up 9000/0/0/0"
    ;;
  "set interface state GigabitEthernet13/0/0 up")
    printf '%s\n' 'connect: Connection refused' >&2
    exit 1
    ;;
  "set interface state GigabitEthernet1b/0/0 up") exit 0 ;;
esac
`)
	if err := os.Chmod(vppctl, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))

	server := &Server{}
	_, failures := server.verifyDPDKTakeovers(context.Background(), []string{"ens224", "ens256"})
	if len(failures) != 2 {
		t.Fatalf("failures = %#v, want the crashing candidate plus a retryable neighbour", failures)
	}
	byName := map[string]dpdkTakeoverFailure{}
	for _, failure := range failures {
		byName[failure.LinuxInterface] = failure
	}
	if !byName["ens224"].Crashed || !strings.Contains(strings.ToLower(byName["ens224"].Reason), "connection refused") {
		t.Fatalf("ens224 = %#v, want the admin-up crash event", byName["ens224"])
	}
	if byName["ens256"].Crashed {
		t.Fatalf("ens256 = %#v, a port that was never admin-upped must stay retryable", byName["ens256"])
	}
	server.recordTakeoverCrashGuard(failures)
	state, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "takeover-capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, unsupported := state.NativeUnsupported("vmxnet3", "0000:13:00.0"); !unsupported {
		t.Fatal("the crashing PCI was not crash-guarded")
	}
	if _, unsupported := state.NativeUnsupported("vmxnet3", "0000:1b:00.0"); unsupported {
		t.Fatal("a healthy neighbour was crash-guarded")
	}
}

func TestAFConsentGateUsesProductIDsForVPPOwnedPorts(t *testing.T) {
	root := t.TempDir()
	makeTestNetdev(t, root, "ens192", "0000:0b:00.0", "1")
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", filepath.Join(root, "capabilities.json"))
	writeTestFile(t, filepath.Join(root, "ownership.json"), `{"schema_version":1,"transport":"uio","devices":[{"pci_address":"0000:13:00.0","linux_interface":"ens224","mac_address":"00:50:56:3a:15:08","kernel_driver":"vmxnet3","transport":"uio","created_by_product":true},{"pci_address":"0000:1b:00.0","linux_interface":"ens256","mac_address":"00:50:56:3a:15:09","kernel_driver":"vmxnet3","transport":"uio","created_by_product":true}]}`)
	writeTestFile(t, filepath.Join(root, "inventory.json"), `{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","mac":"00:50:56:3a:15:08","kernel_driver":"vmxnet3"},{"linux_interface":"ens256","pci":"0000:1b:00.0","mac":"00:50:56:3a:15:09","kernel_driver":"vmxnet3"}]}`)
	useTestInterfaces(t, []map[string]any{{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0"}})

	server := &Server{}
	consent := server.canonicalAFRequiredInterfaces(context.Background(), []string{"ens256"})
	if len(consent) != 1 || consent[0] != "eth2" {
		t.Fatalf("AF consent ports = %#v, want [eth2] (never the kernel name ens256)", consent)
	}
	for _, port := range consent {
		if !isProductInterfaceID(port) {
			t.Fatalf("AF consent leaked a kernel netdev name: %q", port)
		}
	}
	ports := server.canonicalPolicyPorts(context.Background(), []string{"ens224", "ens256"})
	sort.Strings(ports)
	if len(ports) != 2 || ports[0] != "eth1" || ports[1] != "eth2" {
		t.Fatalf("canonical policy ports = %#v, want [eth1 eth2]", ports)
	}
	policy := DataplanePolicy{AFPacketFallback: true, Ports: ports}
	if !server.allowsAFPacketFor(context.Background(), policy, "ens256") {
		t.Fatal("consent keyed by product id did not match the kernel name")
	}
	if server.allowsAFPacketFor(context.Background(), policy, "ens224") != true {
		t.Fatal("consent for eth1 was lost")
	}
}

func TestInterfaceSnapshotKeepsVPPOwnedPortAndStableIDs(t *testing.T) {
	root := t.TempDir()
	makeTestNetdev(t, root, "ens192", "0000:0b:00.0", "1")
	makeTestNetdev(t, root, "ens256", "0000:1b:00.0", "1")
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", filepath.Join(root, "capabilities.json"))
	t.Setenv("LY_ROUTE_VPPCTL", "/bin/false")
	writeTestFile(t, filepath.Join(root, "ownership.json"), `{"schema_version":1,"transport":"uio","devices":[{"pci_address":"0000:13:00.0","linux_interface":"ens224","mac_address":"00:50:56:3a:15:08","kernel_driver":"vmxnet3","transport":"uio","created_by_product":true}]}`)
	// ens224 also carries a stale AF_PACKET verification from a previous
	// generation; the live UIO ownership record must win.
	writeTestFile(t, filepath.Join(root, "verification.json"), `{"schema_version":1,"interfaces":[{"linux_interface":"ens256","pci_address":"0000:1b:00.0","mode":"af_packet","verification":"verified","functional_verified":true,"link_state":"up","hardware_link":"up"},{"linux_interface":"ens224","pci_address":"0000:13:00.0","mode":"af_packet","verification":"verified","functional_verified":true,"link_state":"up","hardware_link":"up"}]}`)
	writeTestFile(t, filepath.Join(root, "inventory.json"), `{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","mac":"00:50:56:3a:15:08","kernel_driver":"vmxnet3"},{"linux_interface":"ens256","pci":"0000:1b:00.0","mac":"00:50:56:3a:15:09","kernel_driver":"vmxnet3"}]}`)
	useTestInterfaces(t, []map[string]any{
		{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0", "link_state": "up"},
		{"id": "ens256", "name": "ens256", "pci": "0000:1b:00.0", "link_state": "up"},
	})

	server := &Server{}
	items := []map[string]any{
		{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0", "link_state": "up"},
		{"id": "ens256", "name": "ens256", "pci": "0000:1b:00.0", "link_state": "up"},
	}
	snapshot := server.normalizeInterfaceSnapshot(context.Background(), items, "running", "", false)
	if len(snapshot) != 3 {
		t.Fatalf("snapshot = %#v, want eth0/eth1/eth2 with the VPP-owned port retained", snapshot)
	}
	if got := []string{stringField(snapshot[0], "id"), stringField(snapshot[1], "id"), stringField(snapshot[2], "id")}; got[0] != "eth0" || got[1] != "eth1" || got[2] != "eth2" {
		t.Fatalf("interface order = %#v, want eth0/eth1/eth2", got)
	}
	bySystem := map[string]map[string]any{}
	for _, item := range snapshot {
		bySystem[stringField(item, "system_name")] = item
	}
	owned, ok := bySystem["ens224"]
	if !ok {
		t.Fatalf("VPP-owned ens224 disappeared from /api/v1/interfaces: %#v", snapshot)
	}
	if stringField(owned, "id") != "eth1" {
		t.Fatalf("VPP-owned port id = %q, want eth1", stringField(owned, "id"))
	}
	if stringField(owned, "takeover_state") != "taken_over" || stringField(owned, "takeover_mode") != "dpdk_uio" {
		t.Fatalf("VPP-owned port takeover = %#v", owned)
	}
	if stringField(owned, "verification") != "pending_link" {
		t.Fatalf("stale AF proof on a UIO-bound port = %q, want pending_link", stringField(owned, "verification"))
	}
	if stringField(owned, "link_state") != "unknown" {
		t.Fatalf("VPP-owned port link_state = %q, want unknown without a kernel netdev", stringField(owned, "link_state"))
	}
	af, ok := bySystem["ens256"]
	if !ok {
		t.Fatalf("ens256 disappeared from the snapshot: %#v", snapshot)
	}
	if stringField(af, "id") != "eth2" {
		t.Fatalf("ens256 id = %q, want eth2 (stable across the UIO bind)", stringField(af, "id"))
	}
	if stringField(af, "takeover_state") != "taken_over" || stringField(af, "takeover_mode") != "af_packet" {
		t.Fatalf("AF port takeover = %#v", af)
	}
}

func TestSuppressCrashGuardedCandidatesFallsBackToAF(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "takeover-capabilities.json")
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", path)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPP_VERSION", "test-vpp")
	t.Setenv("LY_ROUTE_VPP_RUNTIME_ID", "test-runtime")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")
	environment := dataplane.CapabilityEnvironment{
		KernelDriver: "vmxnet3",
		BoundDriver:  "",
		VPPVersion:   "test-vpp",
		VPPRuntimeID: "test-runtime",
		Hugepages:    512,
		PCIAddress:   "0000:13:00.0",
	}
	writeTestFile(t, path, fmt.Sprintf(`{"schema_version":1,"devices":[{"driver":"vmxnet3","pci_address":"0000:13:00.0","native_unsupported":true,"reason":"SIGSEGV","environment_fingerprint":%q,"valid_until":%q}]}`, environment.Fingerprint(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	server := &Server{}
	pathRequest := vpp.NativePathRequest{Assignments: []vpp.NativeAssignment{
		{
			LinuxInterface: "ens224",
			Proof:          vpp.CapabilityProof{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: "0000:13:00.0", KernelDriver: "vmxnet3"},
			Candidates: []vpp.CapabilityProof{
				{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: "0000:13:00.0", KernelDriver: "vmxnet3"},
				{Tier: vpp.DataplaneTierNative, Hook: vpp.NativeHookAFPacket, Mode: vpp.NativeModeAFPacket, AcceptanceOnly: true},
			},
		},
		{
			LinuxInterface: "ens256",
			Candidates: []vpp.CapabilityProof{
				{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: "0000:1b:00.0", KernelDriver: "vmxnet3"},
			},
		},
	}}
	server.suppressCrashGuardedCandidates(&pathRequest)
	if len(pathRequest.Assignments[0].Candidates) != 1 || pathRequest.Assignments[0].Candidates[0].Hook != vpp.NativeHookAFPacket {
		t.Fatalf("guarded assignment = %#v, want only the AF fallback", pathRequest.Assignments[0])
	}
	if pathRequest.Assignments[0].Proof.Hook != "" {
		t.Fatalf("stale DPDK proof survived: %#v", pathRequest.Assignments[0].Proof)
	}
	if len(pathRequest.Assignments[1].Candidates) != 1 {
		t.Fatalf("unguarded assignment lost its DPDK candidate: %#v", pathRequest.Assignments[1])
	}
}
