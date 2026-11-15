package httpapi

// The upgrade installer runs as a transient systemd unit so it survives the
// control-plane restart it performs. That means the unit, not this process, is
// the authority on whether the payload finished; recovery reads it before
// turning a stuck task into a failure.

import (
	"os/exec"
	"strings"
)

// upgradeUnitResult reports the unit's Result property (success, exit-code,
// signal, timeout, ...) and a short human reason.
func upgradeUnitResult(unit string) (string, string, bool) {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return "", "", false
	}
	output, err := exec.Command("systemctl", "show", unit,
		"-p", "LoadState", "-p", "Result", "-p", "ExecMainStatus", "-p", "ActiveState").Output()
	if err != nil {
		return "", "", false
	}
	load, result, status, active := "", "", "", ""
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "LoadState":
			load = value
		case "Result":
			result = value
		case "ExecMainStatus":
			status = value
		case "ActiveState":
			active = value
		}
	}
	// systemctl answers with default properties for a unit that never existed
	// (Result=success, ActiveState=inactive). Only a unit systemd still knows
	// about is evidence; a collected unit leaves nothing to judge.
	if result == "" || load == "not-found" || load == "" {
		return "", "", false
	}
	// A unit that never started reports Result=success with an empty status; it
	// carries no evidence, so let the caller keep the generic message.
	if result == "success" && active == "inactive" && status == "" {
		return "", "", false
	}
	reason := "installer exited with status " + status
	if result != "success" {
		reason = "installer unit " + result + " (status " + status + ")"
	}
	return result, reason, true
}
