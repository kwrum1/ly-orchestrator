package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	orchdomain "ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/orchestratorapi"
)

const (
	defaultBypassInterval   = 2 * time.Second
	defaultBypassTimeout    = time.Second
	defaultBypassFailures   = 3
	defaultBypassRecoveries = 3
	defaultBypassWindow     = 10 * time.Second
	bypassStateVersion      = 1
)

type bypassPersistedGroup struct {
	Group   string    `json:"group"`
	Enabled bool      `json:"enabled"`
	Forced  bool      `json:"forced"`
	Cause   string    `json:"cause,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitempty"`
}

type bypassPersistedState struct {
	Version   int                          `json:"version"`
	Config    orchestratorapi.BypassConfig `json:"config"`
	Enabled   bool                         `json:"enabled"`
	Forced    bool                         `json:"forced"`
	Cause     string                       `json:"cause,omitempty"`
	Reason    string                       `json:"reason,omitempty"`
	Since     time.Time                    `json:"since,omitempty"`
	UpdatedAt time.Time                    `json:"updated_at"`
	// Groups carries the per-group forced/active state so an operator force
	// survives a restart.  Automatic state is re-derived from probes.
	Groups []bypassPersistedGroup `json:"groups,omitempty"`
}

// bypassFlowSample is one cumulative readback of a group's passive targets.
type bypassFlowSample struct {
	at       time.Time
	attempts uint64
	success  uint64
	rst      uint64
}

// bypassGroupRuntime is one group's detector and bypass state.
type bypassGroupRuntime struct {
	name    string
	enabled bool
	forced  bool
	cause   string
	reason  string
	since   time.Time

	failures    int
	successes   int
	lastProbeOK bool
	lastProbeAt time.Time
	probes      []orchestratorapi.BypassProbe

	samples []bypassFlowSample
	// window counters are the values inside the trailing window.
	windowAttempts uint64
	windowSuccess  uint64
	windowRST      uint64
	windowTimeouts uint64
	flowStats      []orchestratorapi.BypassFlowStats
	bypassPackets  uint64
	health         string
}

// bypassController is the fail-open supervisor for the transparent
// orchestrator.  It probes configured interfaces and endpoints, and toggles the
// plugin bypass flag when the failure/recovery thresholds are crossed.  The
// plugin keeps the configured policy; bypass only suppresses its evaluation.
//
// The global flag is legacy behaviour.  When config.Groups is non-empty each
// group is detected and bypassed independently: one group may bypass while the
// other keeps enforcing policy.
type bypassController struct {
	mu         sync.Mutex
	runtime    *productionOrchestratorRuntime
	repository *orchdomain.Repository
	statePath  string
	sysfsRoot  string
	now        func() time.Time

	config    orchestratorapi.BypassConfig
	enabled   bool
	forced    bool
	cause     string
	reason    string
	since     time.Time
	failures  int
	successes int

	groups map[string]*bypassGroupRuntime

	lastProbeAt time.Time
	lastProbeOK bool
	probes      []orchestratorapi.BypassProbe

	probeSignature        string
	appliedProbeSignature string

	probeTCP  func(context.Context, string, time.Duration) error
	probeICMP func(context.Context, string, time.Duration) error
	probeDial func(context.Context, string, string, time.Duration) error

	started bool
}

func newBypassController(runtime *productionOrchestratorRuntime, repository *orchdomain.Repository, statePath, sysfsRoot, tcpTargets, icmpTargets, interfaces string) *bypassController {
	controller := &bypassController{
		runtime:    runtime,
		repository: repository,
		statePath:  strings.TrimSpace(statePath),
		sysfsRoot:  strings.TrimSpace(sysfsRoot),
		now:        func() time.Time { return time.Now().UTC() },
		groups:     map[string]*bypassGroupRuntime{},
	}
	if controller.sysfsRoot == "" {
		controller.sysfsRoot = "/sys"
	}
	config, _ := normalizeBypassConfig(orchestratorapi.BypassConfig{
		Enabled:             true,
		IntervalSeconds:     defaultBypassInterval.Seconds(),
		TimeoutMilliseconds: int(defaultBypassTimeout / time.Millisecond),
		FailureThreshold:    defaultBypassFailures,
		RecoveryThreshold:   defaultBypassRecoveries,
		Interfaces:          splitList(interfaces),
		TCPTargets:          splitList(tcpTargets),
		ICMPTargets:         splitList(icmpTargets),
	})
	controller.config = config
	controller.probeTCP = tcpProbe
	controller.probeICMP = icmpProbe
	controller.probeDial = tcpSourceProbe
	return controller
}

