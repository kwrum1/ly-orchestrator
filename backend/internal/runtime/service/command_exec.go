package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrCommandTimeout reports that a child process did not exit before its
// hard deadline.  Callers must treat this as "state unknown", not as proof
// that the unit is inactive.
var ErrCommandTimeout = errors.New("command timed out")

const commandWaitDelay = 250 * time.Millisecond

// runCommandBounded runs one child with a hard wall-clock bound.  The context
// alone is not sufficient: a systemctl process talking to a wedged PID1 can
// enter an uninterruptible wait, so Cmd.Wait never returns.  We wait in a
// goroutine and abandon the wait after killing the process group; the HTTP
// request must never inherit that stuck wait.
func runCommandBounded(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return runCommandBoundedEnv(ctx, timeout, nil, name, args...)
}

// runCommandBoundedEnv is runCommandBounded with explicit child environment.
// The environment is appended to os.Environ so a helper still sees PATH and
// the appliance configuration, while the caller identity wins over stale
// values in an environment file.
func runCommandBoundedEnv(ctx context.Context, timeout time.Duration, environment []string, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(commandCtx, name, args...)
	if len(environment) > 0 {
		command.Env = mergeEnvironment(os.Environ(), environment)
	}
	setCommandProcessGroup(command)
	command.WaitDelay = commandWaitDelay
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return "", err
	}

	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()

	select {
	case err := <-done:
		return strings.TrimSpace(output.String()), err
	case <-commandCtx.Done():
		killCommandProcessGroup(command)
		select {
		case <-done:
		case <-time.After(commandWaitDelay):
		}
		// Do not read output here: the child may still be writing to the
		// buffer while it is stuck in the kernel.  The caller only needs a
		// bounded failure result.
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return "", ErrCommandTimeout
		}
		return "", commandCtx.Err()
	}
}

// mergeEnvironment lets the caller identity win over stale values inherited
// from a systemd EnvironmentFile. Duplicate names are removed from the base
// because getenv returns the first match.
func mergeEnvironment(base, override []string) []string {
	if len(override) == 0 {
		return base
	}
	overridden := make(map[string]bool, len(override))
	for _, entry := range override {
		if index := strings.IndexByte(entry, '='); index > 0 {
			overridden[entry[:index]] = true
		}
	}
	result := make([]string, 0, len(base)+len(override))
	for _, entry := range base {
		if index := strings.IndexByte(entry, '='); index > 0 && overridden[entry[:index]] {
			continue
		}
		result = append(result, entry)
	}
	return append(result, override...)
}
