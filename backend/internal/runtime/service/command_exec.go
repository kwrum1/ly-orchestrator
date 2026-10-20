package service

import (
	"bytes"
	"context"
	"errors"
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
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(commandCtx, name, args...)
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
