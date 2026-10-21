package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVPPRouteBatchUsesBoundedChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "batch-sizes")
	binary := filepath.Join(tempDir, "vppctl")
	script := "#!/bin/sh\n[ \"$1\" = exec ] || exit 2\nwc -l < \"$2\" >> \"$LY_ROUTE_TEST_BATCH_LOG\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_TEST_BATCH_LOG", logPath)

	commands := make([]string, vppRouteBatchChunkSize*2+3)
	for index := range commands {
		commands[index] = fmt.Sprintf("ip route add table 100 192.0.2.%d/32 via local0", index%256)
	}
	channel := &vppctlChannel{binary: binary}
	results, err := channel.doVPPRouteBatch(context.Background(), commands)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("batch results = %d, want 3", len(results))
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\n%d\n3", vppRouteBatchChunkSize, vppRouteBatchChunkSize)
	if got := strings.TrimSpace(string(logged)); got != want {
		t.Fatalf("batch sizes = %q, want %q", got, want)
	}
}

func TestVPPCTLCommandTimeoutStopsHungProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "vppctl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL_COMMAND_TIMEOUT", "100ms")
	start := time.Now()
	channel := &vppctlChannel{binary: binary}
	_, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.interface.address",
		VPPCtlCommands: []string{"show version"},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error=%v, want bounded timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout took %s, want under 1.5s", elapsed)
	}
}