func normalizeBypassConfig(config orchestratorapi.BypassConfig) (orchestratorapi.BypassConfig, error) {
	if config.IntervalSeconds == 0 {
		config.IntervalSeconds = defaultBypassInterval.Seconds()
	}
	if config.TimeoutMilliseconds == 0 {
		config.TimeoutMilliseconds = int(defaultBypassTimeout / time.Millisecond)
	}
	if config.FailureThreshold == 0 {
		config.FailureThreshold = defaultBypassFailures
	}
	if config.RecoveryThreshold == 0 {
		config.RecoveryThreshold = defaultBypassRecoveries
	}
	if config.IntervalSeconds < 0.1 || config.IntervalSeconds > 3600 {
		return config, errors.New("interval_seconds must be between 0.1 and 3600")
	}
	if config.TimeoutMilliseconds < 50 || config.TimeoutMilliseconds > 30000 {
		return config, errors.New("timeout_ms must be between 50 and 30000")
	}
	if config.FailureThreshold < 1 || config.FailureThreshold > 100 {
		return config, errors.New("failure_threshold must be between 1 and 100")
	}
	if config.RecoveryThreshold < 1 || config.RecoveryThreshold > 100 {
		return config, errors.New("recovery_threshold must be between 1 and 100")
	}
	config.Interfaces = uniqueList(config.Interfaces)
	for _, name := range config.Interfaces {
		if strings.ContainsAny(name, " \t\r\n/") {
			return config, fmt.Errorf("invalid interface %q", name)
		}
	}
	config.TCPTargets = uniqueList(config.TCPTargets)
	for _, target := range config.TCPTargets {
		host, port, err := net.SplitHostPort(target)
		if err != nil || strings.TrimSpace(host) == "" {
			return config, fmt.Errorf("invalid tcp target %q (want host:port)", target)
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return config, fmt.Errorf("invalid tcp target port %q", target)
		}
	}
	config.ICMPTargets = uniqueList(config.ICMPTargets)
	for _, target := range config.ICMPTargets {
		if _, err := netip.ParseAddr(target); err != nil {
			return config, fmt.Errorf("invalid icmp target %q", target)
		}
	}
	seenGroups := map[string]bool{}
	normalizedGroups := make([]orchestratorapi.BypassGroupConfig, 0, len(config.Groups))
	for _, group := range config.Groups {
		group.Name = strings.TrimSpace(group.Name)
		if group.Name == "" {
			return config, errors.New("group name is required")
		}
		if strings.ContainsAny(group.Name, " \t\r\n") {
			return config, fmt.Errorf("invalid group name %q", group.Name)
		}
		if seenGroups[group.Name] {
			return config, fmt.Errorf("duplicate group %q", group.Name)
		}
		seenGroups[group.Name] = true
		group.Interfaces = uniqueList(group.Interfaces)
		for _, name := range group.Interfaces {
			if strings.ContainsAny(name, " \t\r\n/") {
				return config, fmt.Errorf("invalid interface %q for group %q", name, group.Name)
			}
		}
		if group.WindowSeconds == 0 {
			group.WindowSeconds = defaultBypassWindow.Seconds()
		}
		if group.WindowSeconds < 1 || group.WindowSeconds > 3600 {
			return config, fmt.Errorf("group %q window_seconds must be between 1 and 3600", group.Name)
		}
		if group.FailureThreshold == 0 {
			group.FailureThreshold = config.FailureThreshold
		}
		if group.RecoveryThreshold == 0 {
			group.RecoveryThreshold = config.RecoveryThreshold
		}
		if group.FailureThreshold < 1 || group.FailureThreshold > 100 || group.RecoveryThreshold < 1 || group.RecoveryThreshold > 100 {
			return config, fmt.Errorf("group %q thresholds must be between 1 and 100", group.Name)
		}
		group.TCPTargets = normalizeGroupTCPTargets(group.TCPTargets)
		for _, target := range group.TCPTargets {
			host, port, err := net.SplitHostPort(target.Target)
			if err != nil || strings.TrimSpace(host) == "" {
				return config, fmt.Errorf("group %q has invalid tcp target %q (want host:port)", group.Name, target.Target)
			}
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return config, fmt.Errorf("group %q has invalid tcp target port %q", group.Name, target.Target)
			}
			if target.Source != "" {
				if _, err := netip.ParseAddr(target.Source); err != nil {
					return config, fmt.Errorf("group %q has invalid tcp source %q", group.Name, target.Source)
				}
			}
		}
		group.FlowTargets = normalizeGroupFlowTargets(group.FlowTargets)
		for _, target := range group.FlowTargets {
			if _, err := netip.ParseAddr(target.DstIP); err != nil {
				return config, fmt.Errorf("group %q has invalid flow dst_ip %q", group.Name, target.DstIP)
			}
			if target.DstPort < 1 || target.DstPort > 65535 {
				return config, fmt.Errorf("group %q flow dst_port must be between 1 and 65535", group.Name)
			}
			protocol := strings.ToLower(strings.TrimSpace(target.Protocol))
			if protocol != "" && protocol != "tcp" && protocol != "udp" {
				return config, fmt.Errorf("group %q flow protocol must be tcp or udp", group.Name)
			}
		}
		normalizedGroups = append(normalizedGroups, group)
	}
	sort.Slice(normalizedGroups, func(i, j int) bool { return normalizedGroups[i].Name < normalizedGroups[j].Name })
	config.Groups = normalizedGroups
	return config, nil
}

func normalizeGroupTCPTargets(targets []orchestratorapi.BypassTCPTarget) []orchestratorapi.BypassTCPTarget {
	result := make([]orchestratorapi.BypassTCPTarget, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		target.Target = strings.TrimSpace(target.Target)
		target.Source = strings.TrimSpace(target.Source)
		key := target.Target + "|" + target.Source
		if target.Target == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, target)
	}
	return result
}

func normalizeGroupFlowTargets(targets []orchestratorapi.BypassFlowTarget) []orchestratorapi.BypassFlowTarget {
	result := make([]orchestratorapi.BypassFlowTarget, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		target.DstIP = strings.TrimSpace(target.DstIP)
		target.Protocol = strings.ToLower(strings.TrimSpace(target.Protocol))
		if target.Protocol == "" {
			target.Protocol = "tcp"
		}
		key := fmt.Sprintf("%s|%d|%s", target.DstIP, target.DstPort, target.Protocol)
		if target.DstIP == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, target)
	}
	return result
}

func splitList(value string) []string {
	return uniqueList(strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}))
}

