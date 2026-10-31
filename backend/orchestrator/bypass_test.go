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

func TestNormalizeBypassGroupConfig(t *testing.T) {
	config, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, Groups: []orchestratorapi.BypassGroupConfig{
		{Name: "grp-b", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.168.20.82", DstPort: 53, Protocol: "UDP"}}},
		{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.168.10.82", DstPort: 443}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Groups) != 2 || config.Groups[0].Name != "grp-a" || config.Groups[1].Name != "grp-b" {
		t.Fatalf("group order = %#v", config.Groups)
	}
	if config.Groups[0].WindowSeconds != 10 || config.Groups[0].FailureThreshold != 3 || config.Groups[0].RecoveryThreshold != 3 {
		t.Fatalf("group defaults = %#v", config.Groups[0])
	}
	if config.Groups[1].FlowTargets[0].Protocol != "udp" {
		t.Fatalf("protocol normalization = %#v", config.Groups[1].FlowTargets[0])
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, Groups: []orchestratorapi.BypassGroupConfig{{Name: "grp-a"}, {Name: "grp-a"}}}); err == nil {
		t.Fatal("duplicate group must be rejected")
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, Groups: []orchestratorapi.BypassGroupConfig{{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "bad", DstPort: 80}}}}}); err == nil {
		t.Fatal("invalid flow destination must be rejected")
	}
	if _, err := normalizeBypassConfig(orchestratorapi.BypassConfig{Enabled: true, Groups: []orchestratorapi.BypassGroupConfig{{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.0.2.1", DstPort: 80, Protocol: "icmp"}}}}}); err == nil {
		t.Fatal("unsupported flow protocol must be rejected")
	}
}

// A window without business traffic is not a fault: the group stays unknown
// and must never be bypassed for lack of traffic.
func TestPassiveWindowWithoutTrafficDoesNotBypass(t *testing.T) {
	state := &bypassGroupRuntime{name: "grp-a"}
	config := orchestratorapi.BypassGroupConfig{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.0.2.10", DstPort: 443, Protocol: "tcp"}}}
	now := time.Now().UTC()
	state.observeFlow(now, nil, 10*time.Second)
	state.observeFlow(now.Add(12*time.Second), nil, 10*time.Second)
	if state.windowAttempts != 0 || state.windowSuccess != 0 {
		t.Fatalf("counters = %#v", state)
	}
	if health := groupHealth(state, config, true); health != "unknown" {
		t.Fatalf("health = %q, want unknown", health)
	}
}

// Requests with only RST (or no response at all) inside the window are the
// only passive failure evidence.
func TestPassiveWindowAllRSTIsFaulty(t *testing.T) {
	state := &bypassGroupRuntime{name: "grp-a"}
	config := orchestratorapi.BypassGroupConfig{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.0.2.10", DstPort: 443, Protocol: "tcp"}}}
	now := time.Now().UTC()
	first := []orchestratorapi.BypassFlowStats{{Group: "grp-a", DstIP: "192.0.2.10", DstPort: 443, Protocol: "tcp", Attempts: 5, Syn: 5, RST: 5}}
	second := []orchestratorapi.BypassFlowStats{{Group: "grp-a", DstIP: "192.0.2.10", DstPort: 443, Protocol: "tcp", Attempts: 10, Syn: 10, RST: 10}}
	state.observeFlow(now, nil, 10*time.Second)
	state.observeFlow(now.Add(2*time.Second), first, 10*time.Second)
	state.observeFlow(now.Add(11*time.Second), second, 10*time.Second)
	if state.windowAttempts != 10 || state.windowSuccess != 0 || state.windowRST != 10 || !state.windowReady {
		t.Fatalf("window = attempts %d success %d rst %d ready %t", state.windowAttempts, state.windowSuccess, state.windowRST, state.windowReady)
	}
	if health := groupHealth(state, config, true); health != "faulty" {
		t.Fatalf("health = %q, want faulty", health)
	}
}

// Any reverse packet is a success for UDP request/response services; a DNS
// error answer (NXDOMAIN/SERVFAIL) is still a response and must not bypass.
func TestPassiveWindowUDPResponseRecovers(t *testing.T) {
	state := &bypassGroupRuntime{name: "grp-a", enabled: true}
	config := orchestratorapi.BypassGroupConfig{Name: "grp-a", FlowTargets: []orchestratorapi.BypassFlowTarget{{DstIP: "192.0.2.53", DstPort: 53, Protocol: "udp"}}}
	now := time.Now().UTC()
	stats := []orchestratorapi.BypassFlowStats{{Group: "grp-a", DstIP: "192.0.2.53", DstPort: 53, Protocol: "udp", Attempts: 4, Responses: 4, Success: 4}}
	state.observeFlow(now, nil, 10*time.Second)
	state.observeFlow(now.Add(2*time.Second), stats, 10*time.Second)
	if state.windowSuccess != 4 {
		t.Fatalf("window success = %d", state.windowSuccess)
	}
	state.enabled = false
	if health := groupHealth(state, config, true); health != "healthy" {
		t.Fatalf("health = %q, want healthy", health)
	}
}

func TestTopologyGroupInterfacesMapsNames(t *testing.T) {
	groups := topologyGroupInterfaces(orchestratorViewForTest())
	if len(groups["grp-a"]) != 2 || groups["grp-a"][0] != "ens160" || groups["grp-b"][1] != "ens161" {
		t.Fatalf("group interfaces = %#v", groups)
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
