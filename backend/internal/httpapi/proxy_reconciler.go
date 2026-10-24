package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/proxy"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

const (
	defaultProxyReconcileInterval = 10 * time.Second
	// proxyFailurePagePort is the packaged nginx listener that serves the
	// fixed failure page through the nftables redirect.
	proxyFailurePagePort = 8081
	// Two consecutive all-dead observatory samples are required before the
	// failure page is published. An inactive Xray process enters failure mode
	// immediately because there is no ambiguity about liveness.
	proxyFailureRequiredDownSamples = 2
	proxyFailureHelperTimeout       = 15 * time.Second
)

// ProxyFailureStatus is the externally visible all-dead / failure-page state.
// Timestamps are UTC and nil until the corresponding event happened.
type ProxyFailureStatus struct {
	AllNodesDown       bool       `json:"all_nodes_down"`
	FailurePageActive  bool       `json:"failure_page_active"`
	FailureReason      string     `json:"failure_reason,omitempty"`
	FailureSince       *time.Time `json:"failure_since,omitempty"`
	RecoveredAt        *time.Time `json:"recovered_at,omitempty"`
	LastSampleAt       *time.Time `json:"last_health_sample_at,omitempty"`
	ConsecutiveAllDown int        `json:"consecutive_all_down_samples"`
	DeadNodeIDs        []string   `json:"dead_node_ids,omitempty"`
	FailureError       string     `json:"failure_error,omitempty"`
}

