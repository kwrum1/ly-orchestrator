package vpp

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MirrorConfig is the desired port-mirroring (SPAN) generation for the
// appliance. One physical data port is the mirror destination; any number of
// other data ports are mirrored into it. VPP owns the session, so the desired
// generation is replayed by the durable vpp operations file after every VPP
// restart instead of being written into startup.conf.
type MirrorConfig struct {
	// Destination is the VPP interface name of the mirror port.
	Destination string `json:"destination"`
	// LinuxInterface is the kernel/product name of the mirror port and is only
	// carried for diagnostics.
	LinuxInterface string         `json:"linux_interface,omitempty"`
	Sources        []MirrorSource `json:"sources"`
}

// MirrorSource is one mirrored ingress. L2 keeps non-IP frames in the copy,
// but VPP only registers span-l2-input/output on interfaces that traverse the
// L2 feature arc (bridged ports). Routed gateway ports must use L2=false or no
// frame is copied; the product therefore defaults L2 off and keeps the flag for
// bridged deployments.
type MirrorSource struct {
	Interface      string `json:"interface"`
	LinuxInterface string `json:"linux_interface,omitempty"`
	Direction      string `json:"direction"`
	L2             bool   `json:"l2"`
}

const (
	MirrorDirectionRX   = "rx"
	MirrorDirectionTX   = "tx"
	MirrorDirectionBoth = "both"
)

// NormalizeMirrorDirection validates the operator-facing direction word.
// An empty value means "both", which is the product default.
func NormalizeMirrorDirection(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", MirrorDirectionBoth:
		return MirrorDirectionBoth, nil
	case MirrorDirectionRX:
		return MirrorDirectionRX, nil
	case MirrorDirectionTX:
		return MirrorDirectionTX, nil
	}
	return "", fmt.Errorf("mirror direction must be one of rx, tx, both")
}

