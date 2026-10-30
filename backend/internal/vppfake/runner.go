package vppfake

import (
	"fmt"
	"os"
	"strings"
)

// Runner executes CLI lines against an on-disk state file, mirroring one
// vppctl process invocation. It is shared by the fake binary and by tests that
// want the state machine without a process boundary.
type Runner struct {
	StatePath string
	TracePath string
	FailToken string
}

// Run executes one command line.
func (runner Runner) Run(command string) (string, string, int) {
	command = strings.TrimSpace(command)
	if runner.TracePath != "" {
		if handle, err := os.OpenFile(runner.TracePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = handle.WriteString(command + "\n")
			_ = handle.Close()
		}
	}
	var stdout, stderr string
	var code int
	err := withLock(runner.StatePath, func(state *State) error {
		stdout, stderr, code = Execute(state, command)
		return nil
	})
	if err != nil {
		return "", err.Error(), 1
	}
	if runner.FailToken != "" && strings.Contains(command, runner.FailToken) {
		return stdout, fmt.Sprintf("injected vppctl failure for %q\n", command), 17
	}
	if debugPath := os.Getenv("FAKE_VPPCTL_DEBUG_LOG"); debugPath != "" {
		if handle, err := os.OpenFile(debugPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = handle.WriteString("### " + command + "\n" + stdout + "\n")
			_ = handle.Close()
		}
	}
	return stdout, stderr, code
}

// RunArgs joins vppctl-style argv into one CLI line and executes it.
func (runner Runner) RunArgs(args []string) (string, string, int) {
	if len(args) > 0 && args[0] == "exec" {
		if len(args) < 2 {
			return "", "vppctl exec requires a file\n", 1
		}
		return runner.runExecFile(args[1])
	}
	return runner.Run(strings.Join(args, " "))
}

func (runner Runner) runExecFile(path string) (string, string, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err.Error(), 1
	}
	var stdout strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out, errOut, code := runner.Run(line)
		stdout.WriteString(out)
		if code != 0 {
			return stdout.String(), fmt.Sprintf("cli line error: %s%s\n", line, errOut), code
		}
	}
	return stdout.String(), "", 0
}
