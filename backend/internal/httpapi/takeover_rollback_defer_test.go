package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A failed destructive bind stops VPP. Rebuilding the previous AF_PACKET
// attachment at that moment can only fail and would label a recoverable
// rollback as fatal; Recover restarts VPP and re-adopts it (WS-AK).
func TestRollbackDefersNativeRestoreUntilVPPIsRunning(t *testing.T) {
	root := t.TempDir()
	vppctl := filepath.Join(root, "vppctl")
	if err := os.WriteFile(vppctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	executor := systemTakeoverExecutor{server: &Server{}}
	transition := &takeoverTransition{
		Interface:    "eth1",
		Netdev:       "eth1",
		PCIAddress:   "0000:02:00.0",
		VPPInterface: "lyroute-eth1",
		FromMode:     takeoverModeAFPacket,
		ToMode:       takeoverModeDPDKUIO,
	}
	if err := executor.Rollback(context.Background(), transition, takeoverStep{Kind: "detach", Mode: takeoverModeAFPacket}); err != nil {
		t.Fatalf("rollback reported a failure while VPP was stopped for the DPDK release: %v", err)
	}
}
