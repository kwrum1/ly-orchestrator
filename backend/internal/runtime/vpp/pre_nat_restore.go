package vpp

import (
	"context"
	"fmt"
	"strings"
)

// RestorePreNATClassifier reinstalls the persisted previous-generation
// classifier header. The plugin keeps one ingress interface and one LAN prefix
// per appliance, so replaying policy rules is not enough: a failed apply may
// have moved the header to another LAN (or left it clear). Restoring the
// captured header makes the rollback deterministic and keeps the previous
// generation's classification intact.
func (a Adapter) RestorePreNATClassifier(ctx context.Context, transactionID string, state *PreNATClassifierState) error {
	if state == nil {
		return nil
	}
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" {
		return fmt.Errorf("%w: transaction ID is required", ErrSnapshotIncomplete)
	}
	commands := preNATClassifierRestoreCommands(state)
	if len(commands) == 0 {
		return nil
	}
	if a.Client == nil {
		return fmt.Errorf("%w: vpp client is not configured", ErrVPPUnavailable)
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("%w: open pre-NAT restore channel: %v", ErrVPPUnavailable, err)
	}
	defer channel.Close()
	return restorePreNATClassifierOnChannel(ctx, channel, transactionID, state, commands)
}

func preNATClassifierRestoreCommands(state *PreNATClassifierState) []string {
	if state == nil {
		return nil
	}
	if !state.Enabled {
		return []string{"?set ly-route pre-nat-route clear", "show ly-route pre-nat-route"}
	}
	interfaceName := strings.TrimSpace(state.InterfaceName)
	lanPrefix := strings.TrimSpace(state.LANPrefix)
	if interfaceName == "" || lanPrefix == "" {
		// An enabled classifier without a usable header is not restorable;
		// leave the live header alone rather than clearing working policy.
		return nil
	}
	return []string{
		fmt.Sprintf("?set ly-route pre-nat-route interface %s lan-prefix %s", interfaceName, lanPrefix),
		"show ly-route pre-nat-route",
	}
}

func restorePreNATClassifierOnChannel(ctx context.Context, channel Channel, transactionID string, state *PreNATClassifierState, commands []string) error {
	// The operation name must not start with "vpp.route-policy": the vppctl
	// channel routes that prefix into the ABF/pre-NAT policy lifecycle, which
	// requires a typed policy and deletes the policy it names. This operation
	// only rewrites the classifier header and must run as plain vppctl commands.
	reply, err := doOperation(ctx, channel, Operation{Name: "vpp.pre-nat-classifier.restore", RequestID: transactionID, VPPCtlCommands: commands})
	if err != nil {
		return fmt.Errorf("restore pre-NAT classifier: %w", err)
	}
	observed := observedPreNATClassifier(vppctlCommandResults(reply))
	if observed == nil {
		// Non-vppctl channels return no command results; the replay already
		// succeeded and there is no typed inventory to compare against.
		if len(vppctlCommandResults(reply)) == 0 {
			return nil
		}
		return fmt.Errorf("restore pre-NAT classifier: classifier readback is unavailable after replay")
	}
	if observed.Enabled != state.Enabled {
		return fmt.Errorf("restore pre-NAT classifier: enabled=%t, want %t", observed.Enabled, state.Enabled)
	}
	if state.Enabled {
		if observed.InterfaceName != strings.TrimSpace(state.InterfaceName) || observed.LANPrefix != strings.TrimSpace(state.LANPrefix) {
			return fmt.Errorf("restore pre-NAT classifier: got interface %q lan-prefix %q, want %q %q", observed.InterfaceName, observed.LANPrefix, state.InterfaceName, state.LANPrefix)
		}
	}
	return nil
}
