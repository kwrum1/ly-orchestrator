package dataplane

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestEffectiveDPDKDriverHonoursTransportConsent(t *testing.T) {
	cases := []struct {
		env  string
		mode vpp.NativeMode
		want string
	}{
		{"", vpp.NativeModeDPDKVFIO, "vfio-pci"},
		{"", vpp.NativeModeDPDKUIO, "uio_pci_generic"},
		{"uio", vpp.NativeModeDPDKVFIO, "uio_pci_generic"},
		{"uio", vpp.NativeModeDPDKUIO, "uio_pci_generic"},
		{"vfio", vpp.NativeModeDPDKUIO, "vfio-pci"},
		{"auto", vpp.NativeModeDPDKVFIO, "vfio-pci"},
	}
	for _, test := range cases {
		t.Setenv("LY_ROUTE_DPDK_TRANSPORT", test.env)
		got, err := effectiveDPDKDriver(test.mode)
		if err != nil {
			t.Fatalf("env=%q mode=%q: %v", test.env, test.mode, err)
		}
		if got != test.want {
			t.Fatalf("env=%q mode=%q: got %q want %q", test.env, test.mode, got, test.want)
		}
	}
}

func TestRenderDPDKStartupHonoursUIOTransitionConsent(t *testing.T) {
	t.Setenv("LY_ROUTE_DPDK_TRANSPORT", "uio")
	attachments := []vpp.NativeAttachment{{
		LinuxInterface: "enp3s0",
		VPPInterface:   "lyroute-enp3s0",
		Mode:           vpp.NativeModeDPDKVFIO,
		PCIAddress:     "0000:03:00.0",
	}}
	config, err := RenderDPDKStartup([]byte("unix { nodaemon }\n"), attachments)
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	if !strings.Contains(text, "uio-driver uio_pci_generic") {
		t.Fatalf("startup config ignored LY_ROUTE_DPDK_TRANSPORT=uio:\n%s", text)
	}
	if strings.Contains(text, "uio-driver vfio-pci") {
		t.Fatalf("startup config still selects VFIO:\n%s", text)
	}
}
