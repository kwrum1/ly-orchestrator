// Command fakevppctl is a deterministic, stateful stand-in for vppctl used by
// the production gateway proof harness. See the vppfake package for the model.
package main

import (
	"fmt"
	"os"

	"ly-route/backend/internal/vppfake"
)

func main() {
	stateDirectory := os.Getenv("FAKE_VPPCTL_STATE_DIR")
	if stateDirectory == "" {
		fmt.Fprintln(os.Stderr, "FAKE_VPPCTL_STATE_DIR is required")
		os.Exit(2)
	}
	runner := vppfake.Runner{
		StatePath: stateDirectory + "/state.json",
		TracePath: os.Getenv("FAKE_VPPCTL_TRACE"),
		FailToken: os.Getenv("FAKE_VPPCTL_FAIL"),
	}
	stdout, stderr, code := runner.RunArgs(os.Args[1:])
	fmt.Print(stdout)
	fmt.Fprint(os.Stderr, stderr)
	os.Exit(code)
}
