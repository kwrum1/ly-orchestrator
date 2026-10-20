package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

func validatePPPoEReadback(ctx context.Context, runner CommandRunner, artifacts []RenderedArtifact) error {
	peerCount := 0
	for _, artifact := range artifacts {
		if !strings.HasPrefix(artifact.Path, "/etc/ly-route/pppoe/ly-route-") {
			continue
		}
		peerCount++
		var plan struct {
			ID         string `json:"id"`
			StatusFile string `json:"status_file"`
		}
		if err := json.Unmarshal([]byte(artifact.Content), &plan); err != nil || plan.ID == "" || plan.StatusFile == "" {
			return fmt.Errorf("PPPoE plan %s has no native status contract", artifact.Path)
		}
		if err := validateNativePPPoEReadback(ctx, runner, plan.ID, plan.StatusFile); err != nil {
			return err
		}
	}
	if peerCount == 0 {
		return fmt.Errorf("PPPoE artifacts contain no peer plans")
	}
	return nil
}

func validateNativePPPoEReadback(ctx context.Context, runner CommandRunner, peerID, statusFile string) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := validateNativePPPoEOnce(ctx, runner, statusFile); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("PPPoE peer %s did not converge: %w", peerID, errors.Join(lastErr, ctx.Err()))
		case <-deadline.C:
			return fmt.Errorf("PPPoE peer %s did not converge within 30s: %w", peerID, lastErr)
		case <-ticker.C:
		}
	}
}

func validateNativePPPoEOnce(ctx context.Context, runner CommandRunner, statusFile string) error {
	output, err := requiredOutput(ctx, runner, "cat", statusFile)
	if err != nil {
		return err
	}
	var status struct {
		State     string `json:"state"`
		Interface string `json:"interface"`
		Session   struct {
			SessionID    uint32 `json:"session_id"`
			LocalAddress string `json:"local_address"`
			PeerAddress  string `json:"peer_address"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return fmt.Errorf("decode native PPPoE status: %w", err)
	}
	if status.State != "connected" || status.Interface == "" {
		return fmt.Errorf("native PPPoE status is not connected")
	}
	local, err := netip.ParseAddr(strings.TrimSpace(status.Session.LocalAddress))
	if err != nil || !local.Is4() || local.IsUnspecified() {
		return fmt.Errorf("native PPPoE negotiated an invalid local address %q", status.Session.LocalAddress)
	}
	peer, err := netip.ParseAddr(strings.TrimSpace(status.Session.PeerAddress))
	if err != nil || !peer.Is4() || peer.IsUnspecified() {
		return fmt.Errorf("native PPPoE negotiated an invalid peer address %q", status.Session.PeerAddress)
	}
	if status.Session.SessionID == 0 || status.Session.SessionID >= 0xffff {
		return fmt.Errorf("native PPPoE status has an invalid session id %d", status.Session.SessionID)
	}
	if !validPPPoEInterfaceName(status.Interface) {
		return fmt.Errorf("native PPPoE status has an invalid interface %q", status.Interface)
	}

	// The native plugin owns its session table; `show pppoe session` is the
	// legacy built-in table and is empty on this build.  Read the authoritative
	// detail output first and only fall back to the legacy command.
	client, clientErr := requiredOutput(ctx, runner, "vppctl", "show", "pppoe", "client", "detail")
	matched := false
	if clientErr == nil && strings.Contains(client, "PPPOE_CLIENT_SESSION") {
		sessionID := fmt.Sprintf("session-id %d", status.Session.SessionID)
		ipv4 := fmt.Sprintf("runtime ipv4 local %s peer %s", local, peer)
		if !strings.Contains(client, sessionID) || !strings.Contains(client, ipv4) || !strings.Contains(client, status.Interface) {
			return fmt.Errorf("VPP PPPoE client detail does not match native session %d %s peer %s", status.Session.SessionID, local, peer)
		}
		matched = true
	}
	if !matched {
		vpp, sessionErr := requiredOutput(ctx, runner, "vppctl", "show", "pppoe", "session")
		if sessionErr != nil {
			return fmt.Errorf("VPP PPPoE readback is unavailable: %v", errors.Join(clientErr, sessionErr))
		}
		needle := fmt.Sprintf("client-ip %s session-id %d", local, status.Session.SessionID)
		if !strings.Contains(vpp, needle) {
			return fmt.Errorf("VPP PPPoE readback does not match native client status")
		}
	}

	addresses, err := requiredOutput(ctx, runner, "vppctl", "show", "interface", "address", status.Interface)
	if err != nil {
		return err
	}
	if !strings.Contains(addresses, local.String()+"/32") {
		return fmt.Errorf("VPP PPPoE interface %s does not own negotiated address %s", status.Interface, local)
	}
	return nil
}

func validateIPv6RAReadback(ctx context.Context, runner CommandRunner, artifacts []RenderedArtifact) error {
	content, err := artifactContent(artifacts, "/etc/radvd.conf")
	if err != nil {
		return err
	}
	interfaceName, prefix, err := expectedIPv6RA(content)
	if err != nil {
		return err
	}
	output, err := requiredOutput(ctx, runner, "radvdump")
	if err != nil {
		return err
	}
	normalized := normalizedLine(output)
	if !strings.Contains(normalized, "interface "+interfaceName) || !strings.Contains(normalized, "prefix "+prefix) {
		return fmt.Errorf("IPv6 RA live state missing interface %s delegated prefix %s", interfaceName, prefix)
	}
	return nil
}

func validPPPoEInterfaceName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "/\\ \t\r\n\"'") {
		return false
	}
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "pppoe_session") ||
		strings.HasPrefix(lower, "pppox") ||
		strings.HasPrefix(lower, "lyppp-")
}

func expectedIPv6RA(content string) (string, string, error) {
	var interfaceName, prefix string
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface" {
			interfaceName = fields[1]
		}
		if len(fields) == 2 && fields[0] == "prefix" {
			prefix = fields[1]
		}
	}
	if interfaceName == "" || prefix == "" || !strings.HasSuffix(prefix, "/64") {
		return "", "", fmt.Errorf("rendered IPv6 RA plan lacks delegated /64")
	}
	return interfaceName, prefix, nil
}
