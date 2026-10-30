package vpp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writePPPoERuntimeStatus(t *testing.T, dir, peer, interfaceName string) {
	t.Helper()
	path := filepath.Join(dir, peer+".json")
	content := `{"state":"connecting","interface":"` + interfaceName + `"}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePPPoECommandWaitsForDelayedStatusAndInterface(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	dir := t.TempDir()
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_STATUS_DIR", dir)
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_ATTEMPTS", "100")
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_INTERVAL", "20ms")
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("LY_ROUTE_TEST_ARGS_LOG", argsLog)
	binary := filepath.Join(dir, "vppctl")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = show ] && [ \"$2\" = interface ]; then\n" +
		"  echo 'Name Idx State'\n" +
		"  echo 'lyppp-test 1 up'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"$*\" >> \"$LY_ROUTE_TEST_ARGS_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		writePPPoERuntimeStatus(t, dir, "wan-primary", "lyppp-test")
	}()

	channel := &vppctlChannel{binary: binary}
	start := time.Now()
	_, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.dns-service.network",
		RequestID:      "txn-pppoe-delay",
		VPPCtlCommands: []string{"set interface nat44 in lydns0 out pppoe-runtime:wan-primary"},
	})
	if err != nil {
		t.Fatalf("runtime PPPoE command failed while the status was delayed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("command executed before the status file appeared (%s)", elapsed)
	}
	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "out lyppp-test") {
		t.Fatalf("executed command did not use the resolved session interface: %q", string(logged))
	}
	if strings.Contains(string(logged), "pppoe-runtime:") {
		t.Fatalf("unresolved placeholder reached vppctl: %q", string(logged))
	}
}

func TestRuntimePPPoECommandFailsClosedWhenStatusNeverAppears(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	dir := t.TempDir()
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_STATUS_DIR", dir)
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_ATTEMPTS", "3")
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_INTERVAL", "10ms")
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("LY_ROUTE_TEST_ARGS_LOG", argsLog)
	binary := filepath.Join(dir, "vppctl")
	script := "#!/bin/sh\nif [ \"$1\" = show ] && [ \"$2\" = interface ]; then echo 'Name Idx State'; exit 0; fi\n" +
		"echo \"$*\" >> \"$LY_ROUTE_TEST_ARGS_LOG\"\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	channel := &vppctlChannel{binary: binary}
	_, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.dns-service.network",
		RequestID:      "txn-pppoe-missing",
		VPPCtlCommands: []string{"set interface nat44 in lydns0 out pppoe-runtime:wan-primary"},
	})
	if err == nil {
		t.Fatal("missing PPPoE status must fail the required command")
	}
	if !strings.Contains(err.Error(), `PPPoE runtime peer "wan-primary" is not ready`) {
		t.Fatalf("error = %v, want an explicit PPPoE readiness rejection", err)
	}
	if data, readErr := os.ReadFile(argsLog); readErr == nil && strings.Contains(string(data), "nat44") {
		t.Fatalf("VPP command was executed without a published status: %q", string(data))
	}
}

func TestRuntimePPPoECommandWaitsForSessionInterfaceCreation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	dir := t.TempDir()
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_STATUS_DIR", dir)
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_ATTEMPTS", "100")
	t.Setenv("LY_ROUTE_PPPOE_RUNTIME_READY_INTERVAL", "20ms")
	writePPPoERuntimeStatus(t, dir, "wan-primary", "lyppp-late")
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("LY_ROUTE_TEST_ARGS_LOG", argsLog)
	marker := filepath.Join(dir, "interface-created")
	binary := filepath.Join(dir, "vppctl")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = show ] && [ \"$2\" = interface ]; then\n" +
		"  echo 'Name Idx State'\n" +
		"  [ -f \"$LY_ROUTE_TEST_INTERFACE_MARKER\" ] && echo 'lyppp-late 1 up'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"$*\" >> \"$LY_ROUTE_TEST_ARGS_LOG\"\nexit 0\n"
	t.Setenv("LY_ROUTE_TEST_INTERFACE_MARKER", marker)
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("1"), 0o600)
	}()

	channel := &vppctlChannel{binary: binary}
	_, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.dns-service.network",
		RequestID:      "txn-pppoe-iface-delay",
		VPPCtlCommands: []string{"set interface nat44 in lydns0 out pppoe-runtime:wan-primary"},
	})
	if err != nil {
		t.Fatalf("command failed while the session interface was being created: %v", err)
	}
	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "out lyppp-late") {
		t.Fatalf("resolved interface missing from executed command: %q", string(logged))
	}
}
