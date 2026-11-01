package httpapi

import "testing"

func TestVPPInterfaceNameKeepsBondIdentity(t *testing.T) {
	for _, name := range []string{"bond-home-lan", "BondEthernet12"} {
		if got := vppInterfaceName(name); got != name {
			t.Fatalf("vppInterfaceName(%q) = %q, want the bond name unchanged", name, got)
		}
	}
	if got := vppInterfaceName("enp2s0"); got != "lyroute-enp2s0" {
		t.Fatalf("vppInterfaceName(enp2s0) = %q", got)
	}
}

func TestParseLinkSpeedMbps(t *testing.T) {
	cases := map[string]int{
		"1000Mb/s": 1000,
		"1Gb/s":    1000,
		"2500Mbps": 2500,
		"10000":    10000,
		"":         0,
		"down":     0,
	}
	for input, want := range cases {
		if got := parseLinkSpeedMbps(input); got != want {
			t.Fatalf("parseLinkSpeedMbps(%q) = %d, want %d", input, got, want)
		}
	}
}
