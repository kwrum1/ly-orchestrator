package vpp

import (
	"fmt"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/flow"
)

func shapeObjectInterface(object flow.VPPObject) string {
	if len(object.Attachments) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(object.Attachments[0]), "input:"), "output:")
}

func smartQoSInterfaceLine(output, interfaceName string) string {
	prefix := "interface " + interfaceName + " enabled "
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func smartQoSInterfacePresent(output, interfaceName string) bool {
	return smartQoSInterfaceLine(output, interfaceName) != ""
}

func verifyShapeResult(results []VPPCTLCommandResult, object flow.VPPObject) error {
	if object.Shaping == nil {
		return snapshotDecodeError("shape object %q has no shaping parameters", object.RuleID)
	}
	interfaceName := shapeObjectInterface(object)
	if interfaceName == "" {
		return snapshotDecodeError("shape object %q has no interface attachment", object.RuleID)
	}
	output, err := commandOutput(results, "show ly-route smart-qos")
	if err != nil {
		return err
	}
	line := smartQoSInterfaceLine(output, interfaceName)
	if line == "" {
		return snapshotDecodeError("smart-QoS shaping is absent for interface %q", interfaceName)
	}
	fields := strings.Fields(line)
	values := map[string]string{}
	for index := 0; index+1 < len(fields); index++ {
		switch fields[index] {
		case "rate-kbps", "burst-bytes", "host-isolation":
			values[fields[index]] = fields[index+1]
		}
	}
	shaping := *object.Shaping
	wantRate := strconv.FormatUint((shaping.RateBPS+999)/1000, 10)
	if values["rate-kbps"] != wantRate {
		return snapshotDecodeError("smart-QoS interface %q rate %q does not match candidate %q", interfaceName, values["rate-kbps"], wantRate)
	}
	isolation := shaping.HostIsolation
	if isolation == flow.HostIsolationAuto {
		role := "lan"
		if shaping.Direction == "uplink" {
			role = "wan"
		}
		isolation = shaping.InterfaceHostIsolation(role)
	}
	if values["host-isolation"] != string(isolation) {
		return snapshotDecodeError("smart-QoS interface %q host isolation %q does not match candidate %q", interfaceName, values["host-isolation"], isolation)
	}
	// Older plugins predate burst-bytes. Only require the token when the
	// operator asked for a non-default burst; the default is still proven by
	// the rate and host-isolation contract.
	if shaping.BurstBPS != 0 {
		wantBurst := strconv.FormatUint(shaping.BurstBytes(), 10)
		if values["burst-bytes"] != wantBurst {
			return snapshotDecodeError("smart-QoS interface %q burst %q does not match candidate %q", interfaceName, values["burst-bytes"], wantBurst)
		}
	}
	return nil
}

func shapeObjectAbsent(results []VPPCTLCommandResult, object flow.VPPObject) (bool, error) {
	interfaceName := shapeObjectInterface(object)
	if interfaceName == "" {
		return false, snapshotDecodeError("shape object %q has no interface attachment", object.RuleID)
	}
	output, err := commandOutputAllowEmpty(results, "show ly-route smart-qos")
	if err != nil {
		return false, err
	}
	return !smartQoSInterfacePresent(output, interfaceName), nil
}

func shapeDeleteCommands(object flow.VPPObject) []string {
	interfaceName := shapeObjectInterface(object)
	if interfaceName == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("?set ly-route smart-qos interface %s disable", interfaceName),
		"show ly-route smart-qos",
	}
}
