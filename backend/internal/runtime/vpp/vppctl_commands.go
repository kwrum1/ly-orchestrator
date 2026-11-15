package vpp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const defaultPPPoERuntimeStatusDir = "/run/ly-route/pppoe"

const (
	defaultPPPoERuntimeReadyAttempts = 60
	defaultPPPoERuntimeReadyInterval = time.Second
)

type pppoeRuntimeStatus struct {
	Interface string `json:"interface"`
}

func resolveRuntimePPPoECommand(command string) (string, error) {
	fields := strings.Fields(command)
	for index, field := range fields {
		resolved, err := resolveRuntimePPPoEInterface(field)
		if err != nil {
			return "", err
		}
		fields[index] = resolved
	}
	return strings.Join(fields, " "), nil
}

// resolveRuntimePPPoECommandReady resolves pppoe-runtime:<peer> placeholders
// after waiting for the peer helper to publish its status contract and for the
// negotiated session interface to appear in VPP.  A clean install starts the
// helper in the pre-gateway phase, but status publication and VPP client
// creation are asynchronous; routes, DNS service networks and NAT must not be
// applied against a placeholder or a nonexistent interface.
func (channel *vppctlChannel) resolveRuntimePPPoECommandReady(ctx context.Context, command string) (string, error) {
	fields := strings.Fields(command)
	for index, field := range fields {
		resolved, err := channel.waitRuntimePPPoEInterface(ctx, field)
		if err != nil {
			return "", err
		}
		fields[index] = resolved
	}
	return strings.Join(fields, " "), nil
}

func runtimePPPoEReadySettings() (int, time.Duration) {
	attempts := defaultPPPoERuntimeReadyAttempts
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_PPPOE_RUNTIME_READY_ATTEMPTS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attempts = parsed
		}
	}
	interval := defaultPPPoERuntimeReadyInterval
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_PPPOE_RUNTIME_READY_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	return attempts, interval
}

func (channel *vppctlChannel) waitRuntimePPPoEInterface(ctx context.Context, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "pppoe-runtime:") {
		return value, nil
	}
	peerID := strings.TrimPrefix(value, "pppoe-runtime:")
	if peerID == "" || strings.ContainsAny(peerID, "/\\ \t\r\n") {
		return "", fmt.Errorf("invalid PPPoE runtime peer %q", peerID)
	}
	if channel.pppoeResolved != nil {
		if resolved, ok := channel.pppoeResolved[peerID]; ok {
			return resolved, nil
		}
	}
	attempts, interval := runtimePPPoEReadySettings()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Stat before decoding: the helper writes the contract atomically, and
		// a clean install legitimately has no status for the first seconds.
		path := pppoeRuntimeStatusPath(peerID)
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			lastErr = fmt.Errorf("PPPoE runtime peer %q status file %s is not published", peerID, path)
		} else if status, err := readPPPoERuntimeStatus(peerID); err != nil {
			lastErr = err
		} else if present, presentErr := channel.runtimeInterfacePresent(ctx, status.Interface); presentErr != nil {
			lastErr = presentErr
		} else if present {
			if channel.pppoeResolved == nil {
				channel.pppoeResolved = map[string]string{}
			}
			channel.pppoeResolved[peerID] = status.Interface
			return status.Interface, nil
		} else {
			lastErr = fmt.Errorf("PPPoE runtime peer %q interface %q is not present in VPP yet", peerID, status.Interface)
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("PPPoE runtime peer %q status is unavailable", peerID)
	}
	return "", fmt.Errorf("PPPoE runtime peer %q is not ready: %w", peerID, lastErr)
}

func (channel *vppctlChannel) runtimeInterfacePresent(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	commandCtx, cancel, timeout := vppctlTimeout(ctx, false)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, channel.binary, "show", "interface")
	output, err := cmd.Output()
	if err != nil {
		if commandCtx.Err() == context.DeadlineExceeded {
			return false, fmt.Errorf("vppctl show interface timed out after %s while waiting for %s", timeout, name)
		}
		return false, nil
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true, nil
		}
	}
	return false, nil
}

func pppoeRuntimeStatusPath(peerID string) string {
	statusDir := strings.TrimSpace(os.Getenv("LY_ROUTE_PPPOE_RUNTIME_STATUS_DIR"))
	if statusDir == "" {
		statusDir = defaultPPPoERuntimeStatusDir
	}
	return statusDir + "/" + peerID + ".json"
}

