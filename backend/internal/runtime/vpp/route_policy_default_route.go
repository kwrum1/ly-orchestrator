package vpp

import (
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// The route policy private FIB (the "tenant table") only forwards when its
// default prefix carries a resolved path. VPP happily keeps the table itself
// (a header plus the automatic 0.0.0.0/32 and multicast drop entries) after a
// failed or skipped `ip route add`, and the native pre-NAT plugin keeps a
// classifier rule that points at that now-empty table. A readback that only
// proves "the table exists" or "the rule exists" therefore reports a healthy
// policy while every packet that matches the rule falls into the table miss
// and is dropped. These helpers make the forwarding default an explicit,
// verified part of the contract.

// routePolicyTenantDefaultVia returns the default next hop that the generated
// operation must install in its private FIB. ok is false when the policy
// deliberately installs no default route (for example a deny policy).
func routePolicyTenantDefaultVia(operation Operation) (string, bool) {
	for _, raw := range routePolicyNativeTableCommands(operation) {
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		if !strings.HasPrefix(command, "ip route add ") {
			continue
		}
		fields := strings.Fields(command)
		// ip route add table <id> 0.0.0.0/0 via <next hop>
		if len(fields) < 6 {
			continue
		}
		defaultPrefix := false
		for _, field := range fields[:6] {
			if field == "0.0.0.0/0" {
				defaultPrefix = true
				break
			}
		}
		if !defaultPrefix {
			continue
		}
		index := strings.Index(command, " via ")
		if index < 0 {
			continue
		}
		via := routePolicyFIBVia(strings.TrimSpace(command[index+5:]))
		if via == "" {
			continue
		}
		return via, true
	}
	return "", false
}

// policyTableFIBPaths reads the last `show ip fib table` result for a tenant
// table. Lifecycles append a fresh readback after a repair, so the last row is
// the authoritative one.
func policyTableFIBPaths(results []VPPCTLCommandResult, tableID int) ([]fibPath, error) {
	command := fmt.Sprintf("show ip fib table %d", tableID)
	output := ""
	seen := false
	for _, result := range results {
		if result.Command != command {
			continue
		}
		seen = true
		if result.Retval != 0 {
			return nil, snapshotDecodeError("command %q returned retval %d", command, result.Retval)
		}
		output = result.Stdout
	}
	if !seen {
		return nil, snapshotDecodeError("route policy tenant table %d readback is missing", tableID)
	}
	if strings.TrimSpace(output) == "" {
		return nil, snapshotDecodeError("route policy tenant table %d readback is empty", tableID)
	}
	return parseFIBOutput(output, tableID, false)
}

// verifyRoutePolicyTenantDefault proves that the tenant table forwards its
// default prefix through the expected next hop. A table that only contains the
// header (or a drop path) fails the apply instead of committing an unreachable
// policy.
func verifyRoutePolicyTenantDefault(results []VPPCTLCommandResult, tableID int, expected string) error {
	paths, err := policyTableFIBPaths(results, tableID)
	if err != nil {
		return err
	}
	if fibPathsSatisfyDefault(paths, expected) {
		return nil
	}
	return snapshotDecodeError("route policy tenant table %d has no forwarding default via %q: observed %v", tableID, expected, paths)
}

// fibPathsSatisfyDefault compares a decoded FIB default path with the next hop
// the plan asked for. It accepts the two VPP representations of a point-to-
// point/interface adjacency (`0.0.0.0 <iface>` for an interface-only path) and
// compares IP/interface tokens exactly so `10.1.18.1` never matches a route via
// `10.1.18.10`.
func fibPathsSatisfyDefault(paths []fibPath, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return false
	}
	expectedFields := strings.Fields(expected)
	for _, path := range paths {
		actualFields := strings.Fields(strings.TrimSpace(path.via))
		if fibViaTokensMatch(actualFields, expectedFields) {
			return true
		}
		if len(actualFields) >= 2 && actualFields[0] == "0.0.0.0" {
			// VPP resolves a point-to-point next hop to a zero-address
			// adjacency (`0.0.0.0 <iface>`). The interface token is then the
			// only identity that can be compared.
			iface := strings.TrimRight(actualFields[1], ":,")
			switch len(expectedFields) {
			case 1:
				if iface == expectedFields[0] {
					return true
				}
			case 2:
				if iface == expectedFields[1] {
					return true
				}
			}
		}
	}
	return false
}

func fibViaTokensMatch(actual, expected []string) bool {
	if len(expected) == 0 || len(actual) < len(expected) {
		return false
	}
	for index, want := range expected {
		if strings.TrimRight(actual[index], ":,") != want {
			return false
		}
	}
	return true
}

// routePolicyReadbackDefaultVia mirrors the command compiler so the snapshot
// decoder can assert the same default path that the apply promised. It returns
// an empty string when the policy has no resolvable target (for example a
// legacy native classifier restored without a WAN binding), in which case the
// decoder keeps its previous, classifier-only validation.
func routePolicyReadbackDefaultVia(policy trafficpolicy.RoutePolicy, groups map[string]trafficpolicy.WANGroup, option routePolicyCommandOptions) string {
	if option.optimizedIPv4 && strings.TrimSpace(option.defaultVia) != "" {
		return routePolicyFIBVia(option.defaultVia)
	}
	target := strings.TrimSpace(routePolicyTarget(policy, groups))
	if target == "" || strings.EqualFold(target, "local") {
		return ""
	}
	return routePolicyFIBVia(target)
}
