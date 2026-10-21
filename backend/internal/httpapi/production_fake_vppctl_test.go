package httpapi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
	"ly-route/backend/internal/vppfake"
)

// The production gateway proof needs a data plane that behaves like VPP, not a
// command → bytes table: the transaction order is only meaningful when a
// readback issued before a mutation provably cannot observe the object the
// mutation creates. fakevppctl implements that state machine (see the
// vppfake package). It is compiled once per test binary and invoked through
// the real vppctl client, so the exec boundary, stdout/stderr/exit-code
// handling, and command grammar stay on the production code path.
var (
	fakeVPPOnce   sync.Once
	fakeVPPBinary string
	fakeVPPError  error
)

func fakeVPPCTL(t *testing.T) string {
	t.Helper()
	fakeVPPOnce.Do(func() {
		root, err := backendModuleRoot()
		if err != nil {
			fakeVPPError = err
			return
		}
		directory, err := os.MkdirTemp("", "ly-route-fake-vppctl-")
		if err != nil {
			fakeVPPError = err
			return
		}
		target := filepath.Join(directory, "fakevppctl")
		build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", target, "./internal/vppfake/cmd/fakevppctl")
		build.Dir = root
		if output, err := build.CombinedOutput(); err != nil {
			fakeVPPError = fmt.Errorf("build fake vppctl: %w: %s", err, output)
			return
		}
		fakeVPPBinary = target
	})
	if fakeVPPError != nil {
		t.Fatal(fakeVPPError)
	}
	return fakeVPPBinary
}

func backendModuleRoot() (string, error) {
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
			return "", errors.New("backend go.mod was not found above the test working directory")
		}
		directory = parent
	}
}

// seedFixtureState represents the prior committed generation in the fake data
// plane. Only objects the prior plan proves are seeded; everything the apply
// under test creates must be created by the transaction's own commands.
func seedFixtureState(t *testing.T, plan vpp.Plan, prior vpp.Plan) string {
	t.Helper()
	directory := t.TempDir()
	stateDirectory := filepath.Join(directory, "vpp-state")
	lanInterface, lanPrefix := "", ""
	for _, assignment := range plan.AddressAssignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") && assignment.VPPInterface != "" {
			lanInterface = assignment.VPPInterface
			lanPrefix = maskedPrefix(assignment.CIDR)
			break
		}
	}
	interfaces := make([]*vppfake.Interface, 0, len(prior.Interfaces))
	for _, state := range prior.Interfaces {
		interfaces = append(interfaces, &vppfake.Interface{
			Name:      state.Name,
			Up:        state.AdminState == "up",
			Addresses: append([]string(nil), state.Addresses...),
		})
	}
	if err := vppfake.Seed(filepath.Join(stateDirectory, "state.json"), interfaces, lanInterface, lanPrefix); err != nil {
		t.Fatal(err)
	}
	return stateDirectory
}

func maskedPrefix(cidr string) string {
	cidr = strings.TrimSpace(cidr)
	address, suffix, found := strings.Cut(cidr, "/")
	if !found {
		return cidr
	}
	octets := strings.Split(address, ".")
	if len(octets) != 4 {
		return cidr
	}
	bits := 32
	_, _ = fmt.Sscanf(suffix, "%d", &bits)
	numbers := make([]int, 4)
	for index, octet := range octets {
		_, _ = fmt.Sscanf(octet, "%d", &numbers[index])
	}
	mask := uint32(0xffffffff)
	if bits < 32 {
		mask = ^(uint32(1<<(32-bits)) - 1)
	}
	if bits == 0 {
		mask = 0
	}
	value := uint32(numbers[0])<<24 | uint32(numbers[1])<<16 | uint32(numbers[2])<<8 | uint32(numbers[3])
	value &= mask
	return fmt.Sprintf("%d.%d.%d.%d/%d", byte(value>>24), byte(value>>16), byte(value>>8), byte(value), bits)
}

// sortedResponseCommands is retained for diagnostics in other proof helpers.
func sortedResponseCommands(responses map[string]string) []string {
	commands := make([]string, 0, len(responses))
	for command := range responses {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}