func uniqueList(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (controller *bypassController) Start(ctx context.Context) {
	controller.mu.Lock()
	controller.loadLocked()
	controller.started = true
	enabled, forced, cause, reason := controller.enabled, controller.forced, controller.cause, controller.reason
	groups := controller.persistedGroupsLocked()
	controller.mu.Unlock()

	if enabled {
		if err := controller.runtime.SetBypass(ctx, true, nonEmptyValue(cause, "auto"), nonEmptyValue(reason, "restored_bypass_state")); err != nil {
			log.Printf("orchestrator fault bypass restore failed: %v", err)
		}
	}
	for _, group := range groups {
		if !group.Enabled && !group.Forced {
			continue
		}
		state := &bypassGroupRuntime{name: group.Group, enabled: group.Enabled, forced: group.Forced, cause: group.Cause, reason: group.Reason, since: group.Since}
		if err := controller.runtime.SetGroupBypass(ctx, group.Group, group.Enabled, nonEmptyValue(group.Cause, "auto"), nonEmptyValue(group.Reason, "restored_group_bypass")); err != nil {
			log.Printf("orchestrator group bypass restore failed for %s: %v", group.Group, err)
			continue
		}
		controller.mu.Lock()
		controller.groups[group.Group] = state
		controller.mu.Unlock()
	}
	if forced {
		log.Printf("orchestrator fault bypass restored forced state: %s", reason)
	}
	go controller.run(ctx)
}

func (controller *bypassController) persistedGroupsLocked() []bypassPersistedGroup {
	result := make([]bypassPersistedGroup, 0, len(controller.groups))
	for _, group := range controller.groups {
		if !group.enabled && !group.forced {
			continue
		}
		result = append(result, bypassPersistedGroup{Group: group.name, Enabled: group.enabled, Forced: group.forced, Cause: group.cause, Reason: group.reason, Since: group.since})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Group < result[j].Group })
	return result
}

func (controller *bypassController) run(ctx context.Context) {
	controller.probeOnce(ctx)
	for {
		controller.mu.Lock()
		interval := time.Duration(controller.config.IntervalSeconds * float64(time.Second))
		controller.mu.Unlock()
		if interval <= 0 {
			interval = defaultBypassInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			controller.probeOnce(ctx)
		}
	}
}

func (controller *bypassController) Load() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.loadLocked()
	return nil
}

func (controller *bypassController) loadLocked() {
	path := strings.TrimSpace(controller.statePath)
	if path == "" {
		return
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("read orchestrator fault-bypass state: %v", err)
		return
	}
	var persisted bypassPersistedState
	if err := json.Unmarshal(content, &persisted); err != nil {
		log.Printf("decode orchestrator fault-bypass state: %v", err)
		return
	}
	if persisted.Version != bypassStateVersion {
		log.Printf("ignore orchestrator fault-bypass state version %d", persisted.Version)
		return
	}
	config, err := normalizeBypassConfig(persisted.Config)
	if err != nil {
		log.Printf("ignore invalid persisted orchestrator fault-bypass config: %v", err)
		return
	}
	controller.config = config
	controller.enabled = persisted.Enabled
	controller.forced = persisted.Forced
	controller.cause = persisted.Cause
	controller.reason = persisted.Reason
	controller.since = persisted.Since
	for _, group := range persisted.Groups {
		controller.groups[group.Group] = &bypassGroupRuntime{name: group.Group, enabled: group.Enabled, forced: group.Forced, cause: group.Cause, reason: group.Reason, since: group.Since}
	}
	controller.ensureGroupsLocked()
}

// ensureGroupsLocked keeps one detector per configured group and drops state
// for groups that are no longer configured (unless an operator forced them).
func (controller *bypassController) ensureGroupsLocked() {
	configured := map[string]bool{}
	for _, group := range controller.config.Groups {
		configured[group.Name] = true
		if _, ok := controller.groups[group.Name]; !ok {
			controller.groups[group.Name] = &bypassGroupRuntime{name: group.Name, health: "unknown"}
		}
	}
	for name, state := range controller.groups {
		if !configured[name] && !state.forced {
			delete(controller.groups, name)
		}
	}
}

func (controller *bypassController) persistLocked() {
	path := strings.TrimSpace(controller.statePath)
	if path == "" {
		return
	}
	persisted := bypassPersistedState{
		Version:   bypassStateVersion,
		Config:    controller.config,
		Enabled:   controller.enabled,
		Forced:    controller.forced,
		Cause:     controller.cause,
		Reason:    controller.reason,
		Since:     controller.since,
		UpdatedAt: controller.now(),
		Groups:    controller.persistedGroupsLocked(),
	}
	content, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		log.Printf("encode orchestrator fault-bypass state: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		log.Printf("create orchestrator fault-bypass state directory: %v", err)
		return
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0600); err != nil {
		log.Printf("write orchestrator fault-bypass state: %v", err)
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		log.Printf("commit orchestrator fault-bypass state: %v", err)
		_ = os.Remove(temporary)
	}
}

func (controller *bypassController) BypassStatus(ctx context.Context) (orchestratorapi.BypassState, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.statusLocked(), nil
}

