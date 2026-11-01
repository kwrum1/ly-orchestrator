package vpp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
)

type InterfaceBondPlan struct {
	TransactionID        string
	ManagementInterface  string
	Interfaces           []InterfaceState
	Bonds                []BondState
	DeleteInterfaces     []string
	DeleteBonds          []string
	DeleteInterfaceState []InterfaceState
	DeleteBondState      []BondState
}

type RollbackResult string

const (
	RollbackSucceeded RollbackResult = "succeeded"
	RollbackFailed    RollbackResult = "failed"
)

type InterfaceBondLifecycleError struct {
	Operation      string
	Cause          error
	Rollback       error
	RollbackResult RollbackResult
}

func (err *InterfaceBondLifecycleError) Error() string {
	if err.Rollback != nil {
		return fmt.Sprintf("interface/bond operation %s failed: %v; rollback %s: %v", err.Operation, err.Cause, err.RollbackResult, err.Rollback)
	}
	return fmt.Sprintf("interface/bond operation %s failed: %v; rollback %s", err.Operation, err.Cause, err.RollbackResult)
}

func (err *InterfaceBondLifecycleError) Unwrap() error { return errors.Join(err.Cause, err.Rollback) }

type InterfaceBondApplyResult struct {
	Receipt  Receipt
	Readback Snapshot
}

func BuildInterfaceBondOperations(plan InterfaceBondPlan) ([]Operation, error) {
	transactionID := strings.TrimSpace(plan.TransactionID)
	if transactionID == "" {
		return nil, fmt.Errorf("%w: transaction ID is required", ErrSnapshotIncomplete)
	}
	management := strings.TrimSpace(plan.ManagementInterface)
	operations := make([]Operation, 0, len(plan.Interfaces)+len(plan.Bonds)+len(plan.DeleteInterfaces)+len(plan.DeleteBonds))
	bondIDs := make(map[int]string, len(plan.Bonds))
	for _, bond := range plan.Bonds {
		for _, member := range bond.Members {
			if strings.TrimSpace(member) == management && management != "" {
				return nil, interfaceBondManagementLock(management, "vpp.interface-bond")
			}
		}
		bondID, _ := vppBondIdentity(bond.Name)
		if existing, collision := bondIDs[bondID]; collision && existing != bond.Name {
			return nil, fmt.Errorf("bond names %q and %q resolve to the same VPP interface id %d", existing, bond.Name, bondID)
		}
		bondIDs[bondID] = bond.Name
		commands := createBondCommands(bond)
		operations = append(operations, Operation{Name: "vpp.interface-bond", RequestID: transactionID, Resource: bond.Name, Payload: bond, VPPCtlCommands: commands})
	}
	applied := make(map[string]InterfaceState, len(plan.Interfaces))
	for _, state := range plan.Interfaces {
		name := strings.TrimSpace(state.Name)
		if name == management && management != "" {
			return nil, interfaceBondManagementLock(management, "vpp.interface.address")
		}
		applied[name] = state
		previous, replacing := interfaceStateByName(plan.DeleteInterfaceState, name)
		commands := []string{fmt.Sprintf("set interface state %s %s", name, state.AdminState)}
		if replacing {
			// A role/address change on a surviving interface must retire the
			// addresses the new role no longer owns before the replacement
			// address is added. VPP rejects adding an address that is already
			// attached, and leaving the old address in place can also make the
			// new subnet fail to install. Emission order is part of the
			// contract: delete stale addresses first, then add wanted ones.
			commands = append(commands, staleAddressDeleteCommands(name, staleInterfaceAddresses(previous.Addresses, state.Addresses))...)
		}
		for _, address := range state.Addresses {
			address = strings.TrimSpace(address)
			if address == "" {
				continue
			}
			if replacing && interfaceStateHasAddress(previous, address) {
				// The previous generation already owned this address. Make the
				// add optional so an address that is still present (for example
				// after a partial apply) is an idempotent success instead of the
				// duplicate-add conflict seen on the J1900.
				commands = append(commands, fmt.Sprintf("?set interface ip address %s %s", name, address))
				continue
			}
			commands = append(commands, fmt.Sprintf("set interface ip address %s %s", name, address))
		}
		operations = append(operations, Operation{Name: "vpp.interface.address", RequestID: transactionID, Resource: name, Payload: state, VPPCtlCommands: commands})
	}
	for _, name := range plan.DeleteInterfaces {
		if strings.TrimSpace(name) == management && management != "" {
			return nil, interfaceBondManagementLock(management, "vpp.interface.address")
		}
		state, found := interfaceStateByName(plan.DeleteInterfaceState, name)
		if !found {
			state, found = interfaceStateByName(plan.Interfaces, name)
		}
		if !found {
			return nil, fmt.Errorf("%w: prior interface state %q is required for deletion", ErrSnapshotIncomplete, name)
		}
		if _, survives := applied[strings.TrimSpace(name)]; survives {
			// A surviving interface was already handled above: its stale
			// addresses were deleted before the desired addresses were added.
			continue
		}
		operations = append(operations, Operation{Name: "vpp.interface.address", RequestID: transactionID, Resource: name, VPPCtlCommands: deleteInterfaceCommands(state)})
	}
	for _, name := range plan.DeleteBonds {
		operations = append(operations, Operation{Name: "vpp.interface-bond", RequestID: transactionID, Resource: name, VPPCtlCommands: []string{fmt.Sprintf("delete bond %s", name)}})
	}
	return operations, nil
}

