package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeNAT44VRFHarness(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	vppctl := filepath.Join(dir, "vppctl")
	if err := os.WriteFile(vppctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$NAT44_OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(dir, "harness.sh")
	content := "#!/bin/sh\nset -eu\nVPPCTL=\"" + vppctl + "\"\n" + proxyNAT44VRFShellHelpers + "\n" + body + "\n"
	if err := os.WriteFile(harness, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return harness, vppctl
}

func runNAT44VRFHarness(t *testing.T, harness, output string, args ...string) (string, bool) {
	t.Helper()
	command := exec.Command("sh", append([]string{harness}, args...)...)
	command.Env = append(os.Environ(), "NAT44_OUTPUT="+output)
	combined, err := command.CombinedOutput()
	return string(combined), err == nil
}

func TestProxyNAT44PoolHasVRFRequiresTenantScopedAddress(t *testing.T) {
	harness, _ := writeNAT44VRFHarness(t, `nat44_pool_has_vrf "$1" "$2"`)
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name: "vrf0-only",
			output: `NAT44 pool addresses:
  10.1.18.24
    tenant VRF: 0
`,
			want: false,
		},
		{
			name: "tenant-only",
			output: `NAT44 pool addresses:
  10.1.18.24
    tenant VRF: 30001
`,
			want: true,
		},
		{
			name: "tenant-only-lowercase",
			output: `NAT44 pool addresses:
  10.1.18.24
    tenant vrf: 30001
`,
			want: true,
		},
		{
			name: "both-vrfs",
			output: `NAT44 pool addresses:
  10.1.18.24
    tenant VRF: 0
  10.1.18.24
    tenant VRF: 30001
`,
			want: true,
		},
		{
			name: "twice-nat-does-not-count",
			output: `NAT44 pool addresses:
NAT44 twice-nat pool addresses:
  10.1.18.24
    tenant VRF: 30001
`,
			want: false,
		},
		{
			name: "other-tenant-does-not-count",
			output: `NAT44 pool addresses:
  10.1.18.24
    tenant VRF: 30002
`,
			want: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output, ok := runNAT44VRFHarness(t, harness, testCase.output, "10.1.18.24", "30001")
			if ok != testCase.want {
				t.Fatalf("nat44_pool_has_vrf = %v, want %v; output: %s", ok, testCase.want, output)
			}
		})
	}
}

func TestProxyNAT44WaitVRFReportsActionableReadbackFailure(t *testing.T) {
	harness, _ := writeNAT44VRFHarness(t, `nat44_wait_vrf "tenant NAT pool" "$1" "$2"`)
	output, ok := runNAT44VRFHarness(t, harness, "NAT44 pool addresses:\n  10.1.18.24\n    tenant VRF: 0\n", "10.1.18.24", "30001")
	if ok {
		t.Fatalf("nat44_wait_vrf accepted a VRF 0-only address; output: %s", output)
	}
	for _, required := range []string{"missing '10.1.18.24' in tenant VRF 30001", "last inventory", "tenant VRF: 0"} {
		if !strings.Contains(output, required) {
			t.Fatalf("readback failure is not actionable, missing %q: %s", required, output)
		}
	}
}
