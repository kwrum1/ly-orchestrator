package vpp

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

type natRoleChannel struct {
	commands []string
	attached bool
	ignore   bool
}

func (channel *natRoleChannel) Close() error { return nil }

func (channel *natRoleChannel) Do(_ context.Context, operation Operation) (Reply, error) {
	results := make([]VPPCTLCommandResult, 0, len(operation.VPPCtlCommands))
	for _, command := range operation.VPPCtlCommands {
		command = strings.TrimPrefix(strings.TrimSpace(command), "?")
		channel.commands = append(channel.commands, command)
		stdout := ""
		switch {
		case strings.HasPrefix(command, "show nat44"):
			if channel.attached {
				stdout = "lydns-role in\nlyroute-eth2 out\n"
			}
		case strings.HasPrefix(command, "set interface nat44"):
			if !channel.ignore {
				channel.attached = true
			}
		}
		results = append(results, VPPCTLCommandResult{Command: command, Stdout: stdout})
	}
	return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}, nil
}

func TestEnsureDNSServiceNetworkNATRepairsMissingInputRole(t *testing.T) {
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_ATTEMPTS", "2")
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_INTERVAL", "1ms")
	network := DNSServiceNetwork{UpstreamID: "dns-lab", VPPInterface: "lydns-role", UnderlayRoute: "lyroute-eth2"}
	channel := &natRoleChannel{}
	if err := ensureDNSServiceNetworkNAT(context.Background(), channel, network, nat.BehaviorEndpointDependent); err != nil {
		t.Fatalf("NAT role repair failed: %v", err)
	}
	joined := strings.Join(channel.commands, "\n")
	if !strings.Contains(joined, "set interface nat44 in lydns-role out lyroute-eth2") {
		t.Fatalf("NAT role attach command missing:\n%s", joined)
	}
}

func TestEnsureDNSServiceNetworkNATFailsWithActionableReason(t *testing.T) {
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_ATTEMPTS", "2")
	t.Setenv("LY_ROUTE_DNS_NAT_ROLE_INTERVAL", "1ms")
	network := DNSServiceNetwork{UpstreamID: "dns-lab", VPPInterface: "lydns-role", UnderlayRoute: "lyroute-eth2"}
	channel := &natRoleChannel{ignore: true}
	err := ensureDNSServiceNetworkNAT(context.Background(), channel, network, nat.BehaviorEndpointDependent)
	if err == nil {
		t.Fatal("missing NAT role must fail closed")
	}
	for _, want := range []string{"lydns-role", "NAT44 input role", "lyroute-eth2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
}