func staleInterfaceAddresses(previous, desired []string) []string {
	keep := make(map[string]struct{}, len(desired))
	for _, address := range desired {
		if address = strings.TrimSpace(address); address != "" {
			keep[address] = struct{}{}
		}
	}
	stale := make([]string, 0, len(previous))
	for _, address := range previous {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, found := keep[address]; found {
			continue
		}
		stale = append(stale, address)
	}
	return stale
}

func staleAddressDeleteCommands(name string, addresses []string) []string {
	commands := make([]string, 0, len(addresses))
	for _, cidr := range addresses {
		// Optional: a stale address may already be gone after a partial or
		// replayed apply. Its absence must not block adding the new address.
		commands = append(commands, fmt.Sprintf("?set interface ip address del %s %s", name, cidr))
	}
	return commands
}

func interfaceStateHasAddress(state InterfaceState, address string) bool {
	address = strings.TrimSpace(address)
	for _, candidate := range state.Addresses {
		if strings.TrimSpace(candidate) == address {
			return true
		}
	}
	return false
}

func interfaceBondManagementLock(management, operation string) error {
	return &DataplaneLockedError{Prerequisites: []PrerequisiteResult{{Name: "management_excluded_from_operations", Interface: management, Passed: false, Reason: "operation " + operation + " references the management interface"}}}
}

func (a Adapter) ApplyInterfaceBond(ctx context.Context, plan InterfaceBondPlan, prior Snapshot, attempted ...InterfaceBondPlan) (InterfaceBondApplyResult, error) {
	plan.DeleteInterfaceState = appendInterfaceDeleteState(plan.DeleteInterfaceState, plan.DeleteInterfaces, prior.Interfaces)
	plan.DeleteBondState = appendBondDeleteState(plan.DeleteBondState, plan.DeleteBonds, prior.Bonds)
	rollbackPlan := plan
	if len(attempted) > 0 {
		rollbackPlan = attempted[0]
	}
	if len(plan.Interfaces) == 0 && len(plan.DeleteInterfaces) == 0 && len(plan.Bonds) == 0 && len(plan.DeleteBonds) == 0 {
		// Nothing to mutate or prove. A readback request built from an empty
		// plan would fall back to the default capability set and then fail with
		// "no interface state returned", which used to mask an otherwise
		// successful rollback.
		return InterfaceBondApplyResult{Receipt: Receipt{RequestID: plan.TransactionID}, Readback: prior}, nil
	}
	operations, err := BuildInterfaceBondOperations(plan)
	if err != nil {
		return InterfaceBondApplyResult{}, err
	}
	if a.Client == nil {
		return InterfaceBondApplyResult{}, fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return InterfaceBondApplyResult{}, fmt.Errorf("%w: open apply channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	for _, operation := range operations {
		if _, err := doOperation(ctx, channel, operation); err != nil {
			return InterfaceBondApplyResult{}, a.interfaceBondFailure(ctx, channel, plan.TransactionID, operation.Name, err, prior, rollbackPlan)
		}
	}
	readback, err := a.Snapshot(ctx, lifecycleSnapshotRequest(plan.TransactionID, plan))
	if err != nil {
		return InterfaceBondApplyResult{}, a.interfaceBondFailure(ctx, channel, plan.TransactionID, "readback", err, prior, rollbackPlan)
	}
	return InterfaceBondApplyResult{Receipt: Receipt{RequestID: plan.TransactionID, Operations: operations}, Readback: readback}, nil
}

func (a Adapter) interfaceBondFailure(ctx context.Context, channel Channel, transactionID, operation string, cause error, prior Snapshot, current InterfaceBondPlan) error {
	// Replay the previous generation first, then remove what the failed plan
	// introduced. Doing this in the other order deleted the surviving LAN
	// address before it could be replayed and created an addressless window.
	rollbackErr := errors.Join(applySnapshot(ctx, channel, transactionID, prior), cleanupInterfaceBond(ctx, channel, transactionID, current, prior))
	if len(prior.Interfaces) > 0 || len(prior.Bonds) > 0 {
		// The rollback readback is verification-only. It must tolerate an
		// empty request instead of switching to the default capability set and
		// reporting "no interface state returned" for a successful restore.
		request := lifecycleSnapshotRequest(transactionID, InterfaceBondPlan{Interfaces: prior.Interfaces, Bonds: prior.Bonds})
		request.AllowMissing = true
		readback, err := a.Snapshot(ctx, request)
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		} else if !reflect.DeepEqual(readback.Interfaces, prior.Interfaces) || !reflect.DeepEqual(readback.Bonds, prior.Bonds) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback readback does not match prior snapshot"))
		}
	}
	result := RollbackSucceeded
	if rollbackErr != nil {
		result = RollbackFailed
	}
	return &InterfaceBondLifecycleError{Operation: operation, Cause: cause, Rollback: rollbackErr, RollbackResult: result}
}

