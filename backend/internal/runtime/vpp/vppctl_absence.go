package vpp

import "strings"

func verifyVPPCTLAbsence(results []VPPCTLCommandResult, commands []string, kind, id string) error {
	for _, command := range commands {
		// Table objects are intentionally retained after their routes are
		// removed, so `show ip table` cannot prove resource absence. The FIB
		// readback below is authoritative.
		if strings.HasPrefix(strings.TrimSpace(command), "show ip table ") {
			continue
		}
		output, err := commandOutputAllowEmpty(results, command)
		if err != nil {
			return err
		}
		if strings.HasPrefix(strings.TrimSpace(command), "show ip fib table ") {
			if !fibTableHasForwardingDefault(output) {
				continue
			}
		}
		value := strings.ToLower(strings.TrimSpace(output))
		if value != "" && !strings.Contains(value, "not found") && !strings.Contains(value, "no such") && !strings.Contains(value, "not configured") && value != "empty" {
			return snapshotDecodeError("deleted %s %q remains or returned unknown absence grammar", kind, id)
		}
	}
	return nil
}
