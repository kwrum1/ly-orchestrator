package vpp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestQoSPluginAvailableReadsPluginInventory(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "vppctl")
	script := "#!/bin/sh\nprintf '%s\\n' ' 51. qos_plugin.so 26.06-release Quality of Service'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	available, err := QoSPluginAvailable(context.Background(), binary)
	if err != nil || !available {
		t.Fatalf("QoSPluginAvailable = %t, %v; want true, nil", available, err)
	}

	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' ' 52. nat_plugin.so 26.06-release NAT'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	available, err = QoSPluginAvailable(context.Background(), binary)
	if err != nil || available {
		t.Fatalf("QoSPluginAvailable = %t, %v; want false, nil", available, err)
	}
}

func TestQoSPluginAvailableFailsOpenWhenProbeCannotRun(t *testing.T) {
	available, err := QoSPluginAvailable(context.Background(), filepath.Join(t.TempDir(), "missing-vppctl"))
	if err == nil {
		t.Fatal("missing vppctl did not report an error")
	}
	if !available {
		t.Fatal("probe failure dropped QoS instead of failing open")
	}
}