func readPPPoERuntimeStatus(peerID string) (pppoeRuntimeStatus, error) {
	data, err := os.ReadFile(pppoeRuntimeStatusPath(peerID))
	if err != nil {
		return pppoeRuntimeStatus{}, fmt.Errorf("read PPPoE runtime peer %q: %w", peerID, err)
	}
	var status pppoeRuntimeStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return pppoeRuntimeStatus{}, fmt.Errorf("decode PPPoE runtime peer %q: %w", peerID, err)
	}
	if !validRuntimePPPoEInterface(status.Interface) {
		return pppoeRuntimeStatus{}, fmt.Errorf("PPPoE runtime peer %q has invalid interface %q", peerID, status.Interface)
	}
	return status, nil
}

func resolveRuntimePPPoEInterface(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "pppoe-runtime:") {
		return value, nil
	}
	peerID := strings.TrimPrefix(value, "pppoe-runtime:")
	if peerID == "" || strings.ContainsAny(peerID, "/\\ \t\r\n") {
		return "", fmt.Errorf("invalid PPPoE runtime peer %q", peerID)
	}
	status, err := readPPPoERuntimeStatus(peerID)
	if err != nil {
		return "", err
	}
	return status.Interface, nil
}

// validRuntimePPPoEInterface accepts the names produced by both the legacy
// native client (pppoe_sessionN) and the Hi-Jiajun plugin (pppoxN, or a
// lyppp-* custom name).  The status file is the authority; the prefix check
// only rejects unsafe/foreign tokens.
func validRuntimePPPoEInterface(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "/\\ \t\r\n\"'") {
		return false
	}
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "pppoe_session") ||
		strings.HasPrefix(lower, "pppox") ||
		strings.HasPrefix(lower, "lyppp-")
}

const (
	defaultVPPCTLCommandTimeout = 30 * time.Second
	defaultVPPCTLBatchTimeout   = 2 * time.Minute
)

func vppctlTimeout(ctx context.Context, batch bool) (context.Context, context.CancelFunc, time.Duration) {
	name := "LY_ROUTE_VPPCTL_COMMAND_TIMEOUT"
	fallback := defaultVPPCTLCommandTimeout
	if batch {
		name = "LY_ROUTE_VPPCTL_BATCH_TIMEOUT"
		fallback = defaultVPPCTLBatchTimeout
	}
	timeout := fallback
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	return commandCtx, cancel, timeout
}

// forbiddenVPPCTLCommands is the control plane's hard stop for VPP CLI verbs
// that are unsafe on this image. It is enforced for every command the control
// executes, including commands persisted by an older plan.
//
// `ping` seeds an in2out NAT session whose inside address is the NAT outside
// address (10.99.0.1). The echo reply then re-enters nat44-ed-out2in with that
// self-referential session and the worker SIGSEGVs
// (`nat44_ed_out2in_node_fn_x86_64_v3 +0x680`, reproduced against VPP 26.06 +
// AF_PACKET). One diagnostic command must never take forwarding down; the
// product uses a kernel-bound `ping -I` probe instead.
var forbiddenVPPCTLCommands = map[string]string{
	"ping": "vppctl ping SIGSEGVs nat44-ed-out2in on this image; use the diagnostics probe (kernel-bound ping -I)",
}

// unsafePacketGeneratorNode reports the pktgen nodes that crash VPP 26.06 on
// this image. `packet-generator new ... node interface-output interface <if>`
// passes the parser but the first enabled packet dereferences a per-buffer
// header the node never received:
//
//	#0 vnet_per_buffer_interface_output_node_fn + 0x45b (libvnet.so.26.06)
//	vpp.service: Main process exited, code=killed, status=6/ABRT
//
// Reproduced on the J1900 appliance (74-vpp-crash.txt): one enable-stream took
// VPP down, the two DPDK ports lost carrier, and recovery needed a reboot.
// Packet injection must enter the graph at a real feature node
// (`ethernet-input` or `ip4-lookup`), which is what the shipped traffic helper
// uses.
func unsafePacketGeneratorNode(lower []string) (string, bool) {
	if len(lower) < 3 || lower[0] != "packet-generator" {
		return "", false
	}
	for index, field := range lower {
		if field != "interface-output" {
			continue
		}
		node := field
		if index > 0 && lower[index-1] == "node" {
			node = "node " + field
		}
		return node, true
	}
	return "", false
}

