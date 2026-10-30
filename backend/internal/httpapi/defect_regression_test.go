package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

// The inventory is rebuilt from the interface table on every apply. Deleting a
// WAN line only unbinds the port; dropping it from the inventory made
// re-creating the line on the same NIC fail with 422 and left no self-heal path.
func TestDataplaneInventoryKeepsUnboundLivePorts(t *testing.T) {
	root := t.TempDir()
	ports := map[string]string{"ens192": "0000:03:00.0", "ens161": "0000:04:00.0"}
	for name, pci := range ports {
		pciDir := filepath.Join(root, "bus", "pci", "devices", pci)
		if err := os.MkdirAll(pciDir, 0o755); err != nil {
			t.Fatal(err)
		}
		portDir := filepath.Join(root, "class", "net", name)
		if err := os.MkdirAll(portDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(pciDir, filepath.Join(portDir, "device")); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	inventoryPath := filepath.Join(t.TempDir(), "dataplane-inventory.json")
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)
	previous := `{
  "schema_version": 1,
  "source": "ly-route-control-api",
  "management": {"linux_interface": "ens160", "pci": "0000:0b:00.0", "role": "management"},
  "data_interfaces": [
    {"linux_interface": "ens161", "pci": "0000:04:00.0", "mac": "00:50:56:3a:15:21", "kernel_driver": "vmxnet3", "role": "data"},
    {"linux_interface": "ens192", "pci": "0000:03:00.0", "mac": "00:50:56:3a:15:22", "kernel_driver": "vmxnet3", "role": "data"}
  ]
}`
	if err := os.WriteFile(inventoryPath, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(WithClock(fixedClock()))
	// The plan now advertises only the LAN port (the WAN line was deleted).
	if err := server.writeDataplaneInventory(context.Background(), []string{"ens192"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ens161"`) {
		t.Fatalf("unbound port disappeared from the inventory: %s", body)
	}
	// A port whose hardware is really gone must still be pruned.
	if err := os.RemoveAll(filepath.Join(root, "class", "net", "ens161")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "bus", "pci", "devices", "0000:04:00.0")); err != nil {
		t.Fatal(err)
	}
	if err := server.writeDataplaneInventory(context.Background(), []string{"ens192"}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"ens161"`) {
		t.Fatalf("removed hardware stayed in the inventory: %s", body)
	}
}

// The runtime/status body must always be valid JSON, including when a failed
// apply reason carries raw newlines, carriage returns or tabs from VPP output.
func TestRuntimeStatusEscapesControlCharactersInReasons(t *testing.T) {
	server := New(WithClock(fixedClock()))
	server.lastFailedApply = &RuntimeApplyResult{
		Status:        "apply_failed",
		RuntimeState:  "degraded",
		TransactionID: "txn-control-chars",
		Reason:        "line one\nline two\r\n\ttabbed \"quoted\" \\ backslash",
		Components: []RuntimeComponentState{{
			Name:   "vpp",
			State:  "degraded",
			Reason: "refusing VPP command\nrollback step \"restore\"\tfailed\r",
		}},
	}
	response := request(t, server, http.MethodGet, "/api/v1/runtime/status")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.Bytes()
	if !json.Valid(body) {
		t.Fatalf("runtime/status returned invalid JSON:\n%s", body)
	}
	for _, control := range []byte{'\n', '\r', '\t'} {
		// Raw control characters may only appear between JSON tokens, never
		// inside a string. encoding/json escapes every one of them.
		if strings.Contains(string(body), "\\"+string(control)) || strings.Contains(string(body), "\""+string(control)) {
			t.Fatalf("raw control character %q inside a JSON string", control)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	failed, ok := decoded["last_failed_apply"].(map[string]any)
	if !ok {
		t.Fatalf("last_failed_apply missing: %s", body)
	}
	if got, _ := failed["reason"].(string); !strings.Contains(got, "line two") {
		t.Fatalf("reason did not round-trip: %q", got)
	}
}

// The two 4xx paths that used to surface as a bare dataplane_locked must name
// the ports that lost their attachment proof.
func TestUnprovenAddressAssignmentPortsNamesTheAffectedPorts(t *testing.T) {
	plan := RuntimePlan{DataplaneProof: []vpp.PrerequisiteResult{{
		Name: "address_assignment_proven", Interface: "ens161", Passed: false,
	}, {
		Name: "address_assignment_vpp_interface_matches", Interface: "ens161", Passed: false,
	}, {
		Name: "address_assignment_proven", Interface: "ens192", Passed: true,
	}}}
	ports := unprovenAddressAssignmentPorts(plan)
	if len(ports) != 1 || ports[0] != "ens161" {
		t.Fatalf("ports = %#v", ports)
	}
}
