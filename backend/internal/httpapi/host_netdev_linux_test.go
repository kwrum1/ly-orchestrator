//go:build linux

package httpapi

import "testing"

func TestGROIfreqEncodesEthtoolRequest(t *testing.T) {
	request, value, err := newGROIfreq("enp5s0", ethtoolSGRO, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(request.Name[:6]); got != "enp5s0" {
		t.Fatalf("ifr_name = %q", got)
	}
	if request.Data == nil {
		t.Fatal("ifr_data is nil")
	}
	if value.Cmd != ethtoolSGRO || value.Data != 0 {
		t.Fatalf("ethtool_value = %#v", value)
	}
	if got := len(request.Name) + len(request.Pad) + 8; got != ifreqSize {
		t.Fatalf("ifreq size = %d, want %d", got, ifreqSize)
	}
}

func TestGROIfreqRejectsOversizedName(t *testing.T) {
	if _, _, err := newGROIfreq("this-name-is-too-long", ethtoolGGRO, 0); err == nil {
		t.Fatal("expected an error for an oversized interface name")
	}
}

func TestDisableHostGROIgnoresMissingInterface(t *testing.T) {
	// The takeover tests use synthetic netdev names. A missing device must not
	// mask the create error that follows, and there is no GRO to disable.
	if err := disableHostGRO("lyroute-wsad-missing0"); err != nil {
		t.Fatalf("disableHostGRO(missing) = %v", err)
	}
}
