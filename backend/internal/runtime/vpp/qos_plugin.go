package vpp

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// QoSTimeout bounds a capability probe. `show plugins` is a read-only CLI
// command and must never hold an apply plan for long.
const QoSTimeout = 5 * time.Second

// QoSPluginAvailable reports whether the running VPP build loaded the QoS
// plugin. A build without it still exposes some `show qos` stubs that print
// "unknown input" or an empty result, so the plugin inventory is the only
// reliable capability signal.
//
// The probe fails open: when vppctl itself cannot run (unit tests, early boot)
// the caller keeps the previous behaviour instead of silently dropping QoS
// from a plan. A failed probe therefore returns available=true with the error
// so the caller can record it as an observation degradation.
func QoSPluginAvailable(ctx context.Context, binary string) (bool, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "vppctl"
	}
	probeCtx, cancel := context.WithTimeout(ctx, QoSTimeout)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, binary, "show", "plugins").Output()
	if err != nil {
		return true, fmt.Errorf("probe VPP QoS plugin: %w", err)
	}
	return strings.Contains(string(output), "qos_plugin.so"), nil
}