// cleanupInterfaceBond removes state the failed plan introduced. An interface
// that also exists in the previous generation is never brought down: only the
// addresses the failed plan added are dropped, and the caller replays the
// previous addresses first so the LAN always keeps one.
func cleanupInterfaceBond(ctx context.Context, channel Channel, transactionID string, plan InterfaceBondPlan, prior Snapshot) error {
	var cleanup []error
	for _, bond := range plan.Bonds {
		if _, survives := bondStateByName(prior.Bonds, bond.Name); survives {
			continue
		}
		if _, err := doOperation(ctx, channel, Operation{Name: "vpp.interface-bond.rollback-delete", RequestID: transactionID, Resource: bond.Name, VPPCtlCommands: []string{fmt.Sprintf("delete bond %s", bond.Name)}}); err != nil {
			cleanup = append(cleanup, err)
		}
	}
	for _, state := range plan.Interfaces {
		name := strings.TrimSpace(state.Name)
		if name == "" {
			continue
		}
		previous, survives := interfaceStateByName(prior.Interfaces, name)
		if !survives {
			if _, err := doOperation(ctx, channel, Operation{Name: "vpp.interface.address.rollback-delete", RequestID: transactionID, Resource: name, VPPCtlCommands: deleteInterfaceCommands(state)}); err != nil {
				cleanup = append(cleanup, err)
			}
			continue
		}
		stale := staleInterfaceAddresses(state.Addresses, previous.Addresses)
		if len(stale) == 0 {
			continue
		}
		if _, err := doOperation(ctx, channel, Operation{Name: "vpp.interface.address.rollback-stale", RequestID: transactionID, Resource: name, VPPCtlCommands: optionalAddressDeleteCommands(name, stale)}); err != nil {
			cleanup = append(cleanup, err)
		}
	}
	return errors.Join(cleanup...)
}

// optionalAddressDeleteCommands drops addresses the failed generation added.
// The command is optional so restoring a generation that never had a
// particular address cannot fail on an already-absent CIDR.
func optionalAddressDeleteCommands(name string, addresses []string) []string {
	commands := make([]string, 0, len(addresses))
	for _, cidr := range addresses {
		commands = append(commands, fmt.Sprintf("?set interface ip address del %s %s", name, cidr))
	}
	return commands
}

// BuildInterfaceRollbackCleanupOperations tears down only interfaces the failed
// generation created. A surviving interface is a prerequisite of the previous
// generation and must keep its address: RollbackRestore replays it, and
// RemoveStaleInterfaceAddresses removes this generation's extra address after
// the replay (build-then-delete).
func BuildInterfaceRollbackCleanupOperations(plan InterfaceBondPlan, prior []InterfaceState) ([]Operation, error) {
	transactionID := strings.TrimSpace(plan.TransactionID)
	if transactionID == "" {
		return nil, fmt.Errorf("%w: transaction ID is required", ErrSnapshotIncomplete)
	}
	operations := make([]Operation, 0, len(plan.Interfaces))
	for _, state := range plan.Interfaces {
		name := strings.TrimSpace(state.Name)
		if name == "" {
			continue
		}
		if _, survives := interfaceStateByName(prior, name); survives {
			continue
		}
		operations = append(operations, Operation{Name: "vpp.interface.address.rollback-delete", RequestID: transactionID, Resource: name, VPPCtlCommands: deleteInterfaceCommands(state)})
	}
	return operations, nil
}

