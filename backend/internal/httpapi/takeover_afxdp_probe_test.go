package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAFXDPProbeCacheProbesOnceThenInvalidates(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	fingerprint := "vpp=100\nnet=eth0=0000:03:00.0"
	pluginFingerprint := "plugins-a"
	pluginLoaded := true
	pluginCalls := 0
	queueCalls := 0
	cache := &afXDPProbeCache{
		fingerprintFn: func() string { return fingerprint },
		now:           func() time.Time { return now },
		pluginFn: func(context.Context, string) (string, bool, error) {
			pluginCalls++
			return pluginFingerprint, pluginLoaded, nil
		},
		queueFn: func(context.Context, string) ([]int, error) {
			queueCalls++
			return []int{12}, nil
		},
	}

	got := cache.queueFDs(context.Background(), "vppctl", false)
	if len(got) != 1 || got[0] != 12 {
		t.Fatalf("first queueFDs = %v, want [12]", got)
	}
	if pluginCalls != 1 || queueCalls != 1 {
		t.Fatalf("first calls plugin=%d queue=%d, want 1/1", pluginCalls, queueCalls)
	}

	// A status poll every few seconds must not touch VPP at all while the
	// process and interface set are unchanged.
	for i := 0; i < 19; i++ {
		now = now.Add(3 * time.Second)
		if got := cache.queueFDs(context.Background(), "vppctl", false); len(got) != 1 || got[0] != 12 {
			t.Fatalf("cached queueFDs = %v, want [12]", got)
		}
	}
	if pluginCalls != 1 || queueCalls != 1 {
		t.Fatalf("cached calls plugin=%d queue=%d, want 1/1", pluginCalls, queueCalls)
	}

	// The periodic plugin inventory recheck is allowed to read `show plugins`,
	// but an unchanged inventory must not re-run `show af_xdp`.
	now = now.Add(3 * time.Second)
	if got := cache.queueFDs(context.Background(), "vppctl", false); len(got) != 1 || got[0] != 12 {
		t.Fatalf("rechecked queueFDs = %v, want [12]", got)
	}
	if pluginCalls != 2 || queueCalls != 1 {
		t.Fatalf("recheck calls plugin=%d queue=%d, want 2/1", pluginCalls, queueCalls)
	}

	// A new netdev/PCI (or a VPP restart, which is part of the same
	// fingerprint) invalidates the cached answer exactly once.
	fingerprint = "vpp=100\nnet=eth0=0000:03:00.0\neth1=0000:04:00.0"
	if got := cache.queueFDs(context.Background(), "vppctl", false); len(got) != 1 || got[0] != 12 {
		t.Fatalf("hardware change queueFDs = %v, want [12]", got)
	}
	if queueCalls != 2 {
		t.Fatalf("hardware change queue=%d, want 2", queueCalls)
	}

	// A plugin inventory change invalidates it as well.
	pluginFingerprint = "plugins-b"
	now = now.Add(afXDPPluginRecheckInterval)
	cache.queueFDs(context.Background(), "vppctl", false)
	if queueCalls != 3 {
		t.Fatalf("plugin change queue=%d, want 3", queueCalls)
	}

	// A transition forces one fresh read even when nothing else changed.
	cache.queueFDs(context.Background(), "vppctl", true)
	if queueCalls != 4 {
		t.Fatalf("forced queue=%d, want 4", queueCalls)
	}

	// Without the plugin the CLI is never probed, even when forced.
	pluginLoaded = false
	if got := cache.queueFDs(context.Background(), "vppctl", true); got != nil {
		t.Fatalf("unloaded plugin queueFDs = %v, want nil", got)
	}
	if queueCalls != 4 {
		t.Fatalf("unloaded plugin queue=%d, want 4", queueCalls)
	}

	// A failed probe is cached too: VPP 26.06 has the plugin but no CLI.
	pluginLoaded = true
	cache.queueFn = func(context.Context, string) ([]int, error) {
		queueCalls++
		return nil, errors.New("unknown input `af_xdp`")
	}
	if got := cache.queueFDs(context.Background(), "vppctl", true); got != nil {
		t.Fatalf("failed probe queueFDs = %v, want nil", got)
	}
	if queueCalls != 5 {
		t.Fatalf("failed probe queue=%d, want 5", queueCalls)
	}
	if got := cache.queueFDs(context.Background(), "vppctl", false); got != nil {
		t.Fatalf("cached failure queueFDs = %v, want nil", got)
	}
	if queueCalls != 5 {
		t.Fatalf("cached failure queue=%d, want 5", queueCalls)
	}
}

