//go:build !linux

package service

import "os/exec"

func setCommandProcessGroup(*exec.Cmd) {}

func killCommandProcessGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