// forbiddenVPPCTLCommand rejects a queued command before it reaches vppctl.
//
// Besides the node-level bans in forbiddenVPPCTLCommands it enforces the ACL
// lifecycle contract for VPP 26.06:
//
//   - `delete acl-plugin acl` (no index) removes the whole ACL table and is a
//     no-op on this image, but on builds where it does delete, every ABF
//     policy and interface lookup context that still references an ACL is left
//     dangling. The ABF plugin then faults in
//     fib_path_list_contribute_forwarding when the path list is re-contributed
//     (observed backtrace: abf_plugin.so -> vlib_cli_input -> SIGSEGV).
//   - `acl-plugin del acl <id>` is the pre-26.06 spelling and is rejected by
//     this image as unknown input. Accepting it would silently hide a failed
//     delete and leave the plan half-applied.
//
// Only the controlled single-ACL form `delete acl-plugin acl index <id>` is
// allowed, and deletion itself is additionally gated on a reference check in
// skipUnsafeReferencedCommand.
func forbiddenVPPCTLCommand(command string) error {
	raw := strings.TrimSpace(command)
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(raw, "?")))
	if len(fields) == 0 {
		return nil
	}
	lower := make([]string, len(fields))
	for index, field := range fields {
		lower[index] = strings.ToLower(field)
	}
	if reason, forbidden := forbiddenVPPCTLCommands[lower[0]]; forbidden {
		return fmt.Errorf("refusing VPP command %q: %s", raw, reason)
	}
	if node, unsafe := unsafePacketGeneratorNode(lower); unsafe {
		return fmt.Errorf("refusing VPP command %q: packet-generator %s SIGSEGVs vnet_per_buffer_interface_output_node_fn on this VPP build and takes every DPDK port's carrier down; inject at `node ethernet-input interface <if>` (see /usr/lib/ly-route/traffic-smoke.sh)", raw, node)
	}
	if lower[0] == "acl-plugin" && len(lower) >= 3 && lower[1] == "del" && lower[2] == "acl" {
		return fmt.Errorf("refusing VPP command %q: the legacy `acl-plugin del acl` verb is not supported by VPP 26.06; use the controlled form `delete acl-plugin acl index <id>` after detaching its references", raw)
	}
	if lower[0] != "delete" || len(lower) < 3 || lower[1] != "acl-plugin" || lower[2] != "acl" {
		return nil
	}
	if len(lower) == 3 {
		return fmt.Errorf("refusing VPP command %q: whole-table ACL deletion is forbidden; ABF/pre-NAT/service-chain references would dangle and the next ABF path-list contribution can SIGSEGV (fib_path_list_contribute_forwarding). Delete one ACL with `delete acl-plugin acl index <id>` after its references are gone", raw)
	}
	if len(lower) != 5 || lower[3] != "index" {
		return fmt.Errorf("refusing VPP command %q: malformed ACL delete; the only accepted form is `delete acl-plugin acl index <id>`", raw)
	}
	id, err := strconv.Atoi(lower[4])
	if err != nil || id < 0 {
		return fmt.Errorf("refusing VPP command %q: ACL index must be a non-negative integer (VPP assigns the first ACL index 0)", raw)
	}
	return nil
}

// aclDeleteTarget reports the ACL id of a controlled single-ACL delete.
func aclDeleteTarget(command string) (int, bool) {
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?")))
	if len(fields) != 5 {
		return 0, false
	}
	if strings.ToLower(fields[0]) != "delete" || strings.ToLower(fields[1]) != "acl-plugin" || strings.ToLower(fields[2]) != "acl" || strings.ToLower(fields[3]) != "index" {
		return 0, false
	}
	id, err := strconv.Atoi(fields[4])
	if err != nil || id < 0 {
		return 0, false
	}
	return id, true
}

// skipUnsafeReferencedCommand is the runtime half of the ACL/ABF crash guard.
// It answers questions that a static verb check cannot: is this ACL still
// referenced, and does the FIB table an ABF policy is about to point at have a
// usable forwarding path? Probe failures fail open; only a positive, readable
// answer blocks the command.
func (channel *vppctlChannel) skipUnsafeReferencedCommand(ctx context.Context, command string) (bool, string) {
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?")))
	if len(fields) == 0 {
		return false, ""
	}
	lower := make([]string, len(fields))
	for index, field := range fields {
		lower[index] = strings.ToLower(field)
	}
	if id, ok := aclDeleteTarget(command); ok {
		references, err := channel.aclDeleteReferences(ctx, id)
		if err != nil || len(references) == 0 {
			return false, ""
		}
		return true, fmt.Sprintf("skipped: ACL %d is still referenced by %s; delete the attachment/policy first", id, strings.Join(references, ", "))
	}
	if lower[0] == "abf" && len(lower) >= 3 && lower[1] == "policy" && lower[2] == "add" {
		return channel.preflightABFPolicyAdd(ctx, fields)
	}
	if lower[0] == "abf" && len(lower) >= 3 && lower[1] == "policy" && lower[2] == "del" {
		id := fieldAfter(fields, "id")
		if id == "" {
			return false, ""
		}
		output, err := channel.runReadOnlyCommand(ctx, "show", "abf", "policy", id)
		if err != nil {
			return false, ""
		}
		trimmed := strings.TrimSpace(strings.ToLower(output))
		if trimmed == "" || strings.Contains(trimmed, "invalid policy") {
			return true, fmt.Sprintf("skipped: ABF policy %s is already absent", id)
		}
	}
	return false, ""
}

