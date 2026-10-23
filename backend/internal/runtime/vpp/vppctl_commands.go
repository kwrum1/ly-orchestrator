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

func (channel *vppctlChannel) doCommands(ctx context.Context, operation Operation) (Reply, error) {
	results := make([]VPPCTLCommandResult, 0, len(operation.VPPCtlCommands))
	for index := 0; index < len(operation.VPPCtlCommands); index++ {
		command := strings.TrimSpace(operation.VPPCtlCommands[index])
		ignoreFailure := strings.HasPrefix(command, "?")
		command = strings.TrimSpace(strings.TrimPrefix(command, "?"))
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
