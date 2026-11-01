package dataplane

import (
	"context"
	"errors"
	"testing"
)

type scriptedRunner struct {
	calls [][]string
	// results is consumed in order; each entry is (output, err).
	results []struct {
		output string
		err    error
	}
	index int
}

func (runner *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string{name}, args...))
	if runner.index >= len(runner.results) {
		return nil, nil
	}
	result := runner.results[runner.index]
	runner.index++
	return []byte(result.output), result.err
}

// A stop job canceled by systemd against a pending auto-restart must not fail
// the dataplane transaction when VPP is actually down.
func TestStopVPPAcceptsCanceledJobWhenVPPIsInactive(t *testing.T) {
	runner := &scriptedRunner{results: []struct {
		output string
		err    error
	}{
		{"", errors.New("exit status 1")},
		{"inactive\n", errors.New("exit status 3")},
	}}
	host := &LinuxHost{Runner: runner}
	if err := host.StopVPP(context.Background()); err != nil {
		t.Fatalf("StopVPP = %v, want nil when VPP is inactive", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %v, want stop + is-active", runner.calls)
	}
}

// When VPP is still active the stop is retried once.
func TestStopVPPRetriesOnceWhileVPPIsActive(t *testing.T) {
	runner := &scriptedRunner{results: []struct {
		output string
		err    error
	}{
		{"", errors.New("exit status 1")},
		{"active\n", nil},
		{"", nil},
	}}
	host := &LinuxHost{Runner: runner}
	if err := host.StopVPP(context.Background()); err != nil {
		t.Fatalf("StopVPP = %v, want nil after a successful retry", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %v, want stop + is-active + stop", runner.calls)
	}
}

// A genuine failure is still reported.
func TestStopVPPReportsFailureWhenStillActiveAfterRetry(t *testing.T) {
	runner := &scriptedRunner{results: []struct {
		output string
		err    error
	}{
		{"", errors.New("exit status 1")},
		{"active\n", nil},
		{"", errors.New("exit status 1")},
	}}
	host := &LinuxHost{Runner: runner}
	if err := host.StopVPP(context.Background()); err == nil {
		t.Fatal("StopVPP = nil, want the original failure")
	}
}
