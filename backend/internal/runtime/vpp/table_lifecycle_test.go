package vpp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFIBTableForwardingDefaultDetection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "missing table", in: "", want: false},
		{name: "header only", in: "ipv4-VRF:100, fib_index:2\n", want: false},
		{name: "retained drop", in: "ipv4-VRF:100\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: dpo-drop ip4\n", want: false},
		{name: "direct via", in: "ipv4-VRF:100\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: ipv4 via 198.51.100.1 pppoe0\n", want: true},
		{name: "lookup table", in: "ipv4-VRF:100\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: dst-address,unicast lookup in ipv4-VRF:0\n", want: true},
	}
	for _, tc := range cases {
		if got := fibTableHasForwardingDefault(tc.in); got != tc.want {
			t.Errorf("%s: fibTableHasForwardingDefault() = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestTableDeletionSkipsNonEmptyAndMissingTables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands.log")
	t.Setenv("LY_ROUTE_TEST_ARGS_LOG", logPath)
	binary := filepath.Join(dir, "vppctl")
	script := `#!/bin/sh
if [ "$1 $2 $3" = "show ip table" ]; then
  echo "[0] table_id:100 ipv4-VRF:100"
  echo "[1] table_id:300 ipv4-VRF:300"
  exit 0
fi
if [ "$1 $2 $3 $4" = "show ip fib table" ]; then
  id=$5
  echo "ipv4-VRF:$id, fib_index:$id"
  case "$id" in
    100) echo "0.0.0.0/0";;
  esac
  exit 0
fi
echo "$*" >> "$LY_ROUTE_TEST_ARGS_LOG"
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	channel := &vppctlChannel{binary: binary}
	_, err := channel.doCommands(context.Background(), Operation{
		Name: "vpp.route-policy.rollback-delete",
		VPPCtlCommands: []string{
			"ip route del table 200 0.0.0.0/0",
			"ip table del 200",
			"ip route del table 100 0.0.0.0/0",
			"ip table del 100",
			"ip table del 300",
			"ip table add 100",
			"ip table add 400",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	for _, skipped := range []string{"ip table del 200", "ip table del 100", "ip table del 300", "ip route del table 200", "ip table add 100"} {
		if strings.Contains(text, skipped) {
			t.Fatalf("unsafe table command executed for %q: %q", skipped, text)
		}
	}
	if !strings.Contains(text, "ip route del table 100 0.0.0.0/0") {
		t.Fatalf("existing table route cleanup was skipped: %q", text)
	}
	if !strings.Contains(text, "ip table add 400") {
		t.Fatalf("missing table was not created: %q", text)
	}
}