// RemoveStaleInterfaceAddresses deletes addresses that the failed generation
// introduced but the previous generation never owned. It must run after the
// previous addresses were replayed, never before, so the interface is never
// addressless. The interface admin state is deliberately untouched.
func (a Adapter) RemoveStaleInterfaceAddresses(ctx context.Context, transactionID string, prior, attempted []InterfaceState) error {
	operations := make([]Operation, 0, len(attempted))
	for _, state := range attempted {
		name := strings.TrimSpace(state.Name)
		if name == "" {
			continue
		}
		previous, survives := interfaceStateByName(prior, name)
		if !survives {
			continue
		}
		stale := staleInterfaceAddresses(state.Addresses, previous.Addresses)
		if len(stale) == 0 {
			continue
		}
		operations = append(operations, Operation{Name: "vpp.interface.address.rollback-stale", RequestID: strings.TrimSpace(transactionID), Resource: name, VPPCtlCommands: optionalAddressDeleteCommands(name, stale)})
	}
	if len(operations) == 0 {
		return nil
	}
	if a.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open stale address cleanup channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	var failures []error
	for _, operation := range operations {
		if _, err := doOperation(ctx, channel, operation); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func deleteInterfaceCommands(state InterfaceState) []string {
	commands := []string{fmt.Sprintf("set interface state %s down", state.Name)}
	for _, cidr := range state.Addresses {
		// Deletion is replayed by both apply and rollback. An address that is
		// already absent must not turn a successful teardown into a failure.
		commands = append(commands, fmt.Sprintf("?set interface ip address del %s %s", state.Name, cidr))
	}
	return commands
}

func interfaceStateByName(states []InterfaceState, name string) (InterfaceState, bool) {
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}
	return InterfaceState{}, false
}

func appendInterfaceDeleteState(existing []InterfaceState, ids []string, prior []InterfaceState) []InterfaceState {
	states := append([]InterfaceState(nil), existing...)
	for _, id := range ids {
		if _, found := interfaceStateByName(states, id); found {
			continue
		}
		if state, found := interfaceStateByName(prior, id); found {
			states = append(states, state)
		}
	}
	return states
}

func applySnapshot(ctx context.Context, channel Channel, transactionID string, snapshot Snapshot) error {
	var replay []error
	for _, operation := range snapshotOperations(transactionID, snapshot) {
		if _, err := doOperation(ctx, channel, operation); err != nil {
			replay = append(replay, err)
		}
	}
	return errors.Join(replay...)
}

func doOperation(ctx context.Context, channel Channel, operation Operation) (Reply, error) {
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return Reply{}, &VPPError{Operation: operation.Name, RequestID: operation.RequestID, Err: err}
	}
	if err := NormalizeRetval(operation.Name, operation.RequestID, reply.Retval); err != nil {
		return Reply{}, err
	}
	return reply, nil
}

func snapshotOperations(transactionID string, snapshot Snapshot) []Operation {
	operations := make([]Operation, 0, len(snapshot.Interfaces)+len(snapshot.Bonds))
	for _, state := range snapshot.Interfaces {
		commands := []string{fmt.Sprintf("set interface state %s %s", state.Name, state.AdminState)}
		for _, address := range state.Addresses {
			// Rollback replays an address inventory that may already be live.
			// A duplicate add is a successful no-op, not a rollback failure.
			commands = append(commands, fmt.Sprintf("?set interface ip address %s %s", state.Name, address))
		}
		operations = append(operations, Operation{Name: "vpp.interface.address.rollback", RequestID: transactionID, Resource: state.Name, VPPCtlCommands: commands})
	}
	for _, bond := range snapshot.Bonds {
		commands := createBondCommands(bond)
		operations = append(operations, Operation{Name: "vpp.interface-bond.rollback", RequestID: transactionID, Resource: bond.Name, VPPCtlCommands: commands})
	}
	return operations
}

func interfaceNames(states []InterfaceState) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	return names
}

func bondNames(states []BondState) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	return names
}