func TestAFXDPProbeCacheRecoversAfterPluginReadFailure(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	fail := true
	pluginCalls := 0
	queueCalls := 0
	cache := &afXDPProbeCache{
		fingerprintFn: func() string { return "stable" },
		now:           func() time.Time { return now },
		pluginFn: func(context.Context, string) (string, bool, error) {
			pluginCalls++
			if fail {
				return "", false, errors.New("vpp is restarting")
			}
			return "plugins", true, nil
		},
		queueFn: func(context.Context, string) ([]int, error) {
			queueCalls++
			return []int{12}, nil
		},
	}

	for i := 0; i < 10; i++ {
		if got := cache.queueFDs(context.Background(), "vppctl", false); got != nil {
			t.Fatalf("failed plugin queueFDs = %v, want nil", got)
		}
		now = now.Add(3 * time.Second)
	}
	if pluginCalls != 1 || queueCalls != 0 {
		t.Fatalf("failure calls plugin=%d queue=%d, want 1/0", pluginCalls, queueCalls)
	}

	// Once the plugin inventory is readable again, one probe recovers the fact.
	fail = false
	now = now.Add(afXDPPluginRecheckInterval)
	if got := cache.queueFDs(context.Background(), "vppctl", false); len(got) != 1 || got[0] != 12 {
		t.Fatalf("recovered queueFDs = %v, want [12]", got)
	}
	if pluginCalls != 2 || queueCalls != 1 {
		t.Fatalf("recovery calls plugin=%d queue=%d, want 2/1", pluginCalls, queueCalls)
	}
}

func TestAFXDPProbeCacheSerializesConcurrentProbes(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	queueCalls := 0
	release := make(chan struct{})
	cache := &afXDPProbeCache{
		fingerprintFn: func() string { return "stable" },
		now:           func() time.Time { return now },
		pluginFn: func(context.Context, string) (string, bool, error) {
			return "plugins", true, nil
		},
		queueFn: func(context.Context, string) ([]int, error) {
			mu.Lock()
			queueCalls++
			mu.Unlock()
			<-release
			return []int{7}, nil
		},
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make(chan []int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- cache.queueFDs(context.Background(), "vppctl", false)
		}()
	}
	// Give the first caller time to enter the probe before releasing the
	// rest; the mutex must still collapse them into one VPP read.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	mu.Lock()
	calls := queueCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent queue probes = %d, want 1", calls)
	}
	for result := range results {
		if len(result) != 1 || result[0] != 7 {
			t.Fatalf("concurrent result = %v, want [7]", result)
		}
	}
}

func TestAFXDPInterfaceFingerprintTracksNetdevAndPCI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	addAFXDPTestNetdev(t, root, "eth0", "0000:03:00.0")

	first := afXDPInterfaceFingerprint()
	if first == "" {
		t.Fatal("interface fingerprint is empty")
	}
	addAFXDPTestNetdev(t, root, "eth1", "0000:04:00.0")
	second := afXDPInterfaceFingerprint()
	if second == first {
		t.Fatal("adding a NIC did not change the fingerprint")
	}
	if first != afXDPInterfaceFingerprintAfterRemoval(t, root, "eth1") {
		t.Fatal("removing the added NIC did not restore the original fingerprint")
	}
}

func TestAFXDPFingerprintTracksVPPProcess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	addAFXDPTestNetdev(t, root, "eth0", "0000:03:00.0")
	pidFile := filepath.Join(t.TempDir(), "vpp.pid")
	t.Setenv("LY_ROUTE_VPP_PID_FILE", pidFile)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	first := afXDPFingerprint()
	if err := os.WriteFile(pidFile, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := afXDPFingerprint()
	if first == second {
		t.Fatal("a different VPP process did not change the fingerprint")
	}
}