func (controller *bypassController) statusLocked() orchestratorapi.BypassState {
	runtime := controller.runtime.BypassSnapshot()
	state := orchestratorapi.BypassState{
		Enabled:              controller.enabled || runtime.Enabled,
		Forced:               controller.forced || runtime.Forced,
		Cause:                controller.cause,
		Reason:               controller.reason,
		Since:                controller.since,
		ConsecutiveFailures:  controller.failures,
		ConsecutiveSuccesses: controller.successes,
		LastProbeOK:          controller.lastProbeOK,
		LastProbeAt:          controller.lastProbeAt,
		Probes:               append([]orchestratorapi.BypassProbe(nil), controller.probes...),
		Config:               controller.config,
		Generation:           runtime.Generation,
		BypassPackets:        runtime.Packets,
	}
	for _, group := range controller.config.Groups {
		runtimeGroup, ok := runtime.Groups[group.Name]
		row := orchestratorapi.BypassGroupState{
			Group:         group.Name,
			Health:        "unknown",
			WindowSeconds: group.WindowSeconds,
			TCPTargets:    group.TCPTargets,
			FlowTargets:   group.FlowTargets,
			Interfaces:    group.Interfaces,
		}
		if state, ok := controller.groups[group.Name]; ok {
			row.Enabled = state.enabled
			row.Forced = state.forced
			row.Cause = state.cause
			row.Reason = state.reason
			row.Since = state.since
			row.ConsecutiveFailures = state.failures
			row.ConsecutiveSuccesses = state.successes
			row.LastProbeOK = state.lastProbeOK
			row.LastProbeAt = state.lastProbeAt
			row.Probes = append([]orchestratorapi.BypassProbe(nil), state.probes...)
			row.FlowAttempts = state.windowAttempts
			row.FlowSuccess = state.windowSuccess
			row.FlowRST = state.windowRST
			row.FlowTimeouts = state.windowTimeouts
			row.FlowStats = append([]orchestratorapi.BypassFlowStats(nil), state.flowStats...)
			row.BypassPackets = state.bypassPackets
			if state.health != "" {
				row.Health = state.health
			}
			if state.enabled {
				row.Health = "bypass"
			}
		}
		if ok {
			row.Enabled = row.Enabled || runtimeGroup.Enabled
			if runtimeGroup.Enabled {
				row.Health = "bypass"
			}
			row.BypassPackets = runtimeGroup.Packets
			if row.FlowStats == nil {
				row.FlowStats = flowStatsForGroup(runtime.Flow, group.Name)
			}
		}
		state.Groups = append(state.Groups, row)
		state.BypassPackets += row.BypassPackets
		if row.Enabled {
			state.Enabled = true
			if !state.Forced {
				state.Forced = row.Forced
			}
		}
	}
	if state.Cause == "" {
		state.Cause = runtime.Cause
	}
	if state.Reason == "" {
		state.Reason = runtime.Reason
	}
	if state.Since.IsZero() {
		state.Since = runtime.Since
	}
	if state.Enabled && state.Cause == "" && len(state.Groups) > 0 {
		causes := make([]string, 0)
		for _, group := range state.Groups {
			if group.Enabled {
				causes = append(causes, fmt.Sprintf("%s(%s)", group.Group, nonEmptyValue(group.Cause, "auto")))
			}
		}
		state.Cause = "auto"
		if len(causes) > 0 {
			state.Reason = "groups:" + strings.Join(causes, ",")
		}
	}
	if !state.Enabled {
		state.Cause = ""
		state.Reason = ""
		state.Since = time.Time{}
	}
	return state
}

func (controller *bypassController) SetBypass(ctx context.Context, enabled bool, reason string) (orchestratorapi.BypassState, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if enabled {
		cause := "manual"
		if strings.TrimSpace(reason) == "" {
			reason = "manual_force"
		}
		if err := controller.runtime.SetBypass(ctx, true, cause, reason); err != nil {
			return orchestratorapi.BypassState{}, err
		}
		controller.enabled = true
		controller.forced = true
		controller.cause = cause
		controller.reason = reason
		controller.since = controller.now()
		log.Printf("orchestrator fault bypass forced by operator: %s", reason)
	} else {
		cause := "manual"
		if strings.TrimSpace(reason) == "" {
			reason = "manual_clear"
		}
		if err := controller.runtime.SetBypass(ctx, false, cause, reason); err != nil {
			return orchestratorapi.BypassState{}, err
		}
		controller.enabled = false
		controller.forced = false
		controller.cause = cause
		controller.reason = reason
		controller.since = time.Time{}
		controller.failures = 0
		controller.successes = 0
		log.Printf("orchestrator fault bypass cleared by operator: %s", reason)
	}
	controller.persistLocked()
	return controller.statusLocked(), nil
}

// SetGroupBypass toggles one group's bypass.  Other groups keep their own
// state and the global flag is untouched.
func (controller *bypassController) SetGroupBypass(ctx context.Context, group string, enabled bool, reason string) (orchestratorapi.BypassState, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return controller.SetBypass(ctx, enabled, reason)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !controller.groupConfiguredLocked(group) {
		return orchestratorapi.BypassState{}, fmt.Errorf("unknown orchestration group %q", group)
	}
	cause := "manual"
	if strings.TrimSpace(reason) == "" {
		if enabled {
			reason = "manual_force"
		} else {
			reason = "manual_clear"
		}
	}
	if err := controller.runtime.SetGroupBypass(ctx, group, enabled, cause, reason); err != nil {
		return orchestratorapi.BypassState{}, err
	}
	state := controller.groupLocked(group)
	state.enabled = enabled
	state.forced = enabled
	state.cause = cause
	state.reason = reason
	state.failures = 0
	state.successes = 0
	if enabled {
		state.since = controller.now()
		state.health = "bypass"
	} else {
		state.since = time.Time{}
		state.health = "unknown"
	}
	controller.ensureGroupsLocked()
	controller.persistLocked()
	action := "cleared"
	if enabled {
		action = "enabled"
	}
	log.Printf("orchestrator group bypass %s for %s by operator: %s", action, group, reason)
	return controller.statusLocked(), nil
}

func (controller *bypassController) groupConfiguredLocked(group string) bool {
	for _, item := range controller.config.Groups {
		if item.Name == group {
			return true
		}
	}
	return false
}

func (controller *bypassController) groupLocked(name string) *bypassGroupRuntime {
	state, ok := controller.groups[name]
	if !ok {
		state = &bypassGroupRuntime{name: name, health: "unknown"}
		controller.groups[name] = state
	}
	return state
}

