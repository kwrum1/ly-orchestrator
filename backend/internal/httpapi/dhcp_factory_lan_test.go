package httpapi

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func dnsmasqArtifactContent(t *testing.T, artifacts []service.RenderedArtifact) string {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.Service == service.Dnsmasq && artifact.Path == "/etc/dnsmasq.d/ly-route-dhcp.conf" {
			return artifact.Content
		}
	}
	t.Fatalf("dnsmasq artifact missing: %#v", artifacts)
	return ""
}

func seedLANInterface(t *testing.T, server *Server, id, cidr string) {
	t.Helper()
	if err := server.store.SaveConfig(context.Background(), persistence.ConfigDocument{
		ResourceType: "interface",
		ResourceID:   id,
		Payload:      []byte(`{"id":"` + id + `","interface_id":"` + id + `","name":"` + id + `","gateway_role":"lan","cidr":"` + cidr + `","enabled":true}`),
		PayloadHash:  "fixture-lan-" + id,
	}); err != nil {
		t.Fatalf("seed interface %s: %v", id, err)
	}
}

// The installed image subscribes a factory pool for 192.168.88.0/24 through a
// static drop-in that has no dhcp_server document. The runtime must re-derive
// that pool from the live LAN so a port swap keeps DHCP working.
func TestCurrentDHCPServersRendersFactoryPoolOnLiveLAN(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	seedLANInterface(t, server, "enp4s0", "192.168.88.1/24")

	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	plans, artifacts, _, err := server.currentDHCPServers(ctx, assignments, true)
	if err != nil {
		t.Fatalf("dhcp plan: %v", err)
	}
	if len(plans) == 0 {
		t.Fatalf("no DHCP plan derived from %#v", assignments)
	}
	content := dnsmasqArtifactContent(t, artifacts)
	for _, want := range []string{
		"interface=lylan-enp4s0\n",
		"dhcp-range=set:lyroute-factory-lylan-enp4s0,192.168.88.100,192.168.88.199,255.255.255.0,12h\n",
		"dhcp-option=tag:lyroute-factory-lylan-enp4s0,3,192.168.88.1\n",
		"dhcp-option=tag:lyroute-factory-lylan-enp4s0,6,192.168.88.1\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq artifact missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "lylan-eth2") {
		t.Fatalf("dnsmasq artifact still uses the install-time interface:\n%s", content)
	}

	// Move the LAN to another port. The next apply must render the new LCP and
	// must not keep serving the old one.
	if err := server.store.DeleteConfig(ctx, "interface", "enp4s0"); err != nil {
		t.Fatalf("delete old LAN: %v", err)
	}
	seedLANInterface(t, server, "enp5s0", "192.168.88.1/24")
	assignments, err = server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments after move: %v", err)
	}
	_, movedArtifacts, _, err := server.currentDHCPServers(ctx, assignments, true)
	if err != nil {
		t.Fatalf("dhcp plan after move: %v", err)
	}
	moved := dnsmasqArtifactContent(t, movedArtifacts)
	if !strings.Contains(moved, "interface=lylan-enp5s0\n") {
		t.Fatalf("moved LAN not rendered:\n%s", moved)
	}
	if strings.Contains(moved, "lylan-enp4s0") {
		t.Fatalf("moved LAN generation still references the old port:\n%s", moved)
	}
}

// A generation with no LAN at all must replace the install-time drop-in with an
// explicit no-service file, not return no artifact.
func TestCurrentDHCPServersWritesDisabledDropInWithoutLAN(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())

	_, artifacts, _, err := server.currentDHCPServers(ctx, nil, true)
	if err != nil {
		t.Fatalf("dhcp plan: %v", err)
	}
	content := dnsmasqArtifactContent(t, artifacts)
	if !strings.Contains(content, "# ly-route-dhcp-disabled") {
		t.Fatalf("no-LAN generation did not disable DHCP:\n%s", content)
	}
	if strings.Contains(content, "interface=") || strings.Contains(content, "dhcp-range=") {
		t.Fatalf("no-LAN generation still serves an interface or range:\n%s", content)
	}
}

