package vpp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ly-route/backend/internal/vppfake"
)

// TestInterfaceRoleSwapDeletesOldAddressBeforeAddingNew pins the J1900 role
// swap contract. The old role's address must be removed before the new role's
// address is added, otherwise VPP keeps the old address and rejects the new
// one (or rejects re-adding the address that is already attached), so the
// generation can never converge.
func TestInterfaceRoleSwapDeletesOldAddressBeforeAddingNew(t *testing.T) {
	state := vppfake.NewState()
	state.Interfaces = []*vppfake.Interface{{
		Name:      "lyroute-enp4s0",
		Up:        true,
		Addresses: []string{"10.1.18.24/24"},
	}}
	channel := &interfaceStateChannel{state: state}
	prior := Snapshot{
		TransactionID: "txn-prior",
		Interfaces: []InterfaceState{{
			Name:       "lyroute-enp4s0",
			AdminState: "up",
			LinkState:  "up",
			Addresses:  []string{"10.1.18.24/24"},
		}},
	}
	desired := []InterfaceState{{
		Name:       "lyroute-enp4s0",
		AdminState: "up",
		LinkState:  "up",
		Addresses:  []string{"192.168.88.1/24"},
	}}

	result, err := (Adapter{Client: channel}).ApplyInterfaceBond(context.Background(), InterfaceBondPlan{
		TransactionID:    "txn-role-swap",
		Interfaces:       desired,
		DeleteInterfaces: []string{"lyroute-enp4s0"},
	}, prior)
	if err != nil {
		t.Fatalf("role swap apply: %v", err)
	}

	deleteIndex := commandIndex(channel.commands, "set interface ip address del lyroute-enp4s0 10.1.18.24/24")
	addIndex := commandIndex(channel.commands, "set interface ip address lyroute-enp4s0 192.168.88.1/24")
	if deleteIndex < 0 {
		t.Fatalf("old address was never deleted: %#v", channel.commands)
	}
	if addIndex < 0 {
		t.Fatalf("new address was never added: %#v", channel.commands)
	}
	if deleteIndex > addIndex {
		t.Fatalf("new address added before old address was deleted: %#v", channel.commands)
	}
	if got := state.Interfaces[0].Addresses; len(got) != 1 || got[0] != "192.168.88.1/24" {
		t.Fatalf("live addresses after role swap = %v, want only the new LAN address", got)
	}
	if !InterfaceStatesMatchDesired(result.Readback.Interfaces, desired) {
		t.Fatalf("readback does not match desired after role swap: %#v", result.Readback.Interfaces)
	}
}

// TestInterfaceDuplicateAddressAddIsIdempotent pins the other half of the
// failure: an address the previous generation already owned may still be
// attached when the replacement plan runs. That add must be an idempotent
// success, not a hard duplicate-add conflict that aborts the transaction.
func TestInterfaceDuplicateAddressAddIsIdempotent(t *testing.T) {
	state := vppfake.NewState()
	state.Interfaces = []*vppfake.Interface{{
		Name:      "lyroute-enp4s0",
		Up:        true,
		Addresses: []string{"10.1.18.24/24"},
	}}
	channel := &interfaceStateChannel{state: state}
	prior := Snapshot{
		TransactionID: "txn-prior",
		Interfaces: []InterfaceState{{
			Name:       "lyroute-enp4s0",
			AdminState: "up",
			LinkState:  "up",
			Addresses:  []string{"10.1.18.24/24"},
		}},
	}
	desired := []InterfaceState{{
		Name:       "lyroute-enp4s0",
		AdminState: "up",
		LinkState:  "up",
		Addresses:  []string{"10.1.18.24/24"},
	}}

	result, err := (Adapter{Client: channel}).ApplyInterfaceBond(context.Background(), InterfaceBondPlan{
		TransactionID:    "txn-duplicate-add",
		Interfaces:       desired,
		DeleteInterfaces: []string{"lyroute-enp4s0"},
	}, prior)
	if err != nil {
		t.Fatalf("re-applying an already attached address must succeed: %v", err)
	}
	if channel.duplicateAdds == 0 {
		t.Fatalf("test did not exercise a duplicate add: %#v", channel.commands)
	}
	add := commandIndex(channel.commands, "set interface ip address lyroute-enp4s0 10.1.18.24/24")
	if add < 0 || !strings.HasPrefix(channel.commands[add], "?") {
		t.Fatalf("duplicate add was not emitted as an optional/idempotent command: %#v", channel.commands)
	}
	if !InterfaceStatesMatchDesired(result.Readback.Interfaces, desired) {
		t.Fatalf("readback does not match desired: %#v", result.Readback.Interfaces)
	}
}

