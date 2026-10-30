package vpp

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/vppfake"
)

func securityTestLANs() []SecurityLANBinding {
	return []SecurityLANBinding{
		{Interface: "lyroute-enp3s0", Prefix: netip.MustParsePrefix("192.168.88.0/24")},
		{Interface: "lyroute-enp4s0", Prefix: netip.MustParsePrefix("192.168.99.0/24")},
	}
}

func securityTestACL() trafficpolicy.SecurityACL {
	return trafficpolicy.SecurityACL{
		ID: "acl-block-ssh", Priority: 10, Action: "deny",
		Match: trafficpolicy.Match{
			Sources: []string{"192.168.99.0/24"}, Destinations: []string{"10.1.18.100/32"},
			Protocols: []string{"tcp"}, DestPorts: []string{"22"}, Direction: "input",
		},
	}
}

// The reported P0: a rule whose source is 192.168.99.0/24 (the second LAN) was
// attached to the first LAN interface, so it never saw the traffic it named.
func TestCompileSecurityGenerationBindsACLToSourceLAN(t *testing.T) {
	generation, err := CompileSecurityGenerationForLANs("generation-w", securityTestLANs(), "lyroute-enp3s0", []trafficpolicy.SecurityACL{securityTestACL()}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 1 {
		t.Fatalf("groups = %#v, want exactly the 99.x LAN", generation.ACLs)
	}
	group := generation.ACLs[0]
	if group.Interface != "lyroute-enp4s0" || group.Direction != "input" {
		t.Fatalf("group = %#v, want lyroute-enp4s0 input", group)
	}
}

func TestCompileSecurityGenerationBindsAnyACLToEveryLAN(t *testing.T) {
	anyACL := trafficpolicy.SecurityACL{ID: "acl-any", Priority: 10, Action: "deny", Match: trafficpolicy.Match{Direction: "input"}}
	generation, err := CompileSecurityGenerationForLANs("generation-any", securityTestLANs(), "lyroute-enp3s0", []trafficpolicy.SecurityACL{anyACL}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 2 {
		t.Fatalf("groups = %#v, want one any-LAN group per LAN", generation.ACLs)
	}
	for _, group := range generation.ACLs {
		if group.Direction != "input" {
			t.Fatalf("group = %#v, want input", group)
		}
	}
}

func TestCompileSecurityGenerationBindsOutputACLByDestinationLAN(t *testing.T) {
	acl := securityTestACL()
	acl.ID = "acl-wan-to-lan"
	acl.Match.Direction = "output"
	acl.Match.Sources = []string{"10.1.18.0/24"}
	acl.Match.Destinations = []string{"192.168.88.10/32"}
	generation, err := CompileSecurityGenerationForLANs("generation-out", securityTestLANs(), "lyroute-enp4s0", []trafficpolicy.SecurityACL{acl}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 1 || generation.ACLs[0].Interface != "lyroute-enp3s0" || generation.ACLs[0].Direction != "output" {
		t.Fatalf("groups = %#v, want lyroute-enp3s0 output", generation.ACLs)
	}
}

func TestCompileSecurityGenerationExpandsBothDirectionsOnOwningLAN(t *testing.T) {
	acl := securityTestACL()
	acl.Match.Direction = "both"
	acl.Match.Destinations = []string{"192.168.88.0/24"}
	generation, err := CompileSecurityGenerationForLANs("generation-both", securityTestLANs(), "lyroute-enp3s0", []trafficpolicy.SecurityACL{acl}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 2 {
		t.Fatalf("groups = %#v, want one group per direction", generation.ACLs)
	}
	got := map[string]string{}
	for _, group := range generation.ACLs {
		got[group.Direction] = group.Interface
	}
	// input is selected by the source (99.x), output by the destination (88.x).
	if got["input"] != "lyroute-enp4s0" || got["output"] != "lyroute-enp3s0" {
		t.Fatalf("groups = %#v, want input on enp4s0 and output on enp3s0", generation.ACLs)
	}
}

var (
	securityFakeVPPOnce   sync.Once
	securityFakeVPPBinary string
	securityFakeVPPError  error
)

// securityFakeVPPCTL compiles the stateful fake vppctl once per test binary so
// the lifecycle below runs through the production exec boundary.
func securityFakeVPPCTL(t *testing.T) string {
	t.Helper()
	securityFakeVPPOnce.Do(func() {
		root, err := securityBackendModuleRoot()
		if err != nil {
			securityFakeVPPError = err
			return
		}
		directory, err := os.MkdirTemp("", "ly-route-security-fake-vppctl-")
		if err != nil {
			securityFakeVPPError = err
			return
		}
		target := filepath.Join(directory, "fakevppctl")
		build := exec.Command(filepath.Join(goruntime.GOROOT(), "bin", "go"), "build", "-o", target, "./internal/vppfake/cmd/fakevppctl")
		build.Dir = root
		if output, err := build.CombinedOutput(); err != nil {
			securityFakeVPPError = fmt.Errorf("build fake vppctl: %w: %s", err, output)
			return
		}
		securityFakeVPPBinary = target
	})
	if securityFakeVPPError != nil {
		t.Fatal(securityFakeVPPError)
	}
	return securityFakeVPPBinary
}

func securityBackendModuleRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("backend go.mod was not found above the test working directory")
		}
		directory = parent
	}
}

// The lifecycle test proves the attachment is observable in the dataplane
// state (correct sw_if_index) and that cleanup detaches a stale generation ACL
// from a LAN the new generation does not use.
func TestSecurityGenerationBindsToMatchingLANAndCleansUp(t *testing.T) {
	binary := securityFakeVPPCTL(t)
	stateDirectory := t.TempDir()
	t.Setenv("FAKE_VPPCTL_STATE_DIR", stateDirectory)
	statePath := filepath.Join(stateDirectory, "state.json")
	if err := vppfake.Seed(statePath, []*vppfake.Interface{
		{Name: "lyroute-enp3s0", Up: true, Addresses: []string{"192.168.88.1/24"}},
		{Name: "lyroute-enp4s0", Up: true, Addresses: []string{"192.168.99.1/24"}},
	}, "lyroute-enp3s0", "192.168.88.0/24"); err != nil {
		t.Fatal(err)
	}
	state, err := vppfake.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"set acl-plugin acl index 90001 permit ip any any tag ly-route-security-gen-lyroute-enp3s0_input",
		"set acl-plugin interface lyroute-enp3s0 input acl 90001",
	} {
		if _, stderr, code := vppfake.Execute(state, command); code != 0 {
			t.Fatalf("seed %q: %s", command, stderr)
		}
	}
	if err := state.Save(statePath); err != nil {
		t.Fatal(err)
	}

	generation, err := CompileSecurityGenerationForLANs("gateway-security", securityTestLANs(), "lyroute-enp3s0", []trafficpolicy.SecurityACL{securityTestACL()}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := securityGenerationCommands(generation)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operation := Operation{Name: "vpp.security-generation", RequestID: "txn-security-w", Resource: generation.ID, Payload: generation, VPPCtlCommands: commands}
	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatal(err)
	}

	state, err = vppfake.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	enp3s0 := securityFakeInterface(t, state, "lyroute-enp3s0")
	enp4s0 := securityFakeInterface(t, state, "lyroute-enp4s0")
	if len(enp3s0.InputACLs) != 0 {
		t.Fatalf("enp3s0 input ACLs = %v, want none (stale generation ACL detached)", enp3s0.InputACLs)
	}
	if len(enp4s0.InputACLs) != 1 {
		t.Fatalf("enp4s0 input ACLs = %v, want the 99.x rule", enp4s0.InputACLs)
	}
	attached := state.ACLs[0]
	if attached.Tag != "ly-route-security-gen-lyroute_enp4s0_input" {
		t.Fatalf("ACL tag = %q, want the matching LAN", attached.Tag)
	}
	show, _, _ := vppfake.Execute(state, "show acl-plugin interface")
	if !strings.Contains(show, fmt.Sprintf("sw_if_index %d:", enp4s0.Index)) || strings.Contains(show, fmt.Sprintf("sw_if_index %d:", enp3s0.Index)) {
		t.Fatalf("show acl-plugin interface = %q, want only enp4s0 sw_if_index %d", show, enp4s0.Index)
	}

	deleting := operation
	deleting.Name = "vpp.security-generation.rollback-delete"
	if _, err := channel.Do(context.Background(), deleting); err != nil {
		t.Fatal(err)
	}
	state, err = vppfake.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ACLs) != 0 {
		t.Fatalf("ACLs after cleanup = %#v, want none", state.ACLs)
	}
	for _, item := range state.Interfaces {
		if len(item.InputACLs)+len(item.OutputACLs) != 0 {
			t.Fatalf("interface %s keeps ACLs %v/%v after cleanup", item.Name, item.InputACLs, item.OutputACLs)
		}
	}
}

func securityFakeInterface(t *testing.T, state *vppfake.State, name string) *vppfake.Interface {
	t.Helper()
	for _, item := range state.Interfaces {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("interface %q not found in fake state", name)
	return nil
}