func (channel *vppctlChannel) preflightABFPolicyAdd(ctx context.Context, fields []string) (bool, string) {
	viaIndex := -1
	for index, field := range fields {
		if strings.EqualFold(field, "via") && index+2 < len(fields) {
			viaIndex = index
			break
		}
	}
	if viaIndex < 0 {
		return false, ""
	}
	if !strings.EqualFold(fields[viaIndex+1], "ip4-lookup-in-table") {
		return false, ""
	}
	table := strings.TrimSpace(fields[viaIndex+2])
	if table == "" {
		return false, ""
	}
	exists, err := channel.vppTableExists(ctx, table)
	if err == nil && !exists {
		return true, fmt.Sprintf("refused: ABF policy add points at IPv4 table %s, which does not exist", table)
	}
	output, routeErr := channel.runReadOnlyCommand(ctx, "show", "ip", "fib", "table", table)
	if routeErr == nil && !fibTableHasForwardingDefault(output) {
		return true, fmt.Sprintf("refused: ABF policy add points at IPv4 table %s, which has no forwarding default path; contributing this path list to ABF has crashed VPP (fib_path_list_contribute_forwarding)", table)
	}
	return false, ""
}

func fieldAfter(fields []string, name string) string {
	for index := 0; index+1 < len(fields); index++ {
		if strings.EqualFold(fields[index], name) {
			return strings.TrimSpace(fields[index+1])
		}
	}
	return ""
}

// aclDeleteReferences reports every live owner of an ACL. The inventory is
// intentionally conservative: any readable reference blocks the delete.
func (channel *vppctlChannel) aclDeleteReferences(ctx context.Context, id int) ([]string, error) {
	output, err := channel.runReadOnlyCommand(ctx, "show", "acl-plugin", "acl", "index", strconv.Itoa(id))
	if err != nil {
		return nil, err
	}
	references := make([]string, 0, 3)
	// VPP prints "used in lookup context index: " on every ACL, with an empty
	// value when nothing references it. Match the numeric index, not the label:
	// treating the empty line as a reference made every security-generation
	// cleanup fail with "security generation remains after cleanup".
	if aclHasAttachedLookupContext(output) {
		references = append(references, "interface/service-chain ACL lookup context")
	}
	abf, abfErr := channel.runReadOnlyCommand(ctx, "show", "abf", "policy")
	if abfErr == nil && abfPolicyReferencesACL(abf, id) {
		references = append(references, fmt.Sprintf("ABF policy acl:%d", id))
	}
	prenat, prenatErr := channel.runReadOnlyCommand(ctx, "show", "ly-route", "pre-nat-route")
	if prenatErr == nil && strings.Contains(strings.ToLower(prenat), fmt.Sprintf("acl %d", id)) {
		references = append(references, "pre-NAT classifier")
	}
	return references, nil
}

func aclHasAttachedLookupContext(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		for _, label := range []string{"used in lookup context index:", "applied inbound on sw_if_index:", "applied outbound on sw_if_index:"} {
			index := strings.Index(lower, label)
			if index < 0 {
				continue
			}
			rest := strings.TrimSpace(strings.Trim(line[index+len(label):], " []"))
			for _, field := range strings.Fields(rest) {
				if id, err := strconv.Atoi(field); err == nil && id >= 0 {
					return true
				}
			}
		}
	}
	return false
}

func abfPolicyReferencesACL(output string, id int) bool {
	needle := fmt.Sprintf("acl:%d", id)
	for _, field := range strings.FieldsFunc(output, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '(', ')', ',', ';', '[', ']':
			return true
		}
		return false
	}) {
		if strings.EqualFold(field, needle) {
			return true
		}
	}
	return false
}

