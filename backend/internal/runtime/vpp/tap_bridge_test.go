package vpp

import "testing"

func TestTapBridgeInterfaceNames_areStableAndLinuxSafe(t *testing.T) {
	bridge, host := tapBridgeInterfaceNames(" ens33 ")
	if bridge != "lrbr-1bf492ea" || host != "lrh-1bf492ea" {
		t.Fatalf("tap bridge names = %q, %q", bridge, host)
	}
	if len(bridge) > 15 || len(host) > 15 {
		t.Fatalf("tap bridge names exceed Linux IFNAMSIZ: %q, %q", bridge, host)
	}
}

func TestDataplaneAttachOperation_buildsTapBridgeCommands(t *testing.T) {
	operation := DataplaneAttachOperation("req-tap-bridge", NativeAttachment{
		LinuxInterface: "ens33",
		VPPInterface:   "lyroute-ens33",
		Hook:           NativeHookTapBridge,
		Mode:           NativeModeTapBridge,
	})
	// Duplicate TAPs with the product name are released by the apply-level
	// cleanup, not by the attach itself: deleting here would drop the live
	// bridge TAP on every replay.
	want := []string{
		"?create tap if-name lyroute-ens33 host-if-name lrh-1bf492ea host-bridge lrbr-1bf492ea no-gso",
		"set interface state lyroute-ens33 up",
		"show hardware-interfaces lyroute-ens33",
		"show interface lyroute-ens33",
	}
	if len(operation.VPPCtlCommands) != len(want) {
		t.Fatalf("tap bridge commands = %#v", operation.VPPCtlCommands)
	}
	for index := range want {
		if operation.VPPCtlCommands[index] != want[index] {
			t.Fatalf("tap bridge command %d = %q, want %q", index, operation.VPPCtlCommands[index], want[index])
		}
	}
}
