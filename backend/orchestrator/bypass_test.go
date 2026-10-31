package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	orchdomain "ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/orchestratorapi"
)

func TestNormalizeBypassConfigDefaultsAndValidation(t *testing.T) {
	config, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if config.IntervalSeconds != 2 || config.TimeoutMilliseconds != 1000 || config.FailureThreshold != 3 || config.RecoveryThreshold != 3 {
		t.Fatalf("defaults = %#v", config)
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, FailureThreshold: -1}); err == nil {
		t.Fatal("negative failure threshold must be rejected")
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, TCPTargets: []string{"192.168.10.82"}}); err == nil {
		t.Fatal("tcp target without port must be rejected")
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, ICMPTargets: []string{"not-an-ip"}}); err == nil {
		t.Fatal("invalid icmp target must be rejected")
	}
}

func TestProbeInterfaceReadsOperstateAndCarrier(t *testing.T) {
	root := t.TempDir()
	interfaceDir := filepath.Join(root, "class", "net", "ens224")
	if err := os.MkdirAll(interfaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	probe := probeInterface(root, "ens224", now)
	if probe.OK || probe.Detail != "operstate_unreadable" {
		t.Fatalf("missing operstate = %#v", probe)
	}
	if err := os.WriteFile(filepath.Join(interfaceDir, "operstate"), []byte("up\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interfaceDir, "carrier"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	probe = probeInterface(root, "ens224", now)
	if !probe.OK {
		t.Fatalf("healthy interface = %#v", probe)
	}
	if err := os.WriteFile(filepath.Join(interfaceDir, "operstate"), []byte("down\n"), 0644); err != nil {
		t.Fatal(err)
	}
	probe = probeInterface(root, "ens224", now)
	if probe.OK || probe.Detail != "operstate=down carrier=1" {
		t.Fatalf("down interface = %#v", probe)
	}
	if err := os.WriteFile(filepath.Join(interfaceDir, "operstate"), []byte("up\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interfaceDir, "carrier"), []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	probe = probeInterface(root, "ens224", now)
	if probe.OK || probe.Detail != "operstate=up carrier=0" {
		t.Fatalf("no-carrier interface = %#v", probe)
	}
}

func TestTopologyProbeInterfacesIncludesBoundaryAndGroupPorts(t *testing.T) {
	view := orchestratorViewForTest()
	names := topologyProbeInterfaces(view)
	want := []string{"ens160", "ens161", "ens192", "ens193", "ens224", "ens256"}
	if len(names) != len(want) {
		t.Fatalf("interfaces = %#v", names)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("interfaces[%d] = %q, want %q", index, names[index], want[index])
		}
	}
}

func orchestratorViewForTest() orchdomain.TopologyView {
	return orchdomain.TopologyView{
		Interfaces: []orchdomain.InterfaceView{
			{Port: "ens192", Role: orchdomain.RoleLAN},
			{Port: "ens256", Role: orchdomain.RoleWAN},
		},
		Groups: []orchdomain.GroupView{
			{Name: "grp-a", Ports: []orchdomain.DirectedPortView{
				{Interface: "ens160", Direction: orchdomain.DirectionLANFacing},
				{Interface: "ens224", Direction: orchdomain.DirectionWANFacing},
			}},
			{Name: "grp-b", Ports: []orchdomain.DirectedPortView{
				{Interface: "ens193", Direction: orchdomain.DirectionLANFacing},
				{Interface: "ens161", Direction: orchdomain.DirectionWANFacing},
			}},
		},
	}
}