func (channel *vppctlChannel) runReadOnlyCommand(ctx context.Context, args ...string) (string, error) {
	commandCtx, cancel, _ := vppctlTimeout(ctx, false)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, channel.binary, args...).Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (channel *vppctlChannel) doCommands(ctx context.Context, operation Operation) (Reply, error) {
	// A policy apply is dominated by process launches, not VPP work: one
	// vppctl process per rule costs ~35ms on this class of appliance. Build the
	// whole candidate in one `vppctl exec` file instead, then run the one or two
	// readback commands normally. The readback verification compares the policy
	// row count with the rule commands, so a silently dropped rule still fails.
	if reply, handled, err := channel.doTransparentOrchestratorBatch(ctx, operation); handled {
		return reply, err
	}
	results := make([]VPPCTLCommandResult, 0, len(operation.VPPCtlCommands))
	for index := 0; index < len(operation.VPPCtlCommands); index++ {
		command := strings.TrimSpace(operation.VPPCtlCommands[index])
		ignoreFailure := strings.HasPrefix(command, "?")
		command = strings.TrimSpace(strings.TrimPrefix(command, "?"))
		if err := forbiddenVPPCTLCommand(command); err != nil {
			return Reply{Operation: operation.Name, Retval: 1, Payload: VPPCTLReplyPayload{CommandResults: results}}, err
		}
		if command == vppRouteBatchBegin {
			batch, end, err := collectVPPRouteBatch(operation.VPPCtlCommands, index)
			if err != nil {
				return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}, err
			}
			batchResults, batchErr := channel.doVPPRouteBatch(ctx, batch)
			results = append(results, batchResults...)
			if batchErr != nil {
				return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}, batchErr
			}
			index = end
			continue
		}
		logicalCommand := command
		if lanInterface := strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_INTERFACE")); lanInterface != "" {
			command = strings.ReplaceAll(command, "$LY_ROUTE_LAN_INTERFACE", lanInterface)
		}
		// VPP 26.06 aborts on unsafe FIB table lifecycle operations. Route and
		// table commands are made idempotent here so cleanup/replay cannot
		// delete a non-empty table or touch a table that no longer exists.
		if skip, reason := channel.skipDeletedTableCommand(ctx, command); skip {
			results = append(results, VPPCTLCommandResult{Command: logicalCommand, Stderr: reason, Retval: 0})
			continue
		}
		resolvedCommand, resolveErr := channel.resolveRuntimePPPoECommandReady(ctx, command)
		if resolveErr != nil {
			if ignoreFailure {
				results = append(results, VPPCTLCommandResult{Command: logicalCommand, Stderr: resolveErr.Error(), Retval: 1})
				continue
			}
			return Reply{Operation: operation.Name, Retval: 1, Payload: VPPCTLReplyPayload{CommandResults: results}}, resolveErr
		}
		command = resolvedCommand
		if command == "" {
			continue
		}
		if skip, reason := channel.skipUnsafeReferencedCommand(ctx, command); skip {
			results = append(results, VPPCTLCommandResult{Command: logicalCommand, Stderr: reason, Retval: 0})
			continue
		}
		cacheable := cacheableVPPCTLCommand(command)
		if cacheable {
			if stdout, stderr, ok := channel.cache.get(command); ok {
				results = append(results, VPPCTLCommandResult{Command: logicalCommand, Stdout: stdout, Stderr: stderr, Retval: 0})
				continue
			}
		}
		args := strings.Fields(command)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		commandCtx, cancel, timeout := vppctlTimeout(ctx, false)
		cmd := exec.CommandContext(commandCtx, channel.binary, args...)
		cmd.WaitDelay = timeout / 2
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		timedOut := commandCtx.Err() == context.DeadlineExceeded
		cancel()
		retval := int32(0)
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				retval = int32(exitErr.ExitCode())
			} else {
				retval = -1
			}
			if timedOut {
				err = fmt.Errorf("vppctl command %q timed out after %s", command, timeout)
				retval = -1
			}
		}
		results = append(results, VPPCTLCommandResult{Command: logicalCommand, Stdout: stdout.String(), Stderr: stderr.String(), Retval: retval})
		if err == nil && cacheable {
			channel.cache.put(command, stdout.String(), stderr.String())
		} else if !cacheable {
			// A mutation invalidates every prior snapshot: the next readback
			// must not observe a pre-change show result.
			channel.cache.invalidate()
		}
		if err != nil {
			// NAT44 "already present"/"already absent" replies describe the
			// desired end state. Treat them as success so a re-apply cannot
			// retry the same del/add and trigger the VPP NAT44 crash.
			if nat44IdempotentFailure(command, stderr.String()) {
				results[len(results)-1].Retval = 0
				continue
			}
			if ignoreFailure {
				continue
			}
			reply := Reply{Operation: operation.Name, Retval: retval, Payload: VPPCTLReplyPayload{CommandResults: results}}
			failure := fmt.Errorf("vppctl %s command %q failed with retval %d: %w: %s", operation.Name, command, retval, err, strings.TrimSpace(stderr.String()))
			if strings.HasSuffix(operation.Name, ".snapshot") {
				return reply, fmt.Errorf("%w: %v", ErrSnapshotIncomplete, failure)
			}
			return reply, failure
		}
	}
	payload := VPPCTLReplyPayload{CommandResults: results}
	readback, err := decodeVPPCTLReadback(operation, results)
	if err != nil {
		return Reply{Operation: operation.Name, Payload: payload}, err
	}
	payload.Readback = readback
	return Reply{Operation: operation.Name, Payload: payload}, nil
}