func (controller *bypassController) UpdateBypassConfig(ctx context.Context, config orchestratorapi.BypassConfig) (orchestratorapi.BypassState, error) {
	normalized, err := normalizeBypassConfig(config)
	if err != nil {
		return orchestratorapi.BypassState{}, err
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.config = normalized
	controller.failures = 0
	controller.successes = 0
	controller.probes = nil
	controller.lastProbeOK = false
	controller.ensureGroupsLocked()
	for name, group := range controller.groups {
		if group.forced {
			continue
		}
		if !normalized.Enabled && group.enabled {
			if err := controller.runtime.SetGroupBypass(ctx, name, false, "config", "detector_disabled"); err != nil {
				return orchestratorapi.BypassState{}, err
			}
			group.enabled = false
			group.cause = "config"
			group.reason = "detector_disabled"
			group.since = time.Time{}
		}
		group.failures = 0
		group.successes = 0
		group.probes = nil
		group.samples = nil
		group.flowStats = nil
		group.lastProbeOK = false
		group.health = "unknown"
	}
	if !normalized.Enabled && controller.enabled && !controller.forced {
		if err := controller.runtime.SetBypass(ctx, false, "config", "detector_disabled"); err != nil {
			return orchestratorapi.BypassState{}, err
		}
		controller.enabled = false
		controller.cause = "config"
		controller.reason = "detector_disabled"
		controller.since = time.Time{}
	}
	if err := controller.syncProbeTargetsLocked(ctx, normalized); err != nil {
		log.Printf("orchestrator passive probe configuration failed: %v", err)
	}
	controller.persistLocked()
	return controller.statusLocked(), nil
}

// syncProbeTargetsLocked installs the passive flow targets in the plugin when
// the desired set changed or the plugin lost them (VPP restart).
func (controller *bypassController) syncProbeTargetsLocked(ctx context.Context, config orchestratorapi.BypassConfig) error {
	targets := probeTargetsFromConfig(config)
	signature := probeTargetSignature(targets)
	observed := controller.runtime.BypassSnapshot().ProbeTargets
	if signature == controller.appliedProbeSignature && (len(targets) == 0 || observed == len(targets)) {
		return nil
	}
	if err := controller.runtime.SetGroupProbes(ctx, targets); err != nil {
		return err
	}
	controller.probeSignature = signature
	controller.appliedProbeSignature = signature
	if len(targets) == 0 {
		log.Printf("orchestrator passive probe targets cleared")
	} else {
		log.Printf("orchestrator passive probe targets installed: %d target(s)", len(targets))
	}
	return nil
}

type groupProbeTarget struct {
	Group         string
	DstIP         string
	DstPort       int
	Protocol      string
	WindowSeconds float64
}

func probeTargetsFromConfig(config orchestratorapi.BypassConfig) []groupProbeTarget {
	result := []groupProbeTarget{}
	for _, group := range config.Groups {
		for _, target := range group.FlowTargets {
			protocol := strings.ToLower(strings.TrimSpace(target.Protocol))
			if protocol == "" {
				protocol = "tcp"
			}
			result = append(result, groupProbeTarget{Group: group.Name, DstIP: target.DstIP, DstPort: target.DstPort, Protocol: protocol, WindowSeconds: group.WindowSeconds})
		}
	}
	return result
}

func probeTargetSignature(targets []groupProbeTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, fmt.Sprintf("%s/%s/%d/%s/%.0f", target.Group, target.DstIP, target.DstPort, target.Protocol, target.WindowSeconds))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func (controller *bypassController) probeOnce(ctx context.Context) {
	controller.mu.Lock()
	if !controller.config.Enabled {
		controller.mu.Unlock()
		return
	}
	config := controller.config
	forced := controller.forced
	controller.mu.Unlock()

	// One VPP readback supplies both the passive counters and the group flag
	// readback used to re-assert state after a plugin restart.
	observed, observeErr := controller.runtime.ObserveBypassRuntime(ctx)
	if observeErr != nil {
		log.Printf("orchestrator bypass telemetry readback failed: %v", observeErr)
	}

	if len(config.Groups) > 0 {
		controller.probeGroups(ctx, config, observed, observeErr)
		if observeErr == nil {
			controller.mu.Lock()
			if err := controller.syncProbeTargetsLocked(ctx, config); err != nil {
				log.Printf("orchestrator passive probe re-install failed: %v", err)
			}
			controller.mu.Unlock()
		}
		return
	}

	probes := controller.collectProbes(ctx, config)
	allOK := true
	failed := make([]string, 0)
	for _, probe := range probes {
		if !probe.OK {
			allOK = false
			failed = append(failed, probe.Name+"="+nonEmptyValue(probe.Detail, "failed"))
		}
	}
	sort.Strings(failed)
	now := controller.now()

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !controller.config.Enabled || !reflect.DeepEqual(controller.config, config) {
		// A configuration update raced this round; discard the stale result.
		return
	}
	controller.lastProbeAt = now
	controller.lastProbeOK = allOK
	controller.probes = probes
	changed := false
	if allOK {
		controller.successes++
		controller.failures = 0
	} else {
		controller.failures++
		controller.successes = 0
	}

	if forced {
		// A forced bypass never auto-recovers; just make sure the plugin flag
		// survived a VPP restart.
		if !controller.runtime.BypassSnapshot().Enabled {
			if err := controller.runtime.SetBypass(ctx, true, controller.cause, controller.reason); err != nil {
				log.Printf("orchestrator fault bypass re-assert failed: %v", err)
			}
		}
		return
	}

	if !controller.enabled && controller.failures >= controller.config.FailureThreshold {
		reason := limitReason(fmt.Sprintf("probe_failed_%d/%d:%s", controller.failures, controller.config.FailureThreshold, strings.Join(failed, ",")))
		if err := controller.runtime.SetBypass(ctx, true, "auto", reason); err != nil {
			log.Printf("orchestrator fault bypass enter failed: %v", err)
			return
		}
		controller.enabled = true
		controller.cause = "auto"
		controller.reason = reason
		controller.since = now
		changed = true
		log.Printf("orchestrator fault bypass entered after %d failed rounds: %s", controller.failures, reason)
	} else if controller.enabled && !controller.forced && controller.successes >= controller.config.RecoveryThreshold {
		reason := fmt.Sprintf("probe_recovered_%d/%d", controller.successes, controller.config.RecoveryThreshold)
		if err := controller.runtime.SetBypass(ctx, false, "auto", reason); err != nil {
			log.Printf("orchestrator fault bypass exit failed: %v", err)
			return
		}
		controller.enabled = false
		controller.cause = "auto"
		controller.reason = reason
		controller.since = time.Time{}
		changed = true
		log.Printf("orchestrator fault bypass cleared after %d successful rounds", controller.successes)
	} else if controller.enabled && !controller.runtime.BypassSnapshot().Enabled {
		// VPP restarted and reloaded the plugin without the bypass flag.
		if err := controller.runtime.SetBypass(ctx, true, controller.cause, controller.reason); err != nil {
			log.Printf("orchestrator fault bypass re-assert failed: %v", err)
		}
	}
	if changed {
		controller.persistLocked()
	}
}

// probeGroups runs the independent per-group detector.
func (controller *bypassController) probeGroups(ctx context.Context, config orchestratorapi.BypassConfig, observed orchestratorapi.BypassRuntimeState, observeErr error) {
	now := controller.now()
	type outcome struct {
		name     string
		enable   bool
		disable  bool
		reason   string
		cause    string
		probes   []orchestratorapi.BypassProbe
		stats    []orchestratorapi.BypassFlowStats
		health   string
		failures int
		success  int
		probeOK  bool
	}
	outcomes := make([]outcome, 0, len(config.Groups))

	// Take the topology once for topology-derived group interfaces.
	var topologyInterfaces map[string][]string
	if controller.repository != nil {
		if topology, _, err := controller.repository.Snapshot(ctx); err == nil {
			topologyInterfaces = topologyGroupInterfaces(topology.View())
		}
	}

	for _, group := range config.Groups {
		interfaces := append([]string(nil), group.Interfaces...)
		if len(interfaces) == 0 && topologyInterfaces != nil {
			interfaces = append(interfaces, topologyInterfaces[group.Name]...)
		}
		probes := make([]orchestratorapi.BypassProbe, 0, len(interfaces)+len(group.TCPTargets))
		interfaceFailed := false
		for _, name := range interfaces {
			probe := probeInterface(controller.sysfsRoot, name, now)
			if !probe.OK {
				interfaceFailed = true
			}
			probes = append(probes, probe)
		}
		timeout := time.Duration(config.TimeoutMilliseconds) * time.Millisecond
		tcpFailed := false
		for _, target := range group.TCPTargets {
			probe := orchestratorapi.BypassProbe{Name: "tcp:" + target.Target, Kind: "tcp", Target: target.Target, CheckedAt: now}
			err := controller.probeDial(ctx, target.Target, target.Source, timeout)
			if err != nil {
				if target.Source != "" && isBindError(err) {
					// A host-side TCP probe needs a reachable source address.
					// When it cannot be bound the probe is inconclusive and
					// must not bypass the group.
					probe.Kind = "tcp-inconclusive"
					probe.Detail = "source_unreachable:" + sanitizeDetail(err.Error())
				} else {
					probe.Detail = sanitizeDetail(err.Error())
					tcpFailed = true
				}
			} else {
				probe.OK = true
				probe.Detail = "connected"
			}
			probes = append(probes, probe)
		}
		stats := flowStatsForGroup(observed.Flow, group.Name)
		roundFailed := interfaceFailed || tcpFailed
		outcome := outcome{name: group.Name, probes: probes, stats: stats}
		outcome.reason = bypassGroupReason(interfaceFailed, tcpFailed)
		outcome.probeOK = !roundFailed
		outcome.health = "unknown"
		outcomes = append(outcomes, outcome)
	}

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !controller.config.Enabled {
		return
	}
	// Re-read the config snapshot identity: a config update may have raced.
	if len(controller.config.Groups) != len(config.Groups) {
		return
	}
	for index := range outcomes {
		outcome := &outcomes[index]
		groupConfig := config.Groups[index]
		state := controller.groupLocked(outcome.name)
		window := time.Duration(groupConfig.WindowSeconds * float64(time.Second))
		if window <= 0 {
			window = defaultBypassWindow
		}
		state.observeFlow(now, outcome.stats, window)
		state.flowStats = outcome.stats
		state.lastProbeAt = now
		state.lastProbeOK = outcome.probeOK
		state.probes = outcome.probes
		state.health = groupHealth(state, groupConfig, outcome.probeOK)

		flowActive := len(groupConfig.FlowTargets) > 0
		passiveFailed := flowActive && state.windowAttempts > 0 && state.windowSuccess == 0
		activeProbes := len(outcome.probes) > 0
		roundConfigured := activeProbes || flowActive
		roundFailed := !outcome.probeOK

		if roundFailed {
			state.failures++
			state.successes = 0
		} else if roundConfigured && (!flowActive || state.windowAttempts > 0) {
			state.successes++
			state.failures = 0
		}

		if state.forced {
			// Operator force is terminal; only ensure the plugin survived a
			// restart.
			if runtimeGroup, ok := observed.Groups[state.name]; !ok || !runtimeGroup.Enabled {
				if err := controller.runtime.SetGroupBypass(ctx, state.name, true, nonEmptyValue(state.cause, "manual"), nonEmptyValue(state.reason, "manual_force")); err != nil {
					log.Printf("orchestrator group bypass re-assert failed for %s: %v", state.name, err)
				}
			}
			continue
		}

		enter := false
		reason := ""
		if !state.enabled {
			if passiveFailed {
				enter = true
				reason = limitReason(fmt.Sprintf("flow_no_response_%d/%d:%s", state.windowAttempts, state.windowRST, describeFlowTargets(groupConfig.FlowTargets)))
				state.failures = groupConfig.FailureThreshold
			} else if roundFailed && state.failures >= groupConfig.FailureThreshold {
				enter = true
				reason = limitReason(fmt.Sprintf("probe_failed_%d/%d:%s", state.failures, groupConfig.FailureThreshold, outcome.reason))
			}
			if enter {
				if err := controller.runtime.SetGroupBypass(ctx, state.name, true, "auto", reason); err != nil {
					log.Printf("orchestrator group bypass enter failed for %s: %v", state.name, err)
					continue
				}
				state.enabled = true
				state.cause = "auto"
				state.reason = reason
				state.since = now
				state.health = "bypass"
				log.Printf("orchestrator group bypass entered for %s: %s", state.name, reason)
			}
			continue
		}

		// Recovery needs positive evidence: interface/TCP rounds OK and, when
		// passive targets exist, successful responses inside the window.
		recovered := !roundFailed && state.successes >= groupConfig.RecoveryThreshold
		if flowActive {
			recovered = recovered && state.windowSuccess > 0 &&
				state.windowSuccess >= uint64(groupConfig.RecoveryThreshold)
		}
		if recovered {
			reason = fmt.Sprintf("probe_recovered_%d/%d", state.successes, groupConfig.RecoveryThreshold)
			if err := controller.runtime.SetGroupBypass(ctx, state.name, false, "auto", reason); err != nil {
				log.Printf("orchestrator group bypass exit failed for %s: %v", state.name, err)
				continue
			}
			state.enabled = false
			state.cause = "auto"
			state.reason = reason
			state.since = time.Time{}
			state.health = "healthy"
			state.failures = 0
			state.successes = 0
			log.Printf("orchestrator group bypass cleared for %s after %d successful rounds", state.name, state.successes)
		} else if observeErr == nil {
			runtimeGroup, ok := observed.Groups[state.name]
			if !ok || !runtimeGroup.Enabled {
				if err := controller.runtime.SetGroupBypass(ctx, state.name, true, nonEmptyValue(state.cause, "auto"), nonEmptyValue(state.reason, "restored_group_bypass")); err != nil {
					log.Printf("orchestrator group bypass re-assert failed for %s: %v", state.name, err)
				}
			}
		}
	}
	controller.persistLocked()
}

// observeFlow appends the cumulative counters and recomputes the trailing
// window.  A decrease means the plugin restarted or the target set was
// reinstalled, so the history is reset.
func (state *bypassGroupRuntime) observeFlow(now time.Time, stats []orchestratorapi.BypassFlowStats, window time.Duration) {
	var attempts, success, rst uint64
	for _, item := range stats {
		attempts += item.Attempts
		success += item.SynAck + item.Responses
		rst += item.RST
	}
	if len(state.samples) > 0 {
		last := state.samples[len(state.samples)-1]
		if attempts < last.attempts || success < last.success {
			state.samples = nil
		}
	}
	state.samples = append(state.samples, bypassFlowSample{at: now, attempts: attempts, success: success, rst: rst})
	for len(state.samples) > 1 && now.Sub(state.samples[1].at) >= window {
		state.samples = state.samples[1:]
	}
	first := state.samples[0]
	state.windowAttempts = attempts - first.attempts
	state.windowSuccess = success - first.success
	state.windowRST = rst - first.rst
	if state.windowAttempts > state.windowSuccess+state.windowRST {
		state.windowTimeouts = state.windowAttempts - state.windowSuccess - state.windowRST
	} else {
		state.windowTimeouts = 0
	}
}

func groupHealth(state *bypassGroupRuntime, config orchestratorapi.BypassGroupConfig, probeOK bool) string {
	if state.enabled {
		return "bypass"
	}
	if len(config.FlowTargets) > 0 {
		if state.windowAttempts == 0 {
			// No business traffic in the window: port is assumed open and the
			// group is never bypassed for lack of traffic.
			return "unknown"
		}
		if state.windowSuccess == 0 {
			return "faulty"
		}
		return "healthy"
	}
	if !probeOK {
		return "faulty"
	}
	if state.lastProbeOK {
		return "healthy"
	}
	return "unknown"
}

func bypassGroupReason(interfaceFailed, tcpFailed bool) string {
	parts := make([]string, 0, 2)
	if interfaceFailed {
		parts = append(parts, "interface_down")
	}
	if tcpFailed {
		parts = append(parts, "tcp_unreachable")
	}
	if len(parts) == 0 {
		return "probe_ok"
	}
	return strings.Join(parts, "+")
}

func describeFlowTargets(targets []orchestratorapi.BypassFlowTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, fmt.Sprintf("%s:%d/%s", target.DstIP, target.DstPort, target.Protocol))
	}
	return strings.Join(parts, ",")
}