// MirrorSpanCommands renders the disable-first, then create sequence.
//
// VPP keeps one session per source and mode. `set interface span <if> l2
// disable` only clears the L2 session, and the plain `disable` only clears the
// non-L2 session (verified on the lab device), so cleanup has to emit both.
// Disable is emitted for every candidate source before any destination is
// bound: rebinding or removing a mirror must never leave the stale
// "DELETED (idx)" row the CLI documents.
func MirrorSpanCommands(mirror *MirrorConfig, cleanupInterfaces []string) []string {
	cleanup := map[string]struct{}{}
	for _, name := range cleanupInterfaces {
		if name = strings.TrimSpace(name); name != "" {
			cleanup[name] = struct{}{}
		}
	}
	if mirror != nil {
		// A previous destination can be a new source and vice versa, so both
		// sides of the new generation are cleaned first.
		if name := strings.TrimSpace(mirror.Destination); name != "" {
			cleanup[name] = struct{}{}
		}
		for _, source := range mirror.Sources {
			if name := strings.TrimSpace(source.Interface); name != "" {
				cleanup[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(cleanup))
	for name := range cleanup {
		names = append(names, name)
	}
	sort.Strings(names)
	commands := make([]string, 0, len(names)*2+len(mirrorSources(mirror))+1)
	for _, name := range names {
		commands = append(commands,
			"?set interface span "+name+" l2 disable",
			"?set interface span "+name+" disable",
		)
	}
	if mirror != nil {
		for _, source := range mirror.Sources {
			direction, err := NormalizeMirrorDirection(source.Direction)
			if err != nil {
				direction = MirrorDirectionBoth
			}
			if source.L2 {
				commands = append(commands, fmt.Sprintf("set interface span %s l2 destination %s %s", source.Interface, mirror.Destination, direction))
				continue
			}
			commands = append(commands, fmt.Sprintf("set interface span %s destination %s %s", source.Interface, mirror.Destination, direction))
		}
	}
	// The readback is part of the operation so the apply receipt proves the
	// session exists with the requested direction instead of only proving the
	// command was accepted.
	commands = append(commands, "show interface span")
	return commands
}

func mirrorSources(mirror *MirrorConfig) []MirrorSource {
	if mirror == nil {
		return nil
	}
	return mirror.Sources
}

// mirrorSpanReadback returns the last `show interface span` output. Unlike the
// generic supplemental helper, an empty table is valid evidence (no session).
func mirrorSpanReadback(results []VPPCTLCommandResult) (string, error) {
	for index := len(results) - 1; index >= 0; index-- {
		if results[index].Command == "show interface span" {
			return results[index].Stdout, nil
		}
	}
	return "", snapshotDecodeError("supplemental command %q is missing", "show interface span")
}

// desired returns nil when this payload is the empty (no mirror) generation.
// Supplemental verification then requires the live span table to be empty.
func (mirror MirrorConfig) desired() *MirrorConfig {
	if strings.TrimSpace(mirror.Destination) == "" {
		return nil
	}
	copy := mirror
	return &copy
}

// MirrorCleanupCommands disables every session this generation could own. It
// is used by rollback, which must remove the mirror before its interfaces are
// detached.
func MirrorCleanupCommands(mirror MirrorConfig) []string {
	names := map[string]struct{}{}
	if name := strings.TrimSpace(mirror.Destination); name != "" {
		names[name] = struct{}{}
	}
	for _, source := range mirror.Sources {
		if name := strings.TrimSpace(source.Interface); name != "" {
			names[name] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	commands := make([]string, 0, len(sorted)*2+1)
	for _, name := range sorted {
		commands = append(commands,
			"?set interface span "+name+" l2 disable",
			"?set interface span "+name+" disable",
		)
	}
	return append(commands, "show interface span")
}

// MirrorSession is one parsed row of `show interface span`.
type MirrorSession struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Direction   string `json:"direction"`
	L2          bool   `json:"l2"`
}

// mirrorSessionPattern matches one span row. VPP pads inside the parentheses
// (`(    rx)`), so splitting on whitespace would tear the direction apart.
var mirrorSessionPattern = regexp.MustCompile(`^\s*(\S+)\s+(\S+)\s+\(\s*(\S+)\s*\)\s+\(\s*(\S+)\s*\)\s*$`)

// ParseMirrorSessions decodes the VPP CLI table:
//
//	Source                           Destination                       Device       L2
//	lyroute-enp2s0                   lyroute-enp3s0                   (  none) (  both)
//
// An empty output means no session is configured. Rows whose first field is
// the header are ignored.
func ParseMirrorSessions(output string) []MirrorSession {
	sessions := []MirrorSession{}
	for _, line := range strings.Split(output, "\n") {
		matches := mirrorSessionPattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		// VPP keeps the rx/tx (non-L2) setting in the Device column and the
		// L2 setting in the L2 column. They are independent sessions: a row can
		// carry both `(    rx)` and `(  both)`.
		device := strings.ToLower(matches[3])
		l2Direction := strings.ToLower(matches[4])
		if device != "" && device != "none" {
			sessions = append(sessions, MirrorSession{Source: matches[1], Destination: matches[2], Direction: device, L2: false})
		}
		if l2Direction != "" && l2Direction != "none" {
			sessions = append(sessions, MirrorSession{Source: matches[1], Destination: matches[2], Direction: l2Direction, L2: true})
		}
	}
	return sessions
}

// MirrorReadbackMatches proves the live session table is exactly the desired
// generation. An empty desired generation requires an empty table, so removing
// the mirror port is verified rather than assumed.
func MirrorReadbackMatches(output string, mirror *MirrorConfig) error {
	live := ParseMirrorSessions(output)
	if mirror == nil || strings.TrimSpace(mirror.Destination) == "" {
		if len(live) == 0 {
			return nil
		}
		return fmt.Errorf("port mirroring is disabled but VPP still reports %d span session(s)", len(live))
	}
	sessionKey := func(session MirrorSession) string {
		return session.Source + "|" + fmt.Sprintf("%t", session.L2)
	}
	expected := map[string]MirrorSession{}
	for _, source := range mirror.Sources {
		direction, _ := NormalizeMirrorDirection(source.Direction)
		wanted := MirrorSession{
			Source:      strings.TrimSpace(source.Interface),
			Destination: strings.TrimSpace(mirror.Destination),
			Direction:   direction,
			L2:          source.L2,
		}
		expected[sessionKey(wanted)] = wanted
	}
	seen := map[string]MirrorSession{}
	for _, session := range live {
		if session.Destination != strings.TrimSpace(mirror.Destination) {
			return fmt.Errorf("unexpected span session %s -> %s (%s)", session.Source, session.Destination, session.Direction)
		}
		key := sessionKey(session)
		want, wanted := expected[key]
		if !wanted {
			return fmt.Errorf("unexpected span session from %s (%s, l2=%t)", session.Source, session.Direction, session.L2)
		}
		if session.Direction != want.Direction {
			return fmt.Errorf("span session %s direction is %s, expected %s", session.Source, session.Direction, want.Direction)
		}
		seen[key] = session
	}
	for key, want := range expected {
		if _, found := seen[key]; !found {
			return fmt.Errorf("span session %s -> %s (%s, l2=%t) is absent", want.Source, want.Destination, want.Direction, want.L2)
		}
	}
	return nil
}