// doTransparentOrchestratorBatch runs the candidate-building commands of one
// orchestrator apply through a single `vppctl exec`. It only takes over when the
// operation is the plain apply (not an observation/disable), every command is a
// candidate/commit/SPAN mutation or a show, and there is enough work to pay for
// the file round trip. Anything unusual falls back to the per-command path.
func (channel *vppctlChannel) doTransparentOrchestratorBatch(ctx context.Context, operation Operation) (Reply, bool, error) {
	if operation.Name != "vpp.transparent-orchestrator" || len(operation.VPPCtlCommands) < 8 {
		return Reply{}, false, nil
	}
	results := make([]VPPCTLCommandResult, len(operation.VPPCtlCommands))
	mutating := make([]string, 0, len(operation.VPPCtlCommands))
	mutatingSlots := make([]int, 0, len(operation.VPPCtlCommands))
	shows := make([]int, 0, 2)
	for index, raw := range operation.VPPCtlCommands {
		command := strings.TrimSpace(raw)
		results[index].Command = command
		// The batch file cannot express the `?` ignore-failure marker; mirror
		// cleanup uses it, so those applies keep the per-command path.
		if command == "" || strings.HasPrefix(command, "?") {
			return Reply{}, false, nil
		}
		if err := forbiddenVPPCTLCommand(command); err != nil {
			return Reply{}, false, nil
		}
		switch {
		case strings.HasPrefix(command, "show "):
			shows = append(shows, index)
		case strings.HasPrefix(command, "set ly-route orchestrator "),
			strings.HasPrefix(command, "set interface span "),
			strings.HasPrefix(command, "interface span "):
			mutating = append(mutating, command)
			mutatingSlots = append(mutatingSlots, index)
		default:
			return Reply{}, false, nil
		}
	}
	if len(mutating) < 8 {
		return Reply{}, false, nil
	}
	for begin := 0; begin < len(mutating); begin += vppRouteBatchChunkSize {
		end := begin + vppRouteBatchChunkSize
		if end > len(mutating) {
			end = len(mutating)
		}
		label := fmt.Sprintf("vpp orchestrator batch %d-%d/%d", begin+1, end, len(mutating))
		batchResult, err := channel.executeVPPBatchFile(ctx, mutating[begin:end], label)
		if err != nil {
			return Reply{Operation: operation.Name, Retval: 1, Payload: VPPCTLReplyPayload{CommandResults: results}}, true, err
		}
		// Attribute the combined output to the first command of the chunk for
		// diagnostics; the caller only reads the trailing show results.
		results[mutatingSlots[begin]].Stdout = batchResult.Stdout
		results[mutatingSlots[begin]].Stderr = batchResult.Stderr
	}
	for _, index := range shows {
		reply, err := channel.doCommands(ctx, Operation{
			Name:           operation.Name,
			RequestID:      operation.RequestID,
			Resource:       operation.Resource,
			VPPCtlCommands: []string{results[index].Command},
		})
		if err != nil {
			return reply, true, err
		}
		payload, ok := reply.Payload.(VPPCTLReplyPayload)
		if !ok || len(payload.CommandResults) != 1 {
			return reply, true, fmt.Errorf("%w: %s readback is incomplete", ErrSnapshotIncomplete, operation.Name)
		}
		results[index] = payload.CommandResults[0]
	}
	return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}, true, nil
}

func collectVPPRouteBatch(commands []string, begin int) ([]string, int, error) {
	batch := make([]string, 0, 128)
	for index := begin + 1; index < len(commands); index++ {
		command := strings.TrimSpace(commands[index])
		command = strings.TrimSpace(strings.TrimPrefix(command, "?"))
		if command == vppRouteBatchEnd {
			return batch, index, nil
		}
		if command == vppRouteBatchBegin {
			return nil, -1, fmt.Errorf("nested VPP route command batch")
		}
		if command != "" {
			batch = append(batch, command)
		}
	}
	return nil, -1, fmt.Errorf("unterminated VPP route command batch")
}

// Keep large provider datasets in bounded VPP CLI files without paying one
// process launch for every few prefixes. A 512-command batch stays well below
// practical exec-file limits while making full GeoIP replay fast enough for a
// control-plane transaction.
const vppRouteBatchChunkSize = 512

