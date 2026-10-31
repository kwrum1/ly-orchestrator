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
	bypassStateVersion      = 1
)

type bypassPersistedState struct {
	Version   int                          `json:"version"`
	Config    orchestratorapi.BypassConfig `json:"config"`
	Enabled   bool                         `json:"enabled"`
	Forced    bool                         `json:"forced"`
	Cause     string                       `json:"cause,omitempty"`
	Reason    string                       `json:"reason,omitempty"`
	Since     time.Time                    `json:"since,omitempty"`
	UpdatedAt time.Time                    `json:"updated_at"`
}

// bypassController is the fail-open supervisor for the transparent
// orchestrator.  It probes configured interfaces and endpoints, and toggles the
// plugin bypass flag when the failure/recovery thresholds are crossed.  The
// plugin keeps the configured policy; bypass only suppresses its evaluation.
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

	lastProbeAt time.Time
	lastProbeOK bool
	probes      []orchestratorapi.BypassProbe

	probeTCP  func(context.Context, string, time.Duration) error
	probeICMP func(context.Context, string, time.Duration) error

	started bool
}

func newBypassController(runtime *productionOrchestratorRuntime, repository *orchdomain.Repository, statePath, sysfsRoot, tcpTargets, icmpTargets, interfaces string) *bypassController {
	controller := &bypassController{
		runtime:    runtime,
		repository: repository,
		statePath:  strings.TrimSpace(statePath),
		sysfsRoot:  strings.TrimSpace(sysfsRoot),
		now:        func() time.Time { return time.Now().UTC() },
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
	return config, nil
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
	controller.mu.Unlock()

	if enabled {
		if err := controller.runtime.SetBypass(ctx, true, nonEmptyValue(cause, "auto"), nonEmptyValue(reason, "restored_bypass_state")); err != nil {
			log.Printf("orchestrator fault bypass restore failed: %v", err)
		}
	}
	if forced {
		log.Printf("orchestrator fault bypass restored forced state: %s", reason)
	}
	go controller.run(ctx)
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
	if state.Cause == "" {
		state.Cause = runtime.Cause
	}
	if state.Reason == "" {
		state.Reason = runtime.Reason
	}
	if state.Since.IsZero() {
		state.Since = runtime.Since
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
	if !normalized.Enabled && controller.enabled && !controller.forced {
		if err := controller.runtime.SetBypass(ctx, false, "config", "detector_disabled"); err != nil {
			return orchestratorapi.BypassState{}, err
		}
		controller.enabled = false
		controller.cause = "config"
		controller.reason = "detector_disabled"
		controller.since = time.Time{}
	}
	controller.persistLocked()
	return controller.statusLocked(), nil
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