func lifecycleSnapshotRequest(transactionID string, plan InterfaceBondPlan) SnapshotRequest {
	interfaces := append([]InterfaceState(nil), plan.Interfaces...)
	bonds := append([]BondState(nil), plan.Bonds...)
	capabilities := make([]SnapshotCapability, 0, 2)
	if len(interfaces) > 0 || len(plan.DeleteInterfaces) > 0 {
		capabilities = append(capabilities, SnapshotCapabilityInterfaces)
	}
	if len(bonds) > 0 || len(plan.DeleteBonds) > 0 {
		capabilities = append(capabilities, SnapshotCapabilityBonds)
	}
	// An interface that is re-applied with a new state is not absent. The
	// delete list is the diff's way of saying "replace this object"; the
	// operations remove only its stale addresses and leave it admin-up. Listing
	// it in AbsentInterfaces made the readback require admin-down/no-address
	// and failed every first apply after a VPP restart with
	// "deleted interface ... still has active configuration".
	applied := make(map[string]struct{}, len(plan.Interfaces))
	for _, state := range plan.Interfaces {
		if name := strings.TrimSpace(state.Name); name != "" {
			applied[name] = struct{}{}
		}
	}
	absentInterfaces := make([]string, 0, len(plan.DeleteInterfaces))
	for _, name := range plan.DeleteInterfaces {
		if _, survives := applied[strings.TrimSpace(name)]; survives {
			continue
		}
		absentInterfaces = append(absentInterfaces, name)
	}
	return SnapshotRequest{
		TransactionID:    transactionID,
		Interfaces:       interfaceNames(interfaces),
		AbsentInterfaces: absentInterfaces,
		Bonds:            bondNames(bonds),
		AbsentBonds:      append([]string(nil), plan.DeleteBonds...),
		Capabilities:     capabilities,
		Candidates: SnapshotCandidates{
			Interfaces: append(append([]InterfaceState(nil), interfaces...), plan.DeleteInterfaceState...),
			Bonds:      append(append([]BondState(nil), bonds...), plan.DeleteBondState...),
		},
	}
}

// EnsureInterfaceAddresses replays a known-good address inventory add-only and
// proves every address is live afterwards. It is the rollback safety net: it
// never deletes an address, so a LAN that already forwards is not disturbed,
// and it can repair the Linux LCP handoff by re-attaching the VPP address that
// the LCP mirrors.
func (a Adapter) EnsureInterfaceAddresses(ctx context.Context, transactionID string, states []InterfaceState) error {
	if len(states) == 0 {
		return nil
	}
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" {
		return fmt.Errorf("%w: transaction ID is required", ErrSnapshotIncomplete)
	}
	if a.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open address verification channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	for _, state := range states {
		name := strings.TrimSpace(state.Name)
		if name == "" {
			continue
		}
		commands := make([]string, 0, len(state.Addresses)+2)
		commands = append(commands, fmt.Sprintf("set interface state %s %s", name, nonEmptyAdminState(state.AdminState)))
		for _, address := range state.Addresses {
			if address = strings.TrimSpace(address); address != "" {
				// Optional: replaying an address that is already present must
				// not fail the rollback.
				commands = append(commands, fmt.Sprintf("?set interface ip address %s %s", name, address))
			}
		}
		commands = append(commands, "show interface address "+name)
		reply, err := doOperation(ctx, channel, Operation{Name: "vpp.interface.address.verify", RequestID: transactionID, Resource: name, Payload: state, VPPCtlCommands: commands})
		if err != nil {
			return fmt.Errorf("restore address on %s: %w", name, err)
		}
		results := vppctlCommandResults(reply)
		if len(results) == 0 {
			// A non-vppctl channel (tests, mocks) does not return command
			// results. The add commands above already succeeded; there is
			// nothing more to prove here.
			continue
		}
		observed := parseLiveInterfaceAddresses(resultStdoutLast(results, "show interface address "+name))
		missing := missingAddresses(state.Addresses, observed)
		if len(missing) > 0 {
			return fmt.Errorf("restore address on %s: %s is still absent after replay (LCP pair may be detached)", name, strings.Join(missing, ","))
		}
	}
	return nil
}

func nonEmptyAdminState(state string) string {
	if strings.EqualFold(strings.TrimSpace(state), "down") {
		return "down"
	}
	return "up"
}

func parseLiveInterfaceAddresses(output string) map[string]struct{} {
	addresses := make(map[string]struct{})
	for _, field := range strings.Fields(output) {
		field = strings.TrimRight(field, ",")
		if prefix, err := netip.ParsePrefix(field); err == nil {
			addresses[prefix.String()] = struct{}{}
		}
	}
	return addresses
}

func missingAddresses(wanted []string, observed map[string]struct{}) []string {
	missing := make([]string, 0)
	for _, address := range wanted {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, found := observed[address]; found {
			continue
		}
		missing = append(missing, address)
	}
	return missing
}
