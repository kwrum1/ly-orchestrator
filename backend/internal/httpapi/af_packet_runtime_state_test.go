package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

// AF_PACKET is only selectable after explicit consent (the environment
// acceptance switch or the persisted dataplane policy). Once selected it
// forwards traffic, so it must be reported as a running component with a
// performance note instead of degrading an otherwise committed generation.
func TestAFPacketFallbackReportsRunningAfterConsent(t *testing.T) {
	path := vpp.NativePath{Attachments: []vpp.NativeAttachment{{
		LinuxInterface: "enp0s3",
		VPPInterface:   "lyroute-enp0s3",
		Hook:           vpp.NativeHookAFPacket,
		Mode:           vpp.NativeModeAFPacket,
	}}}
	component, ok := afPacketRuntimeComponent(path)
	if !ok {
		t.Fatal("AF_PACKET path must produce an operator-visible component")
	}
	if component.State != "running" || !component.Available {
		t.Fatalf("AF_PACKET component = %#v, want a running component", component)
	}
	if component.Reason == "" {
		t.Fatal("AF_PACKET component must keep the performance limitation as its reason")
	}
	if state := runtimeState([]RuntimeComponentState{{Name: "vpp", State: "running", Available: true}, component}); state != "running" {
		t.Fatalf("runtime state with a consented AF_PACKET path = %q, want running", state)
	}
	if _, ok := afPacketRuntimeComponent(vpp.NativePath{}); ok {
		t.Fatal("a non-AF_PACKET path must not produce the fallback component")
	}
}