func TestAFXDPProbeCacheOnlyRunsTheCLIWhenTheAnswerCanChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake vppctl is a shell script")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	vppctl := filepath.Join(t.TempDir(), "vppctl")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$AFXDP_CALL_LOG\"\n" +
		"case \"$*\" in\n" +
		"  'show plugins') printf '%s\\n' ' 5. af_xdp_plugin.so 26.06-release AF_XDP Device Plugin' ;;\n" +
		"  'show af_xdp') printf '%s\\n' 'rx queue 0 fd 12' ;;\n" +
		"esac\n"
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFXDP_CALL_LOG", calls)

	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	cache := &afXDPProbeCache{
		fingerprintFn: func() string { return "stable" },
		now:           func() time.Time { return now },
	}
	for i := 0; i < 20; i++ {
		now = now.Add(3 * time.Second)
		cache.queueFDs(context.Background(), vppctl, false)
	}
	now = now.Add(afXDPPluginRecheckInterval)
	cache.queueFDs(context.Background(), vppctl, false) // plugin recheck due
	lines := readAFXDPTestCalls(t, calls)
	if got := countAFXDPTestCalls(lines, "show af_xdp"); got != 1 {
		t.Fatalf("show af_xdp calls = %d, want 1 (%v)", got, lines)
	}
	if got := countAFXDPTestCalls(lines, "show plugins"); got != 2 {
		t.Fatalf("show plugins calls = %d, want 2 (%v)", got, lines)
	}

	// A transition is allowed to probe once more.
	cache.queueFDs(context.Background(), vppctl, true)
	lines = readAFXDPTestCalls(t, calls)
	if got := countAFXDPTestCalls(lines, "show af_xdp"); got != 2 {
		t.Fatalf("after forced probe show af_xdp calls = %d, want 2", got)
	}
}

func TestAFXDPProbeCacheNeverProbesWithoutThePlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake vppctl is a shell script")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	vppctl := filepath.Join(t.TempDir(), "vppctl")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$AFXDP_CALL_LOG\"\n" +
		"if [ \"$*\" = 'show plugins' ]; then printf '%s\\n' ' 4. dpdk_plugin.so 26.06-release DPDK Plugin'; fi\n" +
		"if [ \"$*\" = 'show af_xdp' ]; then exit 9; fi\n"
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFXDP_CALL_LOG", calls)

	cache := &afXDPProbeCache{
		fingerprintFn: func() string { return "stable" },
		now:           func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) },
	}
	if got := cache.queueFDs(context.Background(), vppctl, true); got != nil {
		t.Fatalf("queueFDs = %v, want nil without the plugin", got)
	}
	lines := readAFXDPTestCalls(t, calls)
	if got := countAFXDPTestCalls(lines, "show af_xdp"); got != 0 {
		t.Fatalf("show af_xdp calls = %d, want 0 (%v)", got, lines)
	}
}

func TestProbeAFXDPPluginInventoryIsOrderInsensitive(t *testing.T) {
	a, loaded, err := probeAFXDPPluginInventoryWithOutput(t, "  2. b_plugin.so\n  1. a_plugin.so\n  5. af_xdp_plugin.so\n")
	if err != nil || !loaded {
		t.Fatalf("loaded=%v err=%v, want true/nil", loaded, err)
	}
	b, loadedAgain, err := probeAFXDPPluginInventoryWithOutput(t, "  5. af_xdp_plugin.so\n  1. a_plugin.so\n  2. b_plugin.so\n")
	if err != nil || !loadedAgain {
		t.Fatalf("loaded=%v err=%v, want true/nil", loadedAgain, err)
	}
	if a != b {
		t.Fatalf("plugin fingerprints differ for the same set: %s vs %s", a, b)
	}
}

func probeAFXDPPluginInventoryWithOutput(t *testing.T, output string) (string, bool, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake vppctl is a shell script")
	}
	vppctl := filepath.Join(t.TempDir(), "vppctl")
	script := "#!/bin/sh\nprintf '%s' \"$AFXDP_PLUGIN_OUTPUT\"\n"
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFXDP_PLUGIN_OUTPUT", output)
	return probeAFXDPPluginInventory(context.Background(), vppctl)
}

func addAFXDPTestNetdev(t *testing.T, root, name, pci string) {
	t.Helper()
	device := filepath.Join(root, "devices", strings.ReplaceAll(pci, ":", "_"))
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	netdev := filepath.Join(root, "class", "net", name)
	if err := os.MkdirAll(netdev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
}

func afXDPInterfaceFingerprintAfterRemoval(t *testing.T, root, name string) string {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, "class", "net", name)); err != nil {
		t.Fatal(err)
	}
	return afXDPInterfaceFingerprint()
}

func readAFXDPTestCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func countAFXDPTestCalls(lines []string, want string) int {
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == want {
			count++
		}
	}
	return count
}