func flowStatsForGroup(stats []orchestratorapi.BypassFlowStats, group string) []orchestratorapi.BypassFlowStats {
	result := make([]orchestratorapi.BypassFlowStats, 0, len(stats))
	for _, item := range stats {
		if item.Group == group {
			result = append(result, item)
		}
	}
	return result
}

func isBindError(err error) bool {
	if err == nil {
		return false
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		if operation.Op == "dial" && operation.Source != nil {
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cannot assign requested address") ||
		strings.Contains(message, "invalid argument")
}

func (controller *bypassController) collectProbes(ctx context.Context, config orchestratorapi.BypassConfig) []orchestratorapi.BypassProbe {
	now := controller.now()
	interfaces := append([]string(nil), config.Interfaces...)
	if len(interfaces) == 0 && controller.repository != nil {
		if topology, _, err := controller.repository.Snapshot(ctx); err == nil {
			interfaces = topologyProbeInterfaces(topology.View())
		}
	}
	probes := make([]orchestratorapi.BypassProbe, 0, len(interfaces)+len(config.TCPTargets)+len(config.ICMPTargets))
	for _, name := range interfaces {
		probes = append(probes, probeInterface(controller.sysfsRoot, name, now))
	}
	timeout := time.Duration(config.TimeoutMilliseconds) * time.Millisecond
	for _, target := range config.TCPTargets {
		probe := orchestratorapi.BypassProbe{Name: "tcp:" + target, Kind: "tcp", Target: target, CheckedAt: now}
		if err := controller.probeTCP(ctx, target, timeout); err != nil {
			probe.Detail = sanitizeDetail(err.Error())
		} else {
			probe.OK = true
			probe.Detail = "connected"
		}
		probes = append(probes, probe)
	}
	for _, target := range config.ICMPTargets {
		probe := orchestratorapi.BypassProbe{Name: "icmp:" + target, Kind: "icmp", Target: target, CheckedAt: now}
		if err := controller.probeICMP(ctx, target, timeout); err != nil {
			probe.Detail = sanitizeDetail(err.Error())
		} else {
			probe.OK = true
			probe.Detail = "reply"
		}
		probes = append(probes, probe)
	}
	return probes
}

func topologyProbeInterfaces(view orchdomain.TopologyView) []string {
	result := []string{}
	for _, item := range view.Interfaces {
		if item.Bond != nil {
			continue
		}
		result = append(result, item.Port)
	}
	for _, group := range view.Groups {
		for _, port := range group.Ports {
			if port.Bond != nil {
				for _, member := range port.Bond.Members {
					result = append(result, member)
				}
				continue
			}
			result = append(result, port.Interface)
		}
	}
	names := uniqueList(result)
	sort.Strings(names)
	return names
}

// topologyGroupInterfaces maps a group name to its directed port interfaces.
func topologyGroupInterfaces(view orchdomain.TopologyView) map[string][]string {
	result := map[string][]string{}
	for _, group := range view.Groups {
		names := []string{}
		for _, port := range group.Ports {
			if port.Bond != nil {
				names = append(names, port.Bond.Members...)
				continue
			}
			names = append(names, port.Interface)
		}
		result[group.Name] = uniqueList(names)
	}
	return result
}

func probeInterface(root, name string, now time.Time) orchestratorapi.BypassProbe {
	probe := orchestratorapi.BypassProbe{Name: "interface:" + name, Kind: "interface", Target: name, CheckedAt: now}
	if strings.TrimSpace(name) == "" {
		probe.Detail = "empty_interface"
		return probe
	}
	base := filepath.Join(root, "class", "net", name)
	operstateBytes, err := os.ReadFile(filepath.Join(base, "operstate"))
	if err != nil {
		probe.Detail = "operstate_unreadable"
		return probe
	}
	operstate := strings.TrimSpace(string(operstateBytes))
	carrier := ""
	if carrierBytes, err := os.ReadFile(filepath.Join(base, "carrier")); err == nil {
		carrier = strings.TrimSpace(string(carrierBytes))
	}
	healthy := operstate == "up" || operstate == "unknown"
	if carrier != "" && carrier != "1" {
		healthy = false
	}
	if healthy {
		probe.OK = true
		probe.Detail = "operstate=" + operstate
		if carrier != "" {
			probe.Detail += " carrier=" + carrier
		}
		return probe
	}
	probe.Detail = "operstate=" + nonEmptyValue(operstate, "unknown")
	if carrier != "" {
		probe.Detail += " carrier=" + carrier
	}
	return probe
}

func tcpProbe(_ context.Context, target string, timeout time.Duration) error {
	connection, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return err
	}
	return connection.Close()
}

// tcpSourceProbe is the optional active TCP probe with an explicit local
// source.  It only works when the appliance has that source address and a
// route to the target; data-plane group ports are not host interfaces, so this
// probe is optional by design.
func tcpSourceProbe(ctx context.Context, target, source string, timeout time.Duration) error {
	dialer := net.Dialer{Timeout: timeout}
	if strings.TrimSpace(source) != "" {
		address, err := netip.ParseAddr(strings.TrimSpace(source))
		if err != nil {
			return fmt.Errorf("invalid source address %q: %w", source, err)
		}
		dialer.LocalAddr = &net.TCPAddr{IP: net.IP(address.AsSlice())}
	}
	connection, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return connection.Close()
}

func icmpProbe(ctx context.Context, target string, timeout time.Duration) error {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "ping", "-n", "-c", "1", "-W", strconv.Itoa(seconds), target).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func sanitizeDetail(detail string) string {
	detail = strings.ReplaceAll(strings.TrimSpace(detail), " ", "_")
	detail = strings.ReplaceAll(detail, ",", ";")
	if len(detail) > 96 {
		detail = detail[:96]
	}
	return detail
}

func limitReason(reason string) string {
	reason = strings.ReplaceAll(strings.TrimSpace(reason), " ", "_")
	if len(reason) > 140 {
		reason = reason[:140]
	}
	return reason
}

func nonEmptyValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