// An explicit disabled dhcp_server document opts one LAN out of the
// synthesized factory pool: the multi-LAN generation still serves the other
// LAN, and the disabled LAN keeps only its DNS listener.
func TestCurrentDHCPServersHonorsExplicitlyDisabledLAN(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	seedLANInterface(t, server, "enp4s0", "192.168.88.1/24")
	seedLANInterface(t, server, "enp7s0", "192.168.99.1/24")
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "dhcp_server",
		ResourceID:   "dhcp-enp7s0",
		Payload:      []byte(`{"id":"dhcp-enp7s0","name":"disabled","enabled":false,"interface_id":"enp7s0","subnet":"192.168.99.0/24","pools":["192.168.99.100-192.168.99.199"]}`),
		PayloadHash:  "fixture-disabled-dhcp",
	}); err != nil {
		t.Fatalf("seed disabled dhcp server: %v", err)
	}

	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	_, artifacts, _, err := server.currentDHCPServers(ctx, assignments, true)
	if err != nil {
		t.Fatalf("dhcp plan: %v", err)
	}
	content := dnsmasqArtifactContent(t, artifacts)
	for _, want := range []string{
		"interface=lylan-enp4s0\n",
		"dhcp-range=set:lyroute-factory-lylan-enp4s0,192.168.88.100,192.168.88.199,255.255.255.0,12h\n",
		"interface=lylan-enp7s0\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq artifact missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "lyroute-factory-lylan-enp7s0,") || strings.Contains(content, "192.168.99.100,192.168.99.199") {
		t.Fatalf("explicitly disabled LAN still serves a range:\n%s", content)
	}
}

// A single non-factory LAN without a dhcp_server document keeps the DNS
// listener but must not keep (or inherit) any DHCP range.
func TestCurrentDHCPServersDNSOnlyLANHasNoDHCPRange(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	seedLANInterface(t, server, "enp7s0", "192.168.99.1/24")

	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	_, artifacts, _, err := server.currentDHCPServers(ctx, assignments, true)
	if err != nil {
		t.Fatalf("dhcp plan: %v", err)
	}
	content := dnsmasqArtifactContent(t, artifacts)
	if !strings.Contains(content, "interface=lylan-enp7s0") {
		t.Fatalf("LAN DNS listener is missing:\n%s", content)
	}
	if !strings.Contains(content, "# ly-route-dhcp-disabled") || strings.Contains(content, "dhcp-range=") {
		t.Fatalf("LAN without a DHCP server still serves a range:\n%s", content)
	}
}

// Every LAN with an IPv4 address gets its own interface= and .100-.199 pool.
// A second (or third) LAN must never inherit the first LAN's pool.
func TestCurrentDHCPServersRendersOneFactoryPoolPerLAN(t *testing.T) {
	tests := []struct {
		name string
		lans [][2]string
	}{
		{
			name: "two LANs",
			lans: [][2]string{{"enp4s0", "192.168.88.1/24"}, {"enp5s0", "192.168.99.1/24"}},
		},
		{
			name: "three LANs",
			lans: [][2]string{{"enp4s0", "192.168.88.1/24"}, {"enp5s0", "192.168.99.1/24"}, {"enp6s0", "192.168.100.1/24"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _ := inventoryTestServer(t)
			ctx := withInterfaceIdentities(context.Background())
			for _, lan := range test.lans {
				seedLANInterface(t, server, lan[0], lan[1])
			}
			assignments, err := server.runtimeAddressAssignments(ctx)
			if err != nil {
				t.Fatalf("assignments: %v", err)
			}
			plans, artifacts, _, err := server.currentDHCPServers(ctx, assignments, true)
			if err != nil {
				t.Fatalf("dhcp plan: %v", err)
			}
			if len(plans) != len(test.lans) {
				t.Fatalf("plans = %d, want %d: %#v", len(plans), len(test.lans), plans)
			}
			content := dnsmasqArtifactContent(t, artifacts)
			for _, lan := range test.lans {
				prefix, err := netip.ParsePrefix(lan[1])
				if err != nil {
					t.Fatal(err)
				}
				base := strings.TrimSuffix(prefix.Masked().String(), ".0/24")
				control := vpp.LANControlPlaneHostInterface(lan[0])
				for _, want := range []string{
					"interface=" + control + "\n",
					"dhcp-range=set:lyroute-factory-" + control + "," + base + ".100," + base + ".199,255.255.255.0,12h\n",
				} {
					if !strings.Contains(content, want) {
						t.Fatalf("dnsmasq artifact missing %q:\n%s", want, content)
					}
				}
			}
		})
	}
}