func commandIndex(commands []string, substring string) int {
	for index, command := range commands {
		if strings.Contains(command, substring) {
			return index
		}
	}
	return -1
}

// interfaceStateChannel executes interface operations against the stateful
// vppfake and answers the typed interface readback. Unlike the real vppctl
// client it deliberately fails a non-optional duplicate address add, matching
// the VPP error ("which conflicts with ... for interface ...").
type interfaceStateChannel struct {
	state         *vppfake.State
	commands      []string
	duplicateAdds int
}

func (channel *interfaceStateChannel) OpenChannel(context.Context) (Channel, error) {
	return channel, nil
}

func (channel *interfaceStateChannel) Close() error { return nil }

func (channel *interfaceStateChannel) Do(_ context.Context, operation Operation) (Reply, error) {
	switch operation.Name {
	case "vpp.interface.snapshot":
		return Reply{Payload: InterfaceReadback{Interfaces: channel.interfaces()}}, nil
	case "vpp.interface-bond.snapshot":
		return Reply{Payload: BondReadback{Bonds: channel.bonds()}}, nil
	}
	for _, raw := range operation.VPPCtlCommands {
		raw = strings.TrimSpace(raw)
		optional := strings.HasPrefix(raw, "?")
		command := strings.TrimSpace(strings.TrimPrefix(raw, "?"))
		channel.commands = append(channel.commands, raw)
		if channel.duplicateAddressAdd(command) {
			channel.duplicateAdds++
			if !optional {
				return Reply{Retval: 1}, fmt.Errorf("set interface ip address: failed to add address on interface which conflicts with itself")
			}
			continue
		}
		_, stderr, code := vppfake.Execute(channel.state, command)
		if code != 0 && !optional {
			return Reply{Retval: int32(code)}, fmt.Errorf("vppctl %q failed: %s", command, strings.TrimSpace(stderr))
		}
	}
	return Reply{}, nil
}

func (channel *interfaceStateChannel) duplicateAddressAdd(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 6 || fields[0] != "set" || fields[1] != "interface" || fields[2] != "ip" || fields[3] != "address" || fields[4] == "del" {
		return false
	}
	name, cidr := fields[4], fields[5]
	for _, item := range channel.state.Interfaces {
		if item == nil || item.Name != name {
			continue
		}
		for _, address := range item.Addresses {
			if address == cidr {
				return true
			}
		}
	}
	return false
}

func (channel *interfaceStateChannel) interfaces() []InterfaceState {
	result := make([]InterfaceState, 0, len(channel.state.Interfaces))
	for _, item := range channel.state.Interfaces {
		if item == nil {
			continue
		}
		admin := "down"
		if item.Up {
			admin = "up"
		}
		result = append(result, InterfaceState{
			Name:       item.Name,
			AdminState: admin,
			LinkState:  "up",
			Addresses:  append([]string(nil), item.Addresses...),
		})
	}
	return result
}

func (channel *interfaceStateChannel) bonds() []BondState {
	result := make([]BondState, 0, len(channel.state.Bonds))
	for _, item := range channel.state.Bonds {
		if item == nil {
			continue
		}
		result = append(result, BondState{Name: item.Name, Mode: item.Mode, Members: append([]string(nil), item.Members...)})
	}
	return result
}