// proxyFailurePageConfig is the persisted state consumed by the systemd fast
// path (/usr/lib/ly-route/proxy-failure) and passed to the actuator.
type proxyFailurePageConfig struct {
	Version            int       `json:"version"`
	Enabled            bool      `json:"enabled"`
	Active             bool      `json:"active"`
	EgressID           string    `json:"egress_id,omitempty"`
	IngressInterface   string    `json:"ingress_interface"`
	PagePort           int       `json:"page_port"`
	BypassDestinations []string  `json:"bypass_destinations"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type proxyFailureActuator interface {
	Configure(context.Context, proxyFailurePageConfig) error
	Activate(context.Context, proxyFailurePageConfig) error
	Deactivate(context.Context) error
	Disable(context.Context) error
}

// proxyFailureHelper persists the page configuration and drives the packaged
// nftables helper. The state file is written before the helper runs so a crash
// between the two steps still leaves the systemd stop path fail closed.
type proxyFailureHelper struct {
	path      string
	statePath string
	now       func() time.Time
}

func newProxyFailureHelper(path, statePath string, now func() time.Time) *proxyFailureHelper {
	if strings.TrimSpace(path) == "" {
		path = "/usr/lib/ly-route/proxy-failure"
	}
	if strings.TrimSpace(statePath) == "" {
		statePath = "/var/lib/ly-route/proxy-failure.json"
	}
	if now == nil {
		now = time.Now
	}
	return &proxyFailureHelper{path: strings.TrimSpace(path), statePath: strings.TrimSpace(statePath), now: now}
}

func (helper *proxyFailureHelper) Configure(ctx context.Context, config proxyFailurePageConfig) error {
	config.Active = false
	return helper.writeState(config)
}

func (helper *proxyFailureHelper) Activate(ctx context.Context, config proxyFailurePageConfig) error {
	config.Active = true
	if err := helper.writeState(config); err != nil {
		return err
	}
	return helper.run(ctx, "enter")
}

func (helper *proxyFailureHelper) Deactivate(ctx context.Context) error {
	if err := helper.run(ctx, "leave"); err != nil {
		return err
	}
	return helper.markInactive()
}

func (helper *proxyFailureHelper) Disable(ctx context.Context) error {
	if err := helper.run(ctx, "leave"); err != nil {
		return err
	}
	if err := os.Remove(helper.statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove proxy failure state: %w", err)
	}
	return nil
}

func (helper *proxyFailureHelper) run(ctx context.Context, action string) error {
	if helper.path == "" {
		return fmt.Errorf("proxy failure helper path is empty")
	}
	runCtx, cancel := context.WithTimeout(ctx, proxyFailureHelperTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, helper.path, action)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("proxy failure helper %s failed: %w: %s", action, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (helper *proxyFailureHelper) writeState(config proxyFailurePageConfig) error {
	config.Version = 1
	config.UpdatedAt = helper.now().UTC()
	payload, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	directory := filepath.Dir(helper.statePath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create proxy failure state directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".proxy-failure-*.json")
	if err != nil {
		return fmt.Errorf("create proxy failure state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return fmt.Errorf("write proxy failure state: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("chmod proxy failure state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close proxy failure state: %w", err)
	}
	if err := os.Rename(tempName, helper.statePath); err != nil {
		return fmt.Errorf("install proxy failure state: %w", err)
	}
	return nil
}

func (helper *proxyFailureHelper) markInactive() error {
	payload, err := os.ReadFile(helper.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var config proxyFailurePageConfig
	if err := json.Unmarshal(payload, &config); err != nil {
		return fmt.Errorf("decode proxy failure state: %w", err)
	}
	config.Active = false
	return helper.writeState(config)
}

type proxyFailureTracker struct {
	mu           sync.Mutex
	status       ProxyFailureStatus
	downStreak   int
	baselineHash string
	captureHash  string
	// cleared records that a healthy sample already removed any leftover
	// failure generation (for example one installed by the systemd fast path
	// while the control API was down).
	cleared bool
}

func newProxyFailureTracker() *proxyFailureTracker {
	return &proxyFailureTracker{}
}

func (server *Server) proxyFailureSnapshot() ProxyFailureStatus {
	if server == nil || server.proxyFailure == nil {
		return ProxyFailureStatus{}
	}
	return server.proxyFailure.snapshot()
}

func (tracker *proxyFailureTracker) snapshot() ProxyFailureStatus {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	status := tracker.status
	status.DeadNodeIDs = append([]string(nil), tracker.status.DeadNodeIDs...)
	return status
}

func (tracker *proxyFailureTracker) needsClear() bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.status.FailurePageActive || tracker.baselineHash != "" || tracker.captureHash != ""
}

func (tracker *proxyFailureTracker) reset() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.status = ProxyFailureStatus{}
	tracker.downStreak = 0
	tracker.baselineHash = ""
	tracker.captureHash = ""
	tracker.cleared = false
}

func (tracker *proxyFailureTracker) baselineUnchanged(hash string) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.baselineHash == hash
}

func (tracker *proxyFailureTracker) setBaselineHash(hash string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.baselineHash = hash
}

func (tracker *proxyFailureTracker) failureCleared() bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.cleared
}

func (tracker *proxyFailureTracker) markCleared() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.cleared = true
}

func (tracker *proxyFailureTracker) captureUnchanged(hash string) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.captureHash == hash
}

func (tracker *proxyFailureTracker) setCaptureHash(hash string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.captureHash = hash
}

// observe records one successful liveness sample. It returns whether the page
// must be activated or removed on this tick.
func (tracker *proxyFailureTracker) observe(now time.Time, sample proxyLivenessSample) (activate, recoverPage, wasActive bool) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	wasActive = tracker.status.FailurePageActive
	tracker.status.FailureError = ""
	tracker.status.LastSampleAt = &now
	if sample.allDead {
		tracker.downStreak++
		tracker.status.AllNodesDown = true
		tracker.status.ConsecutiveAllDown = tracker.downStreak
		tracker.status.DeadNodeIDs = append([]string(nil), sample.deadNodeIDs...)
		tracker.status.FailureReason = sample.reason
		if !wasActive && (sample.failFast || tracker.downStreak >= proxyFailureRequiredDownSamples) {
			activate = true
		}
		return
	}
	tracker.downStreak = 0
	tracker.status.AllNodesDown = false
	tracker.status.ConsecutiveAllDown = 0
	tracker.status.DeadNodeIDs = nil
	tracker.status.FailureReason = ""
	if wasActive {
		recoverPage = true
	}
	return
}

func (tracker *proxyFailureTracker) recordError(now time.Time, err error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if err == nil {
		return
	}
	tracker.status.FailureError = serviceRuntime.Redact(err.Error())
}

func (tracker *proxyFailureTracker) markActivated(now time.Time, reason string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.cleared = false
	tracker.status.FailurePageActive = true
	tracker.status.AllNodesDown = true
	tracker.status.FailureReason = reason
	if tracker.status.FailureSince == nil {
		since := now
		tracker.status.FailureSince = &since
	}
	tracker.status.FailureError = ""
}

func (tracker *proxyFailureTracker) markRecovered(now time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	recovered := now
	tracker.status.FailurePageActive = false
	tracker.status.AllNodesDown = false
	tracker.status.ConsecutiveAllDown = 0
	tracker.status.DeadNodeIDs = nil
	tracker.status.FailureReason = ""
	tracker.status.FailureSince = nil
	tracker.status.RecoveredAt = &recovered
	tracker.status.FailureError = ""
	tracker.downStreak = 0
	tracker.captureHash = ""
	tracker.cleared = true
}

type proxyLivenessSample struct {
	allDead     bool
	failFast    bool
	deadNodeIDs []string
	reason      string
	states      []serviceRuntime.XrayObservatoryState
}

// StartProxyReconciler keeps adaptive lanes current and, more importantly,
// drives the fixed failure page when every proxy node is dead. It deliberately
// does not require a full gateway apply to toggle the page.
func (server *Server) StartProxyReconciler(ctx context.Context, interval time.Duration) {
	if server == nil || server.profile.ID() != product.Gateway().ID() || server.services == nil || server.services.Controller == nil ||
		server.proxyFailure == nil || server.proxyFailurePage == nil {
		return
	}
	observer, ok := server.services.Controller.(serviceRuntime.XrayObservatoryStateController)
	if !ok {
		return
	}
	if interval <= 0 {
		interval = defaultProxyReconcileInterval
	}
	go server.runProxyReconciler(ctx, observer, interval)
}

func (server *Server) runProxyReconciler(ctx context.Context, observer serviceRuntime.XrayObservatoryStateController, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := server.reconcileProxyHealth(ctx, observer); err != nil && ctx.Err() == nil {
			log.Printf("proxy health reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (server *Server) reconcileProxyHealth(ctx context.Context, observer serviceRuntime.XrayObservatoryStateController) error {
	server.runtimeApplyMu.Lock()
	defer server.runtimeApplyMu.Unlock()

	now := server.now().UTC()
	egress, configured, err := server.runtimeProxyEgress(ctx)
	if err != nil {
		server.proxyFailure.recordError(now, err)
		return err
	}
	if !configured {
		return server.clearProxyFailure(ctx, now)
	}
	compiled, err := proxy.CompileEgress(egress)
	if err != nil {
		server.proxyFailure.recordError(now, err)
		return err
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		server.proxyFailure.recordError(now, err)
		return err
	}
	config := proxyFailurePageConfig{
		Enabled:            true,
		EgressID:           egress.ID,
		IngressInterface:   strings.TrimSpace(compiled.ServiceNetwork.IngressHostInterface),
		PagePort:           proxyFailurePagePort,
		BypassDestinations: proxyFailureBypassDestinations(compiled.ServiceNetwork, compiled.NftablesCapture, assignments),
	}
	if config.IngressInterface == "" {
		err := fmt.Errorf("proxy failure page requires a proxy ingress interface")
		server.proxyFailure.recordError(now, err)
		return err
	}

	// Keep the systemd fast path supplied with a current baseline while the
	// page is inactive. Never overwrite an active failure state.
	if !server.proxyFailure.snapshot().FailurePageActive {
		if err := server.ensureProxyFailureBaseline(ctx, config); err != nil {
			server.proxyFailure.recordError(now, err)
			return err
		}
	}

	sample, err := server.sampleProxyLiveness(ctx, observer, egress)
	if err != nil {
		// Hold the last mode on an observatory or health read error. A single
		// successful sample is enough to recover, but a missing sample is not
		// evidence that nodes are dead.
		server.proxyFailure.recordError(now, err)
		return fmt.Errorf("sample proxy liveness: %w", err)
	}
	activate, recoverPage, wasActive := server.proxyFailure.observe(now, sample)
	if activate {
		if err := server.proxyFailurePage.Activate(ctx, config); err != nil {
			server.proxyFailure.recordError(now, err)
			return fmt.Errorf("activate proxy failure page: %w", err)
		}
		server.proxyFailure.markActivated(now, sample.reason)
		log.Printf("proxy failure page active: %s", sample.reason)
		return nil
	}
	if recoverPage {
		if err := server.restoreProxyCapture(ctx, egress, assignments); err != nil {
			// Keep the page (fail closed) until the normal capture generation
			// can be restored in the same tick.
			server.proxyFailure.recordError(now, err)
			return fmt.Errorf("restore proxy capture: %w", err)
		}
		if err := server.proxyFailurePage.Deactivate(ctx); err != nil {
			server.proxyFailure.recordError(now, err)
			return fmt.Errorf("deactivate proxy failure page: %w", err)
		}
		server.proxyFailure.markRecovered(now)
		log.Printf("proxy failure page cleared: proxy nodes recovered")
		return nil
	}
	if wasActive || sample.allDead {
		// While all nodes are down there is no healthy lane plan to install;
		// the second sample activates the failure page instead.
		return nil
	}

	// A previous control-plane instance (or the systemd fast path) may have
	// left a failure generation behind. Once the proxy is observed healthy,
	// remove it exactly once. This also clears a page left over from a failed
	// crash path, so a healthy proxy can never be stuck behind a stale guard.
	if !server.proxyFailure.failureCleared() {
		if err := server.proxyFailurePage.Deactivate(ctx); err != nil {
			server.proxyFailure.recordError(now, err)
			return fmt.Errorf("clear stale proxy failure generation: %w", err)
		}
		server.proxyFailure.markCleared()
		log.Printf("proxy failure generation cleared: proxy is healthy")
	}

	// Preserve the previous adaptive behaviour: re-balance the capture lanes
	// when the RTT-weighted plan changes. Fixed and fastest captures do not
	// change until the next apply.
	if !server.isAdaptiveProxyEgress(ctx, egress) {
		return nil
	}
	capture, err := server.adaptiveCaptureForReconcile(ctx, egress, sample.states)
	if err != nil {
		return err
	}
	hash, err := server.applyProxyCapture(ctx, capture, assignments)
	if err != nil {
		return err
	}
	server.proxyFailure.setCaptureHash(hash)
	return nil
}

func (server *Server) clearProxyFailure(ctx context.Context, now time.Time) error {
	if !server.proxyFailure.needsClear() {
		return nil
	}
	// No proxy egress is selected: direct forwarding is the product behaviour,
	// so the page must not keep blackholing traffic.
	if err := server.proxyFailurePage.Disable(ctx); err != nil {
		server.proxyFailure.recordError(now, err)
		return fmt.Errorf("disable proxy failure page: %w", err)
	}
	server.proxyFailure.reset()
	return nil
}

func (server *Server) ensureProxyFailureBaseline(ctx context.Context, config proxyFailurePageConfig) error {
	hash := proxyFailureConfigHash(config)
	if server.proxyFailure.baselineUnchanged(hash) {
		return nil
	}
	if err := server.proxyFailurePage.Configure(ctx, config); err != nil {
		return fmt.Errorf("write proxy failure baseline: %w", err)
	}
	server.proxyFailure.setBaselineHash(hash)
	return nil
}

func (server *Server) sampleProxyLiveness(ctx context.Context, observer serviceRuntime.XrayObservatoryStateController, egress proxy.Egress) (proxyLivenessSample, error) {
	health, err := server.services.HealthCheck(ctx, serviceRuntime.Xray)
	if err != nil {
		return proxyLivenessSample{}, fmt.Errorf("read xray health: %w", err)
	}
	if len(health) == 0 || !health[0].Available {
		reason := "xray_inactive"
		if len(health) > 0 && strings.TrimSpace(health[0].Reason) != "" {
			reason = "xray_inactive: " + serviceRuntime.Redact(health[0].Reason)
		}
		return proxyLivenessSample{allDead: true, failFast: true, reason: reason}, nil
	}
	states, err := observer.XrayObservatoryStates(ctx)
	if err != nil {
		return proxyLivenessSample{}, fmt.Errorf("read xray observatory: %w", err)
	}
	if len(states) == 0 {
		return proxyLivenessSample{}, fmt.Errorf("xray observatory has no samples")
	}
	expected, err := server.proxyLivenessTags(ctx, egress)
	if err != nil {
		return proxyLivenessSample{}, err
	}
	relevant := states
	if len(expected) > 0 {
		byTag := make(map[string]serviceRuntime.XrayObservatoryState, len(states))
		for _, state := range states {
			byTag[state.OutboundTag] = state
		}
		relevant = make([]serviceRuntime.XrayObservatoryState, 0, len(expected))
		for _, tag := range expected {
			state, found := byTag[tag]
			if !found {
				return proxyLivenessSample{}, fmt.Errorf("xray observatory has no sample for %s", tag)
			}
			relevant = append(relevant, state)
		}
	}
	dead := make([]string, 0, len(relevant))
	for _, state := range relevant {
		if !state.Alive {
			dead = append(dead, state.OutboundTag)
		}
	}
	sort.Strings(dead)
	allDead := len(relevant) > 0 && len(dead) == len(relevant)
	sample := proxyLivenessSample{allDead: allDead, deadNodeIDs: dead, states: states}
	if allDead {
		sample.reason = "all_proxy_nodes_unreachable"
	}
	return sample, nil
}

// proxyLivenessTags returns the observatory outbound tags that must be present
// for a valid liveness sample. Fastest selection returns nil because the
// applied config contains only the currently selected node; the observatory
// state itself is then the authoritative set.
func (server *Server) proxyLivenessTags(ctx context.Context, egress proxy.Egress) ([]string, error) {
	if id := strings.TrimSpace(egress.NodeID); id != "" {
		return []string{"node-" + id}, nil
	}
	subscriptionID := strings.TrimSpace(egress.SubscriptionID)
	if subscriptionID == "" {
		return nil, nil
	}
	subscription, found, err := server.proxySubscription(ctx, subscriptionID)
	if err != nil || !found {
		return nil, err
	}
	ids := append([]string(nil), subscription.NodeRefs...)
	if len(ids) == 0 {
		nodes, err := server.desiredItems(ctx, "proxy_node")
		if err != nil {
			return nil, err
		}
		for _, item := range nodes {
			if enabled, exists := item["enabled"].(bool); exists && !enabled {
				continue
			}
			if id := strings.TrimSpace(stringField(item, "id")); id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	mode := subscription.Selection
	if mode == "" {
		mode = proxy.SelectionFixed
	}
	switch mode {
	case proxy.SelectionAdaptive:
		prefix := "subscription-" + subscription.ID + "-node-"
		tags := make([]string, 0, len(ids))
		for _, id := range ids {
			tags = append(tags, prefix+id)
		}
		return tags, nil
	case proxy.SelectionFixed:
		if len(ids) == 0 {
			return nil, nil
		}
		return []string{"node-" + ids[0]}, nil
	default:
		return nil, nil
	}
}

func (server *Server) isAdaptiveProxyEgress(ctx context.Context, egress proxy.Egress) bool {
	subscriptionID := strings.TrimSpace(egress.SubscriptionID)
	if subscriptionID == "" {
		return false
	}
	subscription, found, err := server.proxySubscription(ctx, subscriptionID)
	if err != nil || !found {
		return false
	}
	return subscription.Selection == proxy.SelectionAdaptive || subscription.Strategy == proxy.AdaptiveSubscriptionStrategy
}

func (server *Server) adaptiveCaptureForReconcile(ctx context.Context, egress proxy.Egress, states []serviceRuntime.XrayObservatoryState) (proxy.NftablesCapturePlan, error) {
	subscription, found, err := server.proxySubscription(ctx, strings.TrimSpace(egress.SubscriptionID))
	if err != nil {
		return proxy.NftablesCapturePlan{}, err
	}
	if !found {
		return proxy.NftablesCapturePlan{}, fmt.Errorf("adaptive subscription %q is missing", egress.SubscriptionID)
	}
	compiled, err := proxy.CompileEgress(egress)
	if err != nil {
		return proxy.NftablesCapturePlan{}, err
	}
	lanes, err := adaptiveLanesFromObservations(subscription, states, compiled.XrayRuntime.ListenPort, server.now().UTC())
	if err != nil {
		return proxy.NftablesCapturePlan{}, err
	}
	return proxy.BuildAdaptiveCapturePlan(compiled.NftablesCapture, lanes)
}

func (server *Server) restoreProxyCapture(ctx context.Context, egress proxy.Egress, assignments []vpp.AddressAssignment) error {
	compiled, err := proxy.CompileEgress(egress)
	if err != nil {
		return err
	}
	if warning := server.compileProxySubscription(ctx, egress, &compiled); warning != "" {
		return fmt.Errorf("proxy runtime is not compilable: %s", warning)
	}
	if strings.TrimSpace(compiled.NftablesCapture.Table) == "" {
		return fmt.Errorf("proxy capture plan is empty")
	}
	if _, err := server.applyProxyCapture(ctx, compiled.NftablesCapture, assignments); err != nil {
		return err
	}
	return nil
}

func (server *Server) applyProxyCapture(ctx context.Context, capture proxy.NftablesCapturePlan, assignments []vpp.AddressAssignment) (string, error) {
	artifacts, err := serviceRuntime.RenderGatewayNftablesCapture(capture, dnsLCPGuardPlan(assignments))
	if err != nil {
		return "", err
	}
	if len(artifacts) != 1 {
		return "", fmt.Errorf("proxy capture rendered %d nftables artifacts", len(artifacts))
	}
	if err := server.services.Controller.ReloadOrRestart(ctx, serviceRuntime.Nftables, artifacts); err != nil {
		return "", err
	}
	return artifacts[0].ContentHash, nil
}

// proxySubscription returns the selected subscription regardless of selection
// mode. The adaptive reconciler used to reject fixed/fastest; the failure page
// state machine needs all three.
func (server *Server) proxySubscription(ctx context.Context, subscriptionID string) (proxy.Subscription, bool, error) {
	items, err := server.desiredItems(ctx, "proxy_subscription")
	if err != nil {
		return proxy.Subscription{}, false, err
	}
	for _, item := range items {
		enabled, hasEnabled := item["enabled"]
		if stringField(item, "id") != strings.TrimSpace(subscriptionID) || (hasEnabled && !truthy(enabled)) {
			continue
		}
		return proxy.Subscription{
			ID: stringField(item, "id"), URL: "runtime-observer", Enabled: true,
			NodeRefs:  stringSliceField(item, "node_refs"),
			Selection: proxy.SelectionMode(strings.TrimSpace(stringField(item, "selection"))),
			Strategy:  strings.TrimSpace(stringField(item, "strategy")), TopN: intField(item, "top_n"),
		}, true, nil
	}
	return proxy.Subscription{}, false, nil
}

func adaptiveLanesFromObservations(subscription proxy.Subscription, states []serviceRuntime.XrayObservatoryState, basePort int, observedAt time.Time) ([]proxy.AdaptiveNodeLane, error) {
	ids := append([]string(nil), subscription.NodeRefs...)
	sort.Strings(ids)
	nodes := make([]proxy.Node, 0, len(ids))
	probes := make([]proxy.NodeProbe, 0, len(ids))
	prefix := "subscription-" + subscription.ID + "-node-"
	byTag := make(map[string]serviceRuntime.XrayObservatoryState, len(states))
	for _, state := range states {
		byTag[state.OutboundTag] = state
	}
	for _, id := range ids {
		nodes = append(nodes, proxy.Node{ID: id})
		state, found := byTag[prefix+id]
		if found {
			probes = append(probes, proxy.NodeProbe{NodeID: id, Reachable: state.Alive, RTT: time.Duration(state.DelayMilliseconds) * time.Millisecond, ObservedAt: observedAt})
		}
	}
	plan, err := proxy.BuildRTTWeightedTopNPlan(subscription, nodes, probes)
	if err != nil {
		return nil, err
	}
	weights := make(map[string]int, len(plan.Nodes))
	for _, node := range plan.Nodes {
		weights[node.ID] = node.Weight
	}
	portByID := make(map[string]int, len(ids))
	for index, id := range ids {
		portByID[id] = basePort + index
	}
	lanes := make([]proxy.AdaptiveNodeLane, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Weight > 0 {
			lanes = append(lanes, proxy.AdaptiveNodeLane{NodeID: node.ID, ListenerPort: portByID[node.ID], Weight: node.Weight})
		}
	}
	if len(lanes) == 0 {
		return nil, fmt.Errorf("adaptive proxy has no healthy lanes")
	}
	return lanes, nil
}

func proxyFailureConfigHash(config proxyFailurePageConfig) string {
	config.Active = false
	config.UpdatedAt = time.Time{}
	payload, err := json.Marshal(config)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func proxyFailureBypassDestinations(network proxy.ProxyServiceNetwork, capture proxy.NftablesCapturePlan, assignments []vpp.AddressAssignment) []string {
	candidates := append([]string(nil), capture.BypassDestinations...)
	candidates = append(candidates, network.IngressCIDR, network.EgressCIDR)
	candidates = append(candidates, proxyLANRoutes(assignments)...)
	seen := make(map[string]struct{}, len(candidates))
	result := make([]string, 0, len(candidates))
	for _, raw := range candidates {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		canonical := prefix.Masked().String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	return result
}

func proxyFailureStatusFields(status ProxyFailureStatus) map[string]any {
	dead := status.DeadNodeIDs
	if dead == nil {
		dead = []string{}
	}
	return map[string]any{
		"all_nodes_down":               status.AllNodesDown,
		"failure_page_active":          status.FailurePageActive,
		"failure_reason":               status.FailureReason,
		"failure_since":                status.FailureSince,
		"recovered_at":                 status.RecoveredAt,
		"last_health_sample_at":        status.LastSampleAt,
		"consecutive_all_down_samples": status.ConsecutiveAllDown,
		"dead_node_ids":                dead,
		"failure_error":                status.FailureError,
	}
}
