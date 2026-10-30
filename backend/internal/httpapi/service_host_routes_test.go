package httpapi

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// Removing the WAN role must also remove the product-owned upstream host route.
// Leaving it points SmartDNS at a dead probe tap forever, and every LAN DNS
// query times out even though the interception itself works.
func TestEnsureServiceHostRoutesRemovesStaleProbeRouteWithoutWAN(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := context.Background()
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "dns_upstream",
		ResourceID:   "dns-direct-test",
		Payload:      []byte(`{"id":"dns-direct-test","servers":["8.8.8.8"],"enabled":true}`),
		PayloadHash:  "fixture-dns-upstream",
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	original := runHostRouteCommand
	t.Cleanup(func() { runHostRouteCommand = original })
	var calls [][]string
	runHostRouteCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 2 && args[0] == "route" && args[1] == "show" {
			return []byte("8.8.8.8 via 169.254.255.1 dev lyprobe src 169.254.255.2\n"), nil
		}
		return nil, nil
	}

	warnings := server.ensureServiceHostRoutes(ctx, false)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(calls) < 2 {
		t.Fatalf("route calls = %#v, want show+del", calls)
	}
	foundDelete := false
	for _, call := range calls {
		if got := strings.Join(call, " "); got == "route del 8.8.8.8/32 via 169.254.255.1 dev lyprobe" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("stale 8.8.8.8 probe route was not deleted: %#v", calls)
	}
	for _, call := range calls {
		if len(call) > 1 && call[1] == "replace" {
			t.Fatalf("no-WAN apply re-added the black-hole route: %#v", calls)
		}
	}
}

// A WAN line without a gateway (static /24, or DHCP before the lease) leaves
// VPP with the synthetic drop default route.  Routing SmartDNS's upstream into
// that dataplane black-holes every query; the stale probe route must be removed
// so the kernel default route keeps DNS working.
func TestEnsureServiceHostRoutesDropsRouteWhenDataplaneHasNoDefaultRoute(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := context.Background()
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "dns_upstream",
		ResourceID:   "dns-route-blackhole",
		Payload:      []byte(`{"id":"dns-route-blackhole","servers":["8.8.8.8"],"enabled":true}`),
		PayloadHash:  "fixture-dns-upstream",
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	originalRoute := runHostRouteCommand
	t.Cleanup(func() { runHostRouteCommand = originalRoute })
	var calls []string
	runHostRouteCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "route" && args[1] == "show" {
			return []byte("8.8.8.8 via 169.254.255.1 dev lyprobe src 169.254.255.2\n"), nil
		}
		return nil, nil
	}
	originalFIB := dataplaneDefaultRouteQuery
	t.Cleanup(func() { dataplaneDefaultRouteQuery = originalFIB })
	dataplaneDefaultRouteQuery = func(context.Context) (string, error) {
		return "ipv4-VRF:0, fib_index:0\n0.0.0.0/0\n  default-route refs:1 entry-flags:drop\n" +
			"  [@0]: dpo-load-balance\n    [0] [@0]: dpo-drop ip4\n", nil
	}

	warnings := server.ensureServiceHostRoutes(ctx, true)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "route replace ") {
			t.Fatalf("apply routed DNS into a dataplane without a default route: %#v", calls)
		}
	}
	foundDelete := false
	for _, call := range calls {
		if call == "route del 8.8.8.8/32 via 169.254.255.1 dev lyprobe" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("stale probe route was not removed: %#v", calls)
	}
}

// A dataplane with a real default route is still trusted; a dataplane with the
// synthetic drop default is not.
func TestDataplaneForwardsOffSubnet(t *testing.T) {
	originalFIB := dataplaneDefaultRouteQuery
	t.Cleanup(func() { dataplaneDefaultRouteQuery = originalFIB })
	cases := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{
			name:   "drop default",
			output: "0.0.0.0/0\n  default-route refs:1 entry-flags:drop\n    [0] [@0]: dpo-drop ip4\n",
			want:   false,
		},
		{
			name:   "wan default",
			output: "0.0.0.0/0\n    [0] [@4]: ipv4 via 10.1.18.1 lyroute-enp4s0: mtu:9000 next:1\n",
			want:   true,
		},
		{
			name:   "local default",
			output: "0.0.0.0/0\n    [0] [@13]: dpo-receive: 0.0.0.0 on local0\n",
			want:   true,
		},
		{name: "query failed", err: context.DeadlineExceeded, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dataplaneDefaultRouteQuery = func(context.Context) (string, error) {
				return testCase.output, testCase.err
			}
			if got := dataplaneForwardsOffSubnet(context.Background()); got != testCase.want {
				t.Fatalf("dataplaneForwardsOffSubnet = %v, want %v", got, testCase.want)
			}
		})
	}
}
