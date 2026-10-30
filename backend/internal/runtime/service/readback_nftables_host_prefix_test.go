package service

import (
	"strings"
	"testing"
)

// The kernel prints a host prefix without its /32 (or /128). The readback must
// canonicalize both sides or every generation with a 255.255.255.255/32 bypass
// is reported as missing on the live table, which rolls the whole nftables
// generation back to the previous (rule-less) file.
func TestNormalizedNftablesAcceptsKernelHostPrefixRendering(t *testing.T) {
	cases := []struct{ rendered, live string }{
		{
			`ip daddr 255.255.255.255/32 udp dport 53 counter return`,
			`ip daddr 255.255.255.255 udp dport 53 counter packets 0 bytes 0 return`,
		},
		{
			`ip6 daddr ::1/128 tcp dport 53 counter return`,
			`ip6 daddr ::1 tcp dport 53 counter packets 0 bytes 0 return`,
		},
	}
	for _, test := range cases {
		if got, want := normalizedNftables(test.rendered), normalizedNftables(test.live); got != want {
			t.Fatalf("normalized %q = %q, want %q", test.rendered, got, want)
		}
	}
	// A real prefix must keep its length.
	if got := normalizedNftables(`ip daddr 192.168.88.0/24 udp dport 53 counter return`); !strings.Contains(got, "192.168.88.0/24") {
		t.Fatalf("network prefix was rewritten: %q", got)
	}
}