type tableCommandKind int

const (
	tableCommandNone tableCommandKind = iota
	tableCommandRouteDelete
	tableCommandAdd
	tableCommandDelete
)

// skipDeletedTableCommand keeps VPP table lifecycle commands idempotent.
//
// VPP 26.06 has two fatal failure modes around private FIB tables:
//   - `ip table del` aborts the process when the table still owns a route;
//   - deleting and immediately recreating an otherwise empty table can leave
//     stale attached-export state, so the next `ip route add` crashes in
//     fib_attached_export_import.
//
// Tables are therefore retained for the lifetime of the VPP instance. Their
// IDs are stable hashes of the owning resource, so this is bounded and a
// re-apply reuses the same table instead of destroying it. Routes are still
// removed by the normal cleanup commands, and route/table commands aimed at a
// table that does not exist are skipped rather than asking VPP to operate on
// an invalid FIB.
func (channel *vppctlChannel) skipDeletedTableCommand(ctx context.Context, command string) (bool, string) {
	tableID, kind, ok := tableCommandIdentity(command)
	if !ok {
		return false, ""
	}
	exists, err := channel.vppTableExists(ctx, tableID)
	if kind == tableCommandDelete {
		// Destructive table deletion is never sent to VPP. If the inventory
		// query works, remove every owned route first so the retained table is
		// empty; if it does not, leaving the table untouched is still safer than
		// a blind delete.
		if err == nil {
			routes, routeErr := channel.fibTableRoutes(ctx, tableID)
			if routeErr == nil {
				for _, prefix := range routes {
					commandCtx, cancel, _ := vppctlTimeout(ctx, false)
					_ = exec.CommandContext(commandCtx, channel.binary, "ip", "route", "del", "table", tableID, prefix).Run()
					cancel()
				}
			}
		}
		return true, fmt.Sprintf("retained: destructive deletion of IPv4 table %s is disabled", tableID)
	}
	if err != nil {
		// A fake or older vppctl may not implement `show ip table`; preserve the
		// legacy command path rather than silently dropping lifecycle commands.
		return false, ""
	}
	switch kind {
	case tableCommandAdd:
		if exists {
			return true, fmt.Sprintf("skipped: IPv4 table %s already exists", tableID)
		}
	case tableCommandRouteDelete:
		if !exists {
			return true, fmt.Sprintf("skipped: IPv4 table %s does not exist", tableID)
		}
	}
	return false, ""
}

func (channel *vppctlChannel) fibTableRoutes(ctx context.Context, tableID string) ([]string, error) {
	commandCtx, cancel, _ := vppctlTimeout(ctx, false)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, channel.binary, "show", "ip", "fib", "table", tableID).Output()
	if err != nil {
		return nil, err
	}
	return fibTablePrefixes(string(output)), nil
}

func fibTablePrefixes(output string) []string {
	prefixes := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		prefix := fields[0]
		if strings.HasPrefix(prefix, "ipv4") || strings.HasPrefix(prefix, "ipv6") || !strings.Contains(prefix, "/") {
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// fibTableHasForwardingDefault reports whether the table still has a usable
// default route. VPP keeps a special drop entry for a removed default path, so
// an empty prefix inventory is not sufficient to prove that a route policy is
// gone. The default entry is the only path that can forward unmatched traffic
// through the private FIB.
func fibTableHasForwardingDefault(output string) bool {
	lines := strings.Split(output, "\n")
	inDefault := false
	defaultBlock := make([]string, 0, 16)
	flush := func() bool {
		if !inDefault {
			return false
		}
		block := strings.Join(defaultBlock, "\n")
		if strings.Contains(block, "ipv4 via ") || strings.Contains(block, "ip4-lookup-in-table ") || strings.Contains(block, "lookup in ipv4-VRF:") {
			return true
		}
		return false
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.Contains(fields[0], "/") && !strings.HasPrefix(fields[0], "ipv4") && !strings.HasPrefix(fields[0], "ipv6") {
			if flush() {
				return true
			}
			inDefault = fields[0] == "0.0.0.0/0"
			defaultBlock = defaultBlock[:0]
			if inDefault {
				defaultBlock = append(defaultBlock, line)
			}
			continue
		}
		if inDefault {
			defaultBlock = append(defaultBlock, line)
		}
	}
	return flush()
}

func tableCommandIdentity(command string) (string, tableCommandKind, bool) {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) >= 4 && fields[0] == "ip" && fields[1] == "table" && fields[2] == "del" {
		return fields[3], tableCommandDelete, true
	}
	if len(fields) >= 4 && fields[0] == "ip" && fields[1] == "table" && fields[2] == "add" {
		return fields[3], tableCommandAdd, true
	}
	if len(fields) >= 5 && fields[0] == "ip" && fields[1] == "route" && fields[2] == "del" && fields[3] == "table" {
		return fields[4], tableCommandRouteDelete, true
	}
	return "", tableCommandNone, false
}

func (channel *vppctlChannel) vppTableExists(ctx context.Context, tableID string) (bool, error) {
	commandCtx, cancel, _ := vppctlTimeout(ctx, false)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, channel.binary, "show", "ip", "table").Output()
	if err != nil {
		return false, err
	}
	token := "table_id:" + tableID
	for _, line := range strings.Split(string(output), "\n") {
		for _, field := range strings.Fields(line) {
			if field == token {
				return true, nil
			}
		}
	}
	return false, nil
}

func (channel *vppctlChannel) doVPPRouteBatch(ctx context.Context, commands []string) ([]VPPCTLCommandResult, error) {
	// A batch is executed by `vppctl exec <file>`, which bypasses doCommands;
	// validate it here so the ban cannot be laundered through a route batch.
	for _, command := range commands {
		if err := forbiddenVPPCTLCommand(command); err != nil {
			return []VPPCTLCommandResult{{Command: "vpp route command batch"}}, err
		}
	}
	results := make([]VPPCTLCommandResult, 0, (len(commands)+vppRouteBatchChunkSize-1)/vppRouteBatchChunkSize)
	for begin := 0; begin < len(commands); begin += vppRouteBatchChunkSize {
		end := begin + vppRouteBatchChunkSize
		if end > len(commands) {
			end = len(commands)
		}
		result, err := channel.doVPPRouteBatchChunk(ctx, commands[begin:end], begin, end, len(commands))
		results = append(results, result)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func (channel *vppctlChannel) doVPPRouteBatchChunk(ctx context.Context, commands []string, begin, end, total int) (VPPCTLCommandResult, error) {
	label := fmt.Sprintf("vpp route command batch %d-%d/%d", begin+1, end, total)
	return channel.executeVPPBatchFile(ctx, commands, label)
}

// executeVPPBatchFile writes the commands to a CLI file and runs one
// `vppctl exec`, which executes them in a single process.
func (channel *vppctlChannel) executeVPPBatchFile(ctx context.Context, commands []string, label string) (VPPCTLCommandResult, error) {
	file, err := os.CreateTemp("", "ly-route-vpp-batch-*.conf")
	if err != nil {
		return VPPCTLCommandResult{Command: label}, fmt.Errorf("create %s: %w", label, err)
	}
	path := file.Name()
	defer os.Remove(path)
	for _, command := range commands {
		if lanInterface := strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_INTERFACE")); lanInterface != "" {
			command = strings.ReplaceAll(command, "$LY_ROUTE_LAN_INTERFACE", lanInterface)
		}
		resolvedCommand, err := channel.resolveRuntimePPPoECommandReady(ctx, command)
		if err != nil {
			return VPPCTLCommandResult{Command: label}, err
		}
		command = resolvedCommand
		if _, err := file.WriteString(strings.TrimSpace(command) + "\n"); err != nil {
			_ = file.Close()
			return VPPCTLCommandResult{Command: label}, fmt.Errorf("write %s: %w", label, err)
		}
	}
	if err := file.Close(); err != nil {
		return VPPCTLCommandResult{Command: label}, fmt.Errorf("close %s: %w", label, err)
	}
	var stdout, stderr bytes.Buffer
	commandCtx, cancel, timeout := vppctlTimeout(ctx, true)
	cmd := exec.CommandContext(commandCtx, channel.binary, "exec", path)
	cmd.WaitDelay = timeout / 2
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	timedOut := commandCtx.Err() == context.DeadlineExceeded
	cancel()
	result := VPPCTLCommandResult{Command: label, Stdout: stdout.String(), Stderr: stderr.String()}
	channel.cache.invalidate()
	if runErr != nil {
		if timedOut {
			return result, fmt.Errorf("vppctl exec %s timed out after %s", label, timeout)
		}
		return result, fmt.Errorf("vppctl exec %s failed: %w: %s", label, runErr, strings.TrimSpace(stderr.String()))
	}
	combined := strings.ToLower(stdout.String() + "\n" + stderr.String())
	if strings.Contains(combined, "cli line error") || strings.Contains(combined, "unknown input") || strings.Contains(combined, "parse error") {
		return result, fmt.Errorf("vppctl exec %s reported an error: %s", label, strings.TrimSpace(stdout.String()+" "+stderr.String()))
	}
	return result, nil
}
