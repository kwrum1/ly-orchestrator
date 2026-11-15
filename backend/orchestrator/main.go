package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/httpapi"
	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/dataplane"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func Run() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(Version())
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--dump-config" {
		dumpEffectiveConfig()
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// buildVersion is stamped by release builds. It is authoritative for a
// hot-deployed control binary even when the on-disk artifact manifest belongs
// to the previous package.
var buildVersion string

// Version is the release identity reported by /health and the installer
// preflight. Prefer an explicit environment value, then the release stamp
// embedded in the running control binary, then the immutable package
// provenance on disk.
func Version() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_VERSION")); value != "" && !strings.EqualFold(value, "dev") {
		return value
	}
	if value := strings.TrimSpace(buildVersion); value != "" && !strings.EqualFold(value, "dev") {
		return value
	}
	path := strings.TrimSpace(os.Getenv("LY_ROUTE_ARTIFACT_MANIFEST"))
	if path == "" {
		path = "/usr/share/ly-route/artifact-manifest.json"
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var manifest struct {
			SourceCommit string `json:"source_commit"`
			Commit       string `json:"commit"`
			Version      string `json:"version"`
		}
		if json.Unmarshal(data, &manifest) == nil {
			for _, candidate := range []string{manifest.Version, manifest.SourceCommit, manifest.Commit} {
				if value := strings.TrimSpace(candidate); value != "" {
					return value
				}
			}
		}
	}
	return "dev"
}

func run() error {
	profile, err := loadProductProfile(product.Orchestrator().ID().String(), os.Getenv("LY_ROUTE_PRODUCT_PROFILE"))
	if err != nil {
		return fmt.Errorf("load product profile: %w", err)
	}
	host := env("LY_ROUTE_API_HOST", "127.0.0.1")
	port := env("LY_ROUTE_API_PORT", "8080")
	addr := fmt.Sprintf("%s:%s", host, port)
	paths := defaultStatePaths(profile, pathExists)
	databasePath := env("LY_ROUTE_DB_PATH", paths.Database)
	configPath := env("LY_ROUTE_CONFIG_PATH", paths.Config)
	store, err := openStore(context.Background(), databasePath, profile.ID())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	selection := product.NewSelection()
	if err := selection.Initialize(profile); err != nil {
		return fmt.Errorf("initialize product selection: %w", err)
	}
	dataplaneController := productionDataplaneController()
	options := []httpapi.Option{
		httpapi.WithVersion(Version()),
		httpapi.WithStore(store),
		httpapi.WithAuthConfig(httpapi.AuthConfig{
			AdminUsername:       env("LY_ROUTE_ADMIN_USERNAME", ""),
			AdminPassword:       env("LY_ROUTE_ADMIN_PASSWORD", ""),
			ReadonlyUsername:    env("LY_ROUTE_READONLY_USERNAME", "readonly"),
			ReadonlyPassword:    env("LY_ROUTE_READONLY_PASSWORD", ""),
			CookieSecure:        envBool("LY_ROUTE_SESSION_COOKIE_SECURE", false),
			ForcePasswordChange: envBool("LY_ROUTE_FORCE_PASSWORD_CHANGE", true),
		}),
	}
	// A slow board (QEMU/TCG, Atom-class appliances) legitimately needs longer
	// than the 8 minute default; timing out produced rollback churn that looked
	// like a dataplane bug.
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_APPLY_TIMEOUT")); raw != "" {
		timeout, parseErr := time.ParseDuration(raw)
		if parseErr != nil || timeout <= 0 {
			log.Printf("ignoring invalid LY_ROUTE_APPLY_TIMEOUT=%q", raw)
		} else {
			options = append(options, httpapi.WithApplyTimeout(timeout))
		}
	}
	var recoveryRepository *orchestrator.Repository
	var recoveryRuntime *productionOrchestratorRuntime
	var bypass *bypassController
	var reconcileInterval time.Duration
	if profile.ID() == product.Orchestrator().ID() {
		repository, repositoryErr := orchestrator.NewRepository(store, orchestrator.RepositoryOptions{Now: time.Now})
		if repositoryErr != nil {
			return fmt.Errorf("initialize orchestrator repository: %w", repositoryErr)
		}
		// The topology may have been persisted with product ids (eth1) by an
		// older build. Resolve them before startup reconciliation so the proof
		// probe and VPP boundary commands see the kernel netdev (ens160).
		resolveLinuxInterface := httpapi.InterfaceLinuxNameResolver(store)
		managementInterface := env("LY_ROUTE_MANAGEMENT_INTERFACE", "eth0")
		if resolved := resolveLinuxInterface(context.Background(), managementInterface); strings.TrimSpace(resolved) != "" {
			managementInterface = resolved
		}
		orchestratorRuntime := &productionOrchestratorRuntime{
			adapter:             vpp.Adapter{Client: vpp.NewProductionVPPCTLClient(env("LY_ROUTE_VPPCTL", "vppctl"))},
			dataplane:           dataplaneController,
			dpdkActive:          map[string]vpp.NativePath{},
			tracker:             orchestrator.NewHealthTracker(orchestrator.DefaultRecoverySuccesses, time.Now),
			probe:               icmpHealthProbe{binary: env("LY_ROUTE_PING", "ping")},
			capabilityReport:    env("LY_ROUTE_VPP_CAPABILITY_PROOF", "/var/lib/ly-route/vpp-native-capabilities.json"),
			managementInterface: managementInterface,
			linuxInterface:      resolveLinuxInterface,
			proofCommand:        env("LY_ROUTE_VPP_PROBE_COMMAND", "/usr/lib/ly-route/runtime-check.sh"),
			journalPath:         env("LY_ROUTE_ORCHESTRATOR_TRANSACTION_JOURNAL", "/var/lib/ly-route/orchestrator/transparent-transaction.json"),
		}
		if err := orchestratorRuntime.loadTransparentTransactionJournal(); err != nil {
			return fmt.Errorf("load orchestrator transaction journal: %w", err)
		}
		if orchestratorRuntime.transparentJournalPending {
			if _, _, snapshotErr := repository.Snapshot(context.Background()); errors.Is(snapshotErr, orchestrator.ErrTopologyNotFound) {
				if err := orchestratorRuntime.DisableTransparent(context.Background(), "orchestrator-startup-empty"); err != nil {
					return fmt.Errorf("recover unpersisted orchestrator dataplane: %w", err)
				}
			}
		}
		if err := reconcileTransparentStartup(context.Background(), repository, orchestratorRuntime); err != nil {
			return fmt.Errorf("reconcile orchestrator dataplane at startup: %w", err)
		}
		if err := orchestratorRuntime.CommitTransparentTransaction(context.Background()); err != nil {
			return fmt.Errorf("commit recovered orchestrator transaction: %w", err)
		}
		reconcileInterval = envDuration("LY_ROUTE_ORCHESTRATOR_RECONCILE_INTERVAL", 2*time.Second)
		bypass = newBypassController(
			orchestratorRuntime,
			repository,
			env("LY_ROUTE_ORCHESTRATOR_BYPASS_STATE", "/var/lib/ly-route/orchestrator/bypass.json"),
			env("LY_ROUTE_SYSFS_ROOT", "/sys"),
			env("LY_ROUTE_ORCHESTRATOR_BYPASS_TCP_TARGETS", ""),
			env("LY_ROUTE_ORCHESTRATOR_BYPASS_ICMP_TARGETS", ""),
			env("LY_ROUTE_ORCHESTRATOR_BYPASS_INTERFACES", ""),
		)
		options = append(options,
			httpapi.WithOrchestratorRepository(repository),
			httpapi.WithOrchestratorTelemetry(newOrchestratorTelemetryRuntime(repository, env("LY_ROUTE_VPPCTL", "vppctl"), time.Now)),
			httpapi.WithOrchestratorReconcileInterval(reconcileInterval),
			httpapi.WithOrchestratorRuntime(orchestratorRuntime),
			httpapi.WithOrchestratorBypass(bypass),
		)
		recoveryRepository = repository
		recoveryRuntime = orchestratorRuntime
	}
	if envBool("LY_ROUTE_ENABLE_SERVICE_RUNTIME", false) {
		options = append(options, httpapi.WithServiceRuntime(serviceRuntime.Runtime{Controller: serviceRuntime.FilesystemController{RootDir: env("LY_ROUTE_SERVICE_ROOT", ""), Runner: serviceRuntime.SystemctlRunner{}, XrayAPIAddress: env("LY_ROUTE_XRAY_API_ADDRESS", serviceRuntime.DefaultXrayRoutingAPIAddress)}}))
	}
	if profile.AllowsService(product.ServiceDnsmasq) {
		options = append(options, httpapi.WithDHCPLeases(serviceRuntime.DnsmasqLeaseCollector{Path: env("LY_ROUTE_DNSMASQ_LEASE_FILE", serviceRuntime.DefaultDnsmasqLeaseFile)}))
	}
	if envBool("LY_ROUTE_ENABLE_VPP_INTERFACE_TELEMETRY", true) {
		options = append(options, httpapi.WithInterfaceTelemetry(vppctlInterfaceTelemetry{binary: env("LY_ROUTE_VPPCTL", "vppctl")}))
	}
	server, err := httpapi.NewServer(selection, options...)
	if err != nil {
		return fmt.Errorf("initialize control API: %w", err)
	}
	if recoveryRuntime != nil {
		recoveryRuntime.SetMirrorProvider(server.OrchestratorMirrorProvider())
	}
	recoveryCtx, stopRecovery := context.WithCancel(context.Background())
	defer stopRecovery()
	if recoveryRepository != nil && recoveryRuntime != nil {
		go runTransparentRecovery(recoveryCtx, recoveryRepository, recoveryRuntime, reconcileInterval)
	}
	if bypass != nil {
		bypass.Start(recoveryCtx)
	}
	server.StartDataplaneWatcher(context.Background(), envDuration("LY_ROUTE_DATAPLANE_WATCH_INTERVAL", 3*time.Second))
	log.Printf("ly-route %s control API listening on %s database=%s config=%s", profile.ID(), addr, databasePath, configPath)
	if err := httpapi.ListenAndServe(addr, server); err != nil {
		return fmt.Errorf("serve control API: %w", err)
	}
	return nil
}

func productionGatewayTransaction() apply.GatewayTransactionRunner {
	return productionGatewayTransactionWithController(productionDataplaneController())
}

func productionGatewayTransactionWithController(controller *dataplane.Transaction) apply.GatewayTransactionRunner {
	vppctl := env("LY_ROUTE_VPPCTL", "vppctl")
	return apply.NewProductionGatewayTransactionWithDataplane(vpp.Adapter{Client: vpp.NewProductionVPPCTLClient(vppctl)}, controller, time.Now)
}

func productionDataplaneController() *dataplane.Transaction {
	host := dataplane.NewLinuxHost()
	host.SysfsRoot = env("LY_ROUTE_SYSFS_ROOT", "/sys")
	host.StartupConfig = env("LY_ROUTE_VPP_STARTUP_CONFIG", "/etc/vpp/startup.conf")
	host.StateDir = env("LY_ROUTE_DATAPLANE_STATE_DIR", "/var/lib/ly-route/dataplane")
	host.VPPCTL = env("LY_ROUTE_VPPCTL", "vppctl")
	return &dataplane.Transaction{Host: host, Now: time.Now}
}

type productionOrchestratorRuntime struct {
	adapter                   vpp.Adapter
	dataplane                 apply.DataplaneController
	transparentMu             sync.Mutex
	transparentBonds          []vpp.BondState
	transparentRequest        string
	transparentGeneration     string
	transparentAppliedAt      time.Time
	journalPath               string
	transparentJournalPending bool
	// Data-plane readback state used by health/status evidence and by the
	// periodic reconcile.  It records what the plugin actually committed and
	// what the persisted configuration asked for, so a mismatch can be named
	// (config hash vs VPP generation/default) instead of only logged.
	transparentObservedGeneration string
	transparentObservedDefault    string
	transparentConfiguredDefault  string
	reconcileRepairs              int
	reconcileFailures             int
	lastDriftReason               string
	lastReconcileAt               time.Time
	driftDetected                 bool
	bypassMu                      sync.RWMutex
	bypassActive                  bool
	bypassForced                  bool
	bypassCause                   string
	bypassReason                  string
	bypassSince                   time.Time
	bypassPackets                 uint64
	bypassGroups                  map[string]orchestratorapi.BypassGroupRuntimeState
	bypassFlow                    []orchestratorapi.BypassFlowStats
	bypassProbeTargets            int
	dpdkMu                        sync.Mutex
	dpdkActive                    map[string]vpp.NativePath
	tracker                       *orchestrator.HealthTracker
	probe                         orchestrator.HealthProbe
	capabilityReport              string
	managementInterface           string
	linuxInterface                func(context.Context, string) string
	proofCommand                  string
	// mirrorProvider returns the desired SPAN generation plus the VPP interface
	// names whose sessions must be disabled first. The HTTP layer owns the
	// interface documents, so it installs this callback after the server is
	// built; every transparent apply (topology save, explicit apply, recovery)
	// then converges the mirror in the same transaction.
	mirrorProvider func(context.Context) (*vpp.MirrorConfig, []string, error)
}

func (runtime *productionOrchestratorRuntime) ApplyTransparent(ctx context.Context, requestID string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	return runtime.applyTransparentLocked(ctx, requestID, topology, policy)
}

// SetMirrorProvider installs the desired port-mirror source. It must be called
// once during startup, before the control API starts accepting writes.
func (runtime *productionOrchestratorRuntime) SetMirrorProvider(provider func(context.Context) (*vpp.MirrorConfig, []string, error)) {
	runtime.mirrorProvider = provider
}

func (runtime *productionOrchestratorRuntime) desiredMirror(ctx context.Context) (*vpp.MirrorConfig, []string, error) {
	if runtime.mirrorProvider == nil {
		return nil, nil, nil
	}
	return runtime.mirrorProvider(ctx)
}

func (runtime *productionOrchestratorRuntime) applyTransparentLocked(ctx context.Context, requestID string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	// The persisted topology is the operator's intent and may still carry
	// product ids from an older build. Resolve every port once and use the same
	// kernel-name view for the probe, the bonds, the generation and VPP.
	topologyView := runtime.resolveTopologyView(ctx, topology.View())
	interfaces := transparentTopologyInterfaces(topologyView)
	mirror, mirrorCleanup, mirrorErr := runtime.desiredMirror(ctx)
	if mirrorErr != nil {
		return mirrorErr
	}
	if mirror != nil {
		// The mirror destination usually has no LAN/WAN role, so it is not part
		// of the topology interface set. Attach it (and every source) or VPP
		// never creates the interface the SPAN command refers to.
		interfaces = appendTransparentInterfaces(interfaces, mirror.LinuxInterface)
		for _, source := range mirror.Sources {
			interfaces = appendTransparentInterfaces(interfaces, source.LinuxInterface)
		}
	}
	path, err := runtime.selectAndPrepare(ctx, requestID, interfaces, topologyView.ManagementShared)
	if err != nil {
		return err
	}
	if err := runtime.attachNativePath(ctx, requestID, path); err != nil {
		runtime.releaseDPDK(ctx, requestID, err)
		return err
	}
	previousBonds := append([]vpp.BondState(nil), runtime.transparentBonds...)
	desiredBonds := vpp.TransparentOrchestratorBonds(topologyView)
	var policyView *orchestrator.PolicyView
	if policy != nil {
		view := policy.View()
		policyView = &view
	}
	generation, err := transparentGeneration(topologyView, policyView)
	if err != nil {
		return err
	}
	if err := runtime.beginTransparentTransaction(requestID, "apply", generation, previousBonds, desiredBonds); err != nil {
		runtime.releaseDPDK(ctx, requestID, err)
		return err
	}
	if len(previousBonds) > 0 || len(desiredBonds) > 0 {
		if _, err := runtime.adapter.ApplyTransparentBondTransition(ctx, requestID+"-bonds", previousBonds, desiredBonds); err != nil {
			runtime.releaseDPDK(ctx, requestID, err)
			return err
		}
	}
	readback, err := runtime.adapter.ApplyTransparentOrchestrator(ctx, requestID, vpp.TransparentOrchestratorConfig{Generation: generation, Topology: topologyView, Policy: policyView, Mirror: mirror, MirrorCleanup: mirrorCleanup})
	if err != nil {
		var rollbackErr error
		if len(previousBonds) > 0 || len(desiredBonds) > 0 {
			_, rollbackErr = runtime.adapter.ApplyTransparentBondTransition(ctx, requestID+"-bond-rollback", desiredBonds, previousBonds)
		}
		runtime.releaseDPDK(ctx, requestID, err)
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("restore transparent orchestrator bonds: %w", rollbackErr))
		}
		if clearErr := runtime.clearTransparentTransactionLocked(); clearErr != nil {
			return errors.Join(err, fmt.Errorf("clear rolled-back transparent transaction: %w", clearErr))
		}
		return err
	}
	runtime.transparentBonds = append([]vpp.BondState(nil), desiredBonds...)
	runtime.transparentRequest = requestID
	runtime.transparentGeneration = generation
	runtime.transparentAppliedAt = time.Now().UTC()
	runtime.transparentConfiguredDefault = transparentDefaultAction(policyView)
	runtime.transparentObservedGeneration = readback.Generation
	runtime.transparentObservedDefault = readback.Default
	runtime.observeBypassReadback(readback)
	runtime.lastReconcileAt = runtime.transparentAppliedAt
	return nil
}

func (runtime *productionOrchestratorRuntime) observeBypassReadback(observed vpp.TransparentOrchestratorReadback) {
	runtime.bypassMu.Lock()
	defer runtime.bypassMu.Unlock()
	if observed.Bypass && !runtime.bypassActive {
		runtime.bypassSince = time.Now().UTC()
	}
	if !observed.Bypass {
		runtime.bypassSince = time.Time{}
		runtime.bypassCause = ""
		runtime.bypassReason = ""
	} else {
		runtime.bypassCause = observed.BypassCause
		runtime.bypassReason = observed.BypassReason
	}
	runtime.bypassActive = observed.Bypass
	runtime.bypassForced = observed.BypassForced
	runtime.bypassPackets = observed.BypassPackets
	runtime.bypassGroups = bypassGroupsFromReadback(observed)
	runtime.bypassFlow = bypassFlowFromReadback(observed)
	runtime.bypassProbeTargets = observed.ProbeTargets
}

func bypassGroupsFromReadback(observed vpp.TransparentOrchestratorReadback) map[string]orchestratorapi.BypassGroupRuntimeState {
	groups := make(map[string]orchestratorapi.BypassGroupRuntimeState, len(observed.GroupBypass))
	for _, row := range observed.GroupBypass {
		groups[row.Name] = orchestratorapi.BypassGroupRuntimeState{
			Enabled: row.Enabled, Forced: row.Forced, Cause: row.Cause,
			Reason: row.Reason, Packets: row.Packets,
		}
	}
	return groups
}

func bypassFlowFromReadback(observed vpp.TransparentOrchestratorReadback) []orchestratorapi.BypassFlowStats {
	stats := make([]orchestratorapi.BypassFlowStats, 0, len(observed.GroupProbes))
	for _, row := range observed.GroupProbes {
		success := row.SynAck + row.Responses
		timeouts := uint64(0)
		if row.Attempts > success+row.RST {
			timeouts = row.Attempts - success - row.RST
		}
		stats = append(stats, orchestratorapi.BypassFlowStats{
			Group: row.Group, DstIP: row.DstIP, DstPort: row.DstPort,
			Protocol: row.Protocol, Attempts: row.Attempts, Syn: row.Syn,
			SynAck: row.SynAck, RST: row.RST, Responses: row.Responses,
			Success: success, Timeouts: timeouts,
			ICMPUnreachable: row.ICMPUnreachable,
		})
	}
	return stats
}

// transparentGeneration is the single source of truth for the VPP generation
// of a topology/policy pair.  Apply and reconcile must compute it identically,
// otherwise a readback comparison would report drift for an unchanged config.
func transparentGeneration(topologyView orchestrator.TopologyView, policyView *orchestrator.PolicyView) (string, error) {
	payload, err := json.Marshal(struct {
		Topology orchestrator.TopologyView `json:"topology"`
		Policy   *orchestrator.PolicyView  `json:"policy,omitempty"`
	}{Topology: topologyView, Policy: policyView})
	if err != nil {
		return "", fmt.Errorf("encode transparent orchestrator generation: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func transparentDefaultAction(policyView *orchestrator.PolicyView) string {
	if policyView == nil || strings.TrimSpace(string(policyView.Default.Kind)) == "" {
		return string(orchestrator.ActionDirect)
	}
	return string(policyView.Default.Kind)
}

// RecoverTransparent restores the persisted topology/policy when VPP restarts
// and reloads the plugin in the locked state, or when the committed VPP
// generation no longer matches the persisted configuration (for example after
// two overlapping applies committed the older generation last).  It holds the
// transparent mutex across the readback and replay so a user apply cannot
// interleave with the recovery generation.  No topology means orchestration is
// intentionally disabled, so recovery is a no-op.
func (runtime *productionOrchestratorRuntime) RecoverTransparent(ctx context.Context, repository *orchestrator.Repository) error {
	topology, _, err := repository.Snapshot(ctx)
	if errors.Is(err, orchestrator.ErrTopologyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read orchestrator topology for recovery: %w", err)
	}
	var policy *orchestrator.Policy
	current, _, policyErr := repository.PolicySnapshot(ctx)
	switch {
	case policyErr == nil:
		policy = &current
	case errors.Is(policyErr, orchestrator.ErrPolicyNotFound):
	default:
		return fmt.Errorf("read orchestrator policy for recovery: %w", policyErr)
	}
	return runtime.reconcileTransparent(ctx, "orchestrator-recovery", topology, policy, true)
}

// ReconcileTransparent compares the committed VPP generation and default
// action with the configuration that was just persisted, and re-applies the
// configuration when the readback disagrees.  Callers persist first and then
// call this, so the data plane is verified against the durable desired state
// rather than against whatever an earlier request happened to apply.
func (runtime *productionOrchestratorRuntime) ReconcileTransparent(ctx context.Context, requestID string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	return runtime.reconcileTransparent(ctx, requestID, topology, policy, false)
}

func (runtime *productionOrchestratorRuntime) reconcileTransparent(ctx context.Context, requestID string, topology orchestrator.Topology, policy *orchestrator.Policy, skipWhileMutationPending bool) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	if skipWhileMutationPending && runtime.transparentJournalPending {
		// A configuration mutation is between its VPP apply and its
		// persistence. The periodic reconciler must not compare the durable
		// (older) policy with that in-flight generation and undo it.
		return nil
	}
	return runtime.reconcileTransparentLocked(ctx, requestID, topology, policy)
}

func (runtime *productionOrchestratorRuntime) reconcileTransparentLocked(ctx context.Context, requestID string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	topologyView := runtime.resolveTopologyView(ctx, topology.View())
	var policyView *orchestrator.PolicyView
	if policy != nil {
		view := policy.View()
		policyView = &view
	}
	expected, err := transparentGeneration(topologyView, policyView)
	if err != nil {
		runtime.reconcileFailures++
		return err
	}
	configuredDefault := transparentDefaultAction(policyView)
	observed, observeErr := runtime.adapter.ObserveTransparentOrchestrator(ctx, requestID+"-observe")
	runtime.lastReconcileAt = time.Now().UTC()
	if observeErr == nil && transparentStateMatchesConfiguration(observed, expected, configuredDefault) {
		runtime.markReconcileConsistent(requestID, expected, configuredDefault, observed)
		if runtime.transparentJournalPending {
			return runtime.clearTransparentTransactionLocked()
		}
		return nil
	}
	reason := transparentDriftReason(expected, configuredDefault, observed, observeErr)
	runtime.driftDetected = true
	runtime.lastDriftReason = reason
	runtime.reconcileRepairs++
	log.Printf("transparent orchestrator policy drift detected: %s; re-applying configuration generation %s", reason, shortGeneration(expected))
	if err := runtime.applyTransparentLocked(ctx, requestID, topology, policy); err != nil {
		runtime.reconcileFailures++
		if clearErr := runtime.clearTransparentTransactionLocked(); clearErr != nil {
			log.Printf("clear failed reconcile transaction journal: %v", clearErr)
		}
		return fmt.Errorf("transparent orchestrator reconcile apply: %w", err)
	}
	verify, verifyErr := runtime.adapter.ObserveTransparentOrchestrator(ctx, requestID+"-verify")
	if verifyErr != nil || !transparentStateMatchesConfiguration(verify, expected, configuredDefault) {
		runtime.reconcileFailures++
		reason = transparentDriftReason(expected, configuredDefault, verify, verifyErr)
		runtime.lastDriftReason = reason
		return fmt.Errorf("transparent orchestrator reconcile readback failed after re-apply: %s", reason)
	}
	runtime.markReconcileConsistent(requestID, expected, configuredDefault, verify)
	return runtime.clearTransparentTransactionLocked()
}

func (runtime *productionOrchestratorRuntime) markReconcileConsistent(requestID, generation, configuredDefault string, observed vpp.TransparentOrchestratorReadback) {
	repaired := runtime.driftDetected
	runtime.transparentRequest = requestID
	runtime.transparentGeneration = generation
	runtime.transparentObservedGeneration = observed.Generation
	runtime.transparentObservedDefault = observed.Default
	runtime.transparentConfiguredDefault = configuredDefault
	runtime.observeBypassReadback(observed)
	if runtime.transparentAppliedAt.IsZero() || repaired {
		runtime.transparentAppliedAt = time.Now().UTC()
	}
	runtime.driftDetected = false
}

func transparentStateMatchesConfiguration(observed vpp.TransparentOrchestratorReadback, expectedGeneration, configuredDefault string) bool {
	if observed.State != "running" || observed.Generation != expectedGeneration {
		return false
	}
	if strings.TrimSpace(observed.Default) != "" && observed.Default != configuredDefault {
		return false
	}
	return true
}

func transparentDriftReason(expectedGeneration, configuredDefault string, observed vpp.TransparentOrchestratorReadback, observeErr error) string {
	observedDefault := strings.TrimSpace(observed.Default)
	if observedDefault == "" {
		observedDefault = "unknown"
	}
	if observeErr != nil {
		return fmt.Sprintf("configured generation %s default %s, VPP readback failed: %v", shortGeneration(expectedGeneration), configuredDefault, observeErr)
	}
	return fmt.Sprintf("configured generation %s default %s, VPP state %s generation %s default %s", shortGeneration(expectedGeneration), configuredDefault, observed.State, shortGeneration(observed.Generation), observedDefault)
}

func shortGeneration(generation string) string {
	generation = strings.TrimSpace(generation)
	if len(generation) > 12 {
		return generation[:12]
	}
	if generation == "" {
		return "unknown"
	}
	return generation
}

func runTransparentRecovery(ctx context.Context, repository *orchestrator.Repository, runtime *productionOrchestratorRuntime, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := runtime.RecoverTransparent(ctx, repository); err != nil {
				message := err.Error()
				if message != lastError {
					log.Printf("transparent orchestrator recovery failed: %v", err)
					lastError = message
				}
				continue
			}
			if lastError != "" {
				log.Printf("transparent orchestrator recovery succeeded")
				lastError = ""
			}
		}
	}
}

func (runtime *productionOrchestratorRuntime) DisableTransparent(ctx context.Context, requestID string) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	if err := runtime.beginTransparentTransaction(requestID, "disable", "", runtime.transparentBonds, nil); err != nil {
		return err
	}
	err := runtime.adapter.DisableTransparentOrchestrator(ctx, requestID)
	if err == nil && len(runtime.transparentBonds) > 0 {
		_, err = runtime.adapter.ApplyTransparentBondTransition(ctx, requestID+"-bonds", runtime.transparentBonds, nil)
		if err == nil {
			runtime.transparentBonds = nil
		}
	}
	if err == nil {
		runtime.transparentRequest = ""
		runtime.transparentGeneration = ""
		runtime.transparentAppliedAt = time.Time{}
		runtime.transparentConfiguredDefault = ""
		runtime.transparentObservedGeneration = ""
		runtime.transparentObservedDefault = ""
		runtime.setBypassLockedStateOnly(false, false, "", "")
	}
	runtime.releaseDPDK(ctx, requestID, err)
	return err
}

// SetBypass toggles the plugin fault-bypass flag.  It serialises with apply and
// reconcile, but deliberately leaves the committed generation untouched: the
// configured policy is still in the data plane and applies again the moment
// the bypass is cleared.
func (runtime *productionOrchestratorRuntime) SetBypass(ctx context.Context, enabled bool, cause, reason string) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	return runtime.setBypassLocked(ctx, enabled, cause, reason)
}

func (runtime *productionOrchestratorRuntime) setBypassLocked(ctx context.Context, enabled bool, cause, reason string) error {
	requestID := fmt.Sprintf("orchestrator-bypass-%d", time.Now().UnixNano())
	readback, err := runtime.adapter.SetTransparentOrchestratorBypass(ctx, requestID, enabled, cause, reason)
	if err != nil {
		return err
	}
	runtime.bypassMu.Lock()
	if enabled && !runtime.bypassActive {
		runtime.bypassSince = time.Now().UTC()
	}
	if !enabled {
		runtime.bypassSince = time.Time{}
	}
	runtime.bypassActive = readback.Bypass
	runtime.bypassForced = enabled && strings.EqualFold(strings.TrimSpace(cause), "manual")
	runtime.bypassCause = readback.BypassCause
	runtime.bypassReason = readback.BypassReason
	runtime.bypassPackets = readback.BypassPackets
	runtime.bypassMu.Unlock()
	action := "cleared"
	if enabled {
		action = "enabled"
	}
	log.Printf("orchestrator fault bypass %s: cause=%s reason=%s generation=%s", action, cause, reason, shortGeneration(runtime.transparentGeneration))
	return nil
}

// setBypassLocked clears the cached bypass state without issuing a VPP
// command (used when the orchestrator is disabled).
func (runtime *productionOrchestratorRuntime) setBypassLockedStateOnly(enabled bool, forced bool, cause, reason string) {
	runtime.bypassMu.Lock()
	defer runtime.bypassMu.Unlock()
	runtime.bypassActive = enabled
	runtime.bypassForced = forced
	runtime.bypassCause = cause
	runtime.bypassReason = reason
	if !enabled {
		runtime.bypassSince = time.Time{}
	}
}

func (runtime *productionOrchestratorRuntime) BypassSnapshot() orchestratorapi.BypassRuntimeState {
	runtime.bypassMu.RLock()
	defer runtime.bypassMu.RUnlock()
	groups := make(map[string]orchestratorapi.BypassGroupRuntimeState, len(runtime.bypassGroups))
	for name, state := range runtime.bypassGroups {
		groups[name] = state
	}
	return orchestratorapi.BypassRuntimeState{
		Enabled:      runtime.bypassActive,
		Forced:       runtime.bypassForced,
		Cause:        runtime.bypassCause,
		Reason:       runtime.bypassReason,
		Since:        runtime.bypassSince,
		Packets:      runtime.bypassPackets,
		Generation:   runtime.transparentGeneration,
		Groups:       groups,
		Flow:         append([]orchestratorapi.BypassFlowStats(nil), runtime.bypassFlow...),
		ProbeTargets: runtime.bypassProbeTargets,
	}
}

// SetGroupBypass toggles one policy group's bypass flag.  It uses the same
// serialisation as apply/reconcile and leaves the committed generation and all
// other groups untouched.
func (runtime *productionOrchestratorRuntime) SetGroupBypass(ctx context.Context, group string, enabled bool, cause, reason string) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	requestID := fmt.Sprintf("orchestrator-group-bypass-%d", time.Now().UnixNano())
	readback, err := runtime.adapter.SetTransparentOrchestratorGroupBypass(ctx, requestID, group, enabled, cause, reason)
	if err != nil {
		return err
	}
	runtime.observeBypassReadback(readback)
	action := "cleared"
	if enabled {
		action = "enabled"
	}
	log.Printf("orchestrator group fault bypass %s for %s: cause=%s reason=%s generation=%s", action, group, cause, reason, shortGeneration(runtime.transparentGeneration))
	return nil
}

// SetGroupProbes installs the passive request/response targets.  They are
// runtime state, not part of the committed policy generation.
func (runtime *productionOrchestratorRuntime) SetGroupProbes(ctx context.Context, targets []groupProbeTarget) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	converted := make([]vpp.GroupProbeTarget, 0, len(targets))
	for _, target := range targets {
		converted = append(converted, vpp.GroupProbeTarget{
			Group: target.Group, DstIP: target.DstIP, DstPort: target.DstPort,
			Protocol: target.Protocol, WindowSeconds: target.WindowSeconds,
		})
	}
	requestID := fmt.Sprintf("orchestrator-group-probes-%d", time.Now().UnixNano())
	if err := runtime.adapter.SetTransparentOrchestratorProbes(ctx, requestID, converted); err != nil {
		return err
	}
	runtime.bypassMu.Lock()
	runtime.bypassProbeTargets = len(converted)
	runtime.bypassMu.Unlock()
	return nil
}

// ObserveBypassRuntime reads the plugin telemetry (group flags and passive
// counters) without holding the apply serialisation while the probe runs.
func (runtime *productionOrchestratorRuntime) ObserveBypassRuntime(ctx context.Context) (orchestratorapi.BypassRuntimeState, error) {
	if !runtime.transparentMu.TryLock() {
		return runtime.BypassSnapshot(), errors.New("transparent orchestrator runtime is applying or recovering")
	}
	defer runtime.transparentMu.Unlock()
	requestID := fmt.Sprintf("orchestrator-bypass-telemetry-%d", time.Now().UnixNano())
	observed, err := runtime.adapter.ObserveTransparentOrchestrator(ctx, requestID)
	if err != nil {
		return runtime.BypassSnapshot(), err
	}
	runtime.observeBypassReadback(observed)
	return runtime.BypassSnapshot(), nil
}

type transparentTransactionJournal struct {
	Version       int             `json:"version"`
	TransactionID string          `json:"transaction_id"`
	Operation     string          `json:"operation"`
	Generation    string          `json:"generation,omitempty"`
	PreviousBonds []vpp.BondState `json:"previous_bonds,omitempty"`
	DesiredBonds  []vpp.BondState `json:"desired_bonds,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
}

func (runtime *productionOrchestratorRuntime) beginTransparentTransaction(transactionID, operation, generation string, previous, desired []vpp.BondState) error {
	journal := transparentTransactionJournal{Version: 1, TransactionID: strings.TrimSpace(transactionID), Operation: operation, Generation: generation, PreviousBonds: append([]vpp.BondState(nil), previous...), DesiredBonds: append([]vpp.BondState(nil), desired...), StartedAt: time.Now().UTC()}
	if journal.TransactionID == "" || (journal.Operation != "apply" && journal.Operation != "disable") {
		return errors.New("transparent transaction journal identity is invalid")
	}
	content, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode transparent transaction journal: %w", err)
	}
	path := strings.TrimSpace(runtime.journalPath)
	if path == "" {
		return errors.New("transparent transaction journal path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("create transparent transaction journal directory: %w", err)
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open transparent transaction journal: %w", err)
	}
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("write transparent transaction journal: %w", errors.Join(err, closeErr))
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("commit transparent transaction journal: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	runtime.transparentJournalPending = true
	return nil
}

func (runtime *productionOrchestratorRuntime) loadTransparentTransactionJournal() error {
	path := strings.TrimSpace(runtime.journalPath)
	if path == "" {
		return errors.New("transparent transaction journal path is not configured")
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal transparentTransactionJournal
	if err := json.Unmarshal(content, &journal); err != nil {
		return fmt.Errorf("decode transparent transaction journal: %w", err)
	}
	if journal.Version != 1 || strings.TrimSpace(journal.TransactionID) == "" || (journal.Operation != "apply" && journal.Operation != "disable") {
		return errors.New("transparent transaction journal is invalid")
	}
	runtime.transparentBonds = append([]vpp.BondState(nil), journal.DesiredBonds...)
	runtime.transparentJournalPending = true
	return nil
}

func (runtime *productionOrchestratorRuntime) CommitTransparentTransaction(context.Context) error {
	runtime.transparentMu.Lock()
	defer runtime.transparentMu.Unlock()
	return runtime.clearTransparentTransactionLocked()
}

func (runtime *productionOrchestratorRuntime) clearTransparentTransactionLocked() error {
	if !runtime.transparentJournalPending {
		return nil
	}
	path := strings.TrimSpace(runtime.journalPath)
	if path == "" {
		return errors.New("transparent transaction journal path is not configured")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove transparent transaction journal: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	runtime.transparentJournalPending = false
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transaction journal directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync transaction journal directory: %w", err)
	}
	return nil
}

func (runtime *productionOrchestratorRuntime) TransparentRuntimeEvidence(ctx context.Context) (orchestratorapi.TransparentRuntimeEvidence, error) {
	if !runtime.transparentMu.TryLock() {
		return orchestratorapi.TransparentRuntimeEvidence{}, errors.New("transparent orchestrator runtime is applying or recovering")
	}
	defer runtime.transparentMu.Unlock()
	if strings.TrimSpace(runtime.transparentRequest) == "" || strings.TrimSpace(runtime.transparentGeneration) == "" || runtime.transparentAppliedAt.IsZero() {
		return orchestratorapi.TransparentRuntimeEvidence{}, errors.New("transparent orchestrator has no committed runtime generation")
	}
	observed, err := runtime.adapter.ObserveTransparentOrchestrator(ctx, runtime.transparentRequest+"-status")
	if err != nil {
		return orchestratorapi.TransparentRuntimeEvidence{}, err
	}
	evidence := orchestratorapi.TransparentRuntimeEvidence{
		TransactionID:     runtime.transparentRequest,
		Generation:        observed.Generation,
		State:             observed.State,
		AppliedAt:         runtime.transparentAppliedAt,
		ObservedAt:        time.Now().UTC(),
		PolicyGeneration:  runtime.transparentGeneration,
		VPPGeneration:     observed.Generation,
		ConfiguredDefault: runtime.transparentConfiguredDefault,
		ObservedDefault:   observed.Default,
		Repairs:           runtime.reconcileRepairs,
		ReconcileFailures: runtime.reconcileFailures,
		LastDriftReason:   runtime.lastDriftReason,
		LastReconciledAt:  runtime.lastReconcileAt,
	}
	runtime.observeBypassReadback(observed)
	bypass := runtime.BypassSnapshot()
	evidence.Bypass = bypass.Enabled
	evidence.BypassForced = bypass.Forced
	evidence.BypassCause = bypass.Cause
	evidence.BypassReason = bypass.Reason
	evidence.BypassSince = bypass.Since
	evidence.BypassPackets = bypass.Packets
	if len(bypass.Groups) > 0 {
		for name, group := range bypass.Groups {
			if group.Enabled {
				evidence.BypassGroups = append(evidence.BypassGroups, name)
			}
		}
		sort.Strings(evidence.BypassGroups)
		if len(evidence.BypassGroups) > 0 {
			evidence.Bypass = true
			if evidence.BypassCause == "" {
				evidence.BypassCause = "auto"
			}
			if evidence.BypassReason == "" {
				evidence.BypassReason = "groups:" + strings.Join(evidence.BypassGroups, ",")
			}
		}
	}
	if observed.State != "running" {
		return orchestratorapi.TransparentRuntimeEvidence{}, fmt.Errorf("transparent orchestrator state is %s (configured generation %s default %s)", observed.State, shortGeneration(runtime.transparentGeneration), runtime.transparentConfiguredDefault)
	}
	if observed.Generation != runtime.transparentGeneration {
		return orchestratorapi.TransparentRuntimeEvidence{}, fmt.Errorf("transparent orchestrator policy drift: configured generation %s default %s, VPP generation %s default %s (repairs %d)", shortGeneration(runtime.transparentGeneration), runtime.transparentConfiguredDefault, shortGeneration(observed.Generation), nonEmpty(observed.Default, "unknown"), runtime.reconcileRepairs)
	}
	if strings.TrimSpace(observed.Default) != "" && observed.Default != runtime.transparentConfiguredDefault {
		return orchestratorapi.TransparentRuntimeEvidence{}, fmt.Errorf("transparent orchestrator policy drift: configured default %s, VPP default %s generation %s (repairs %d)", runtime.transparentConfiguredDefault, observed.Default, shortGeneration(observed.Generation), runtime.reconcileRepairs)
	}
	return evidence, nil
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type transparentStartupRuntime interface {
	ApplyTransparent(context.Context, string, orchestrator.Topology, *orchestrator.Policy) error
}

func reconcileTransparentStartup(ctx context.Context, repository *orchestrator.Repository, runtime transparentStartupRuntime) error {
	topology, _, err := repository.Snapshot(ctx)
	if errors.Is(err, orchestrator.ErrTopologyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var policy *orchestrator.Policy
	current, _, err := repository.PolicySnapshot(ctx)
	if err == nil {
		policy = &current
	} else if !errors.Is(err, orchestrator.ErrPolicyNotFound) {
		return err
	}
	return runtime.ApplyTransparent(ctx, "orchestrator-startup", topology, policy)
}

func transparentTopologyInterfaces(topology orchestrator.TopologyView) []string {
	seen := map[string]bool{}
	result := []string{}
	bonds := transparentBondMemberMap(topology)
	bondIDs := make(map[string]bool, len(bonds))
	for id := range bonds {
		bondIDs[id] = true
	}
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || bondIDs[name] || seen[name] {
			return
		}
		seen[name] = true
		result = append(result, name)
	}
	for _, item := range topology.Interfaces {
		if item.Bond == nil {
			appendName(item.Port)
			continue
		}
		for _, member := range item.Bond.Members {
			appendName(member)
		}
	}
	for _, group := range topology.Groups {
		for _, port := range group.Ports {
			if port.Bond != nil {
				for _, member := range port.Bond.Members {
					appendName(member)
				}
				continue
			}
			appendName(port.Interface)
		}
	}
	for _, members := range bonds {
		for _, member := range members {
			appendName(member)
		}
	}
	sort.Strings(result)
	return result
}

// transparentBondMemberMap returns every bond in the topology and its members.
// A bond is a synthetic VPP interface; probing it produces a spurious
// runtime_capability_proof failure because it has no PCI/native proof of its
// own. Its members are the dataplane ports that must be proven.
func transparentBondMemberMap(topology orchestrator.TopologyView) map[string][]string {
	result := map[string][]string{}
	add := func(name string, members []string) {
		name = strings.TrimSpace(name)
		if name == "" || len(members) == 0 {
			return
		}
		resolved := make([]string, 0, len(members))
		for _, member := range members {
			if member = strings.TrimSpace(member); member != "" {
				resolved = append(resolved, member)
			}
		}
		if len(resolved) > 0 {
			result[name] = resolved
		}
	}
	for _, item := range topology.Interfaces {
		if item.Bond != nil {
			add(item.Bond.Name, item.Bond.Members)
		}
	}
	for _, group := range topology.Groups {
		for _, port := range group.Ports {
			if port.Bond != nil {
				add(port.Bond.Name, port.Bond.Members)
			}
		}
	}
	return result
}

// appendTransparentInterfaces adds mirror-owned ports to the attachment set
// without duplicating a port already requested by the topology.
func appendTransparentInterfaces(interfaces []string, extra ...string) []string {
	seen := make(map[string]bool, len(interfaces)+len(extra))
	result := make([]string, 0, len(interfaces)+len(extra))
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	return result
}

func (runtime *productionOrchestratorRuntime) ApplyServiceChain(ctx context.Context, requestID string, chain orchestrator.ServiceChain, _ []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	if chain.Direct {
		result, err := runtime.adapter.ApplyServiceChain(ctx, requestID, chain, nil)
		runtime.releaseDPDK(ctx, requestID, err)
		return result, err
	}
	interfaces := serviceChainInterfaces(chain)
	path, err := runtime.selectAndPrepare(ctx, requestID, interfaces, false)
	if err != nil {
		return vpp.ServiceChainApplyResult{}, err
	}
	result, err := runtime.adapter.ApplyServiceChain(ctx, requestID, chain, path.Attachments)
	if err != nil {
		runtime.releaseDPDK(ctx, requestID, err)
	}
	return result, err
}

func (runtime *productionOrchestratorRuntime) ApplyServiceChainTransition(ctx context.Context, requestID string, desired, active orchestrator.ServiceChain, _ []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	if active.Direct {
		return runtime.adapter.ApplyServiceChainBypass(ctx, requestID, desired)
	}
	if desired.Direct {
		result, err := runtime.adapter.ApplyServiceChainTransition(ctx, requestID, desired, active, nil)
		runtime.releaseDPDK(ctx, requestID, err)
		return result, err
	}
	interfaces := serviceChainInterfaces(desired)
	path, err := runtime.selectAndPrepare(ctx, requestID, interfaces, false)
	if err != nil {
		return vpp.ServiceChainApplyResult{}, err
	}
	result, err := runtime.adapter.ApplyServiceChainTransition(ctx, requestID, desired, active, path.Attachments)
	if err != nil {
		runtime.releaseDPDK(ctx, requestID, err)
	}
	return result, err
}

func (runtime *productionOrchestratorRuntime) refreshRuntimeProof(ctx context.Context, interfaces []string, managementShared bool) error {
	interfaces = runtime.resolveLinuxInterfaces(ctx, interfaces)
	if len(interfaces) == 0 {
		return nil
	}
	commandPath := strings.TrimSpace(runtime.proofCommand)
	if commandPath == "" {
		return nil
	}
	if _, err := os.Stat(commandPath); err != nil {
		// Fixture and container tests deliberately run without the appliance
		// probe.  Production images always provide it.
		return nil
	}
	command := exec.CommandContext(ctx, commandPath)
	command.Env = append(os.Environ(),
		"LY_ROUTE_MANAGEMENT_INTERFACE="+runtime.managementInterface,
		"LY_ROUTE_MANAGEMENT_SHARED="+strconv.FormatBool(managementShared),
		"LY_ROUTE_VPP_DATA_INTERFACES="+strings.Join(interfaces, ","),
		"LY_ROUTE_VPP_CAPABILITY_PROOF="+runtime.capabilityReport,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("refresh VPP runtime capability proof: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// resolveLinuxInterfaces maps product ids to kernel netdev names and removes
// duplicates while preserving the first-seen order. A nil resolver is a no-op
// for fixtures and non-appliance builds.
func (runtime *productionOrchestratorRuntime) resolveLinuxInterfaces(ctx context.Context, interfaces []string) []string {
	if runtime.linuxInterface == nil || len(interfaces) == 0 {
		return interfaces
	}
	result := make([]string, 0, len(interfaces))
	seen := make(map[string]bool, len(interfaces))
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if resolved := strings.TrimSpace(runtime.linuxInterface(ctx, name)); resolved != "" {
			name = resolved
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	return result
}

// resolveTopologyView resolves management, logical ports, bonds and group ports
// to kernel netdev names so a restart never feeds ethN to the capability probe
// or to the VPP boundary/group commands.
func (runtime *productionOrchestratorRuntime) resolveTopologyView(ctx context.Context, view orchestrator.TopologyView) orchestrator.TopologyView {
	resolve := func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" || runtime.linuxInterface == nil {
			return value
		}
		if resolved := strings.TrimSpace(runtime.linuxInterface(ctx, value)); resolved != "" {
			return resolved
		}
		return value
	}
	resolved := view
	resolved.ManagementInterface = resolve(view.ManagementInterface)
	resolved.Interfaces = make([]orchestrator.InterfaceView, len(view.Interfaces))
	for index, item := range view.Interfaces {
		item.Port = resolve(item.Port)
		if item.Bond != nil {
			bond := *item.Bond
			bond.Members = make([]string, len(item.Bond.Members))
			for memberIndex, member := range item.Bond.Members {
				bond.Members[memberIndex] = resolve(member)
			}
			item.Bond = &bond
		}
		resolved.Interfaces[index] = item
	}
	resolved.Groups = make([]orchestrator.GroupView, len(view.Groups))
	for groupIndex, group := range view.Groups {
		group.Ports = make([]orchestrator.DirectedPortView, len(view.Groups[groupIndex].Ports))
		for portIndex, port := range view.Groups[groupIndex].Ports {
			port.Interface = resolve(port.Interface)
			if port.Bond != nil {
				bond := *port.Bond
				bond.Members = make([]string, len(port.Bond.Members))
				for memberIndex, member := range port.Bond.Members {
					bond.Members[memberIndex] = resolve(member)
				}
				port.Bond = &bond
			}
			group.Ports[portIndex] = port
		}
		resolved.Groups[groupIndex] = group
	}
	return resolved
}

func (runtime *productionOrchestratorRuntime) attachNativePath(ctx context.Context, requestID string, path vpp.NativePath) error {
	if path.Tier == vpp.DataplaneTierDPDK {
		// DPDK binding/restart is owned by the dataplane controller.
		return nil
	}
	operations := make([]vpp.Operation, 0, len(path.Attachments))
	for _, attachment := range path.Attachments {
		operations = append(operations, vpp.DataplaneAttachOperation(requestID+"-attach", attachment))
	}
	if len(operations) == 0 {
		return nil
	}
	if err := runtime.adapter.ExecuteOperations(ctx, operations); err != nil {
		return fmt.Errorf("attach orchestrator data interfaces: %w", err)
	}
	return nil
}

func (runtime *productionOrchestratorRuntime) selectAndPrepare(ctx context.Context, requestID string, interfaces []string, managementShared bool) (vpp.NativePath, error) {
	if err := runtime.refreshRuntimeProof(ctx, interfaces, managementShared); err != nil {
		return vpp.NativePath{}, err
	}
	request := vpp.LoadNativePathRequest(runtime.capabilityReport, runtime.managementInterface, interfaces, time.Now().UTC())
	// The console no longer gates the AF_PACKET fallback behind operator
	// consent (allowsAFPacketFor always resolves true: a headless appliance must
	// restore its dataplane without input). This path is used to validate a
	// topology before it is saved and used to build the request without the
	// flag, so a first-run save failed with af_fallback_required even though the
	// same plan applied fine.
	request.AFPacketConsent = true
	for index := range request.Assignments {
		request.Assignments[index].AFPacketConsent = true
	}
	path, err := vpp.SelectNativePath(request)
	if err != nil {
		return vpp.NativePath{}, err
	}
	if path.Tier != vpp.DataplaneTierDPDK {
		return path, nil
	}
	if runtime.dataplane == nil {
		return vpp.NativePath{}, fmt.Errorf("orchestrator DPDK fallback controller is unavailable")
	}
	runtime.dpdkMu.Lock()
	_, active := runtime.dpdkActive[requestID]
	runtime.dpdkMu.Unlock()
	if !active {
		receipt, err := runtime.dataplane.Apply(ctx, dataplane.Request{TransactionID: requestID, Path: path})
		if err != nil {
			return vpp.NativePath{}, fmt.Errorf("orchestrator DPDK fallback apply: %w", err)
		}
		if receipt.Changed {
			runtime.dpdkMu.Lock()
			if runtime.dpdkActive == nil {
				runtime.dpdkActive = map[string]vpp.NativePath{}
			}
			runtime.dpdkActive[requestID] = path
			runtime.dpdkMu.Unlock()
		}
	}
	return path, nil
}

func (runtime *productionOrchestratorRuntime) releaseDPDK(ctx context.Context, requestID string, cause error) {
	runtime.dpdkMu.Lock()
	_, active := runtime.dpdkActive[requestID]
	if active {
		delete(runtime.dpdkActive, requestID)
	}
	runtime.dpdkMu.Unlock()
	if active && runtime.dataplane != nil {
		if _, err := runtime.dataplane.Rollback(ctx, requestID); err != nil && cause == nil {
			log.Printf("orchestrator DPDK rollback failed for %s: %v", requestID, err)
		}
	}
}

func (runtime *productionOrchestratorRuntime) ServiceChainUnavailable(ctx context.Context, bindings []orchestrator.ServiceChainBindingInput) (map[string]bool, []orchestrator.GroupHealth, error) {
	unavailable, health := runtime.tracker.Evaluate(ctx, bindings, runtime.probe)
	return unavailable, health, nil
}

type icmpHealthProbe struct{ binary string }

func (probe icmpHealthProbe) Reachable(ctx context.Context, address string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return exec.CommandContext(probeCtx, probe.binary, "-n", "-c", "1", "-W", "1", strings.TrimSpace(address)).Run() == nil
}

func serviceChainInterfaces(chain orchestrator.ServiceChain) []string {
	seen := map[string]bool{}
	interfaces := []string{}
	appendInterface := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			interfaces = append(interfaces, name)
		}
	}
	for _, path := range []orchestrator.ServiceChainPath{chain.Forward, chain.Reverse} {
		appendInterface(path.IngressInterface)
		appendInterface(path.ExitInterface)
		for _, hop := range path.Hops {
			appendInterface(hop.IngressInterface)
			appendInterface(hop.ServiceInterface)
			appendInterface(hop.ReturnInterface)
		}
	}
	sort.Strings(interfaces)
	return interfaces
}

func loadProductProfile(buildID, manifestPath string) (product.Profile, error) {
	path := strings.TrimSpace(manifestPath)
	if path == "" {
		return product.ParseProfile(buildID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return product.Profile{}, fmt.Errorf("read product manifest %s: %w", path, err)
	}
	manifest, err := product.ParseManifest(data)
	if err != nil {
		return product.Profile{}, fmt.Errorf("parse product manifest %s: %w", path, err)
	}
	return manifest.Profile(), nil
}

type statePaths struct {
	Database string
	Config   string
}

func defaultStatePaths(profile product.Profile, exists func(string) bool) statePaths {
	if profile.ID() == product.Gateway().ID() {
		paths := statePaths{Database: "/var/lib/ly-route/gateway/ly-route.db", Config: "/var/lib/ly-route/gateway/config.json"}
		if exists("/var/lib/ly-route/ly-route.db") {
			paths.Database = "/var/lib/ly-route/ly-route.db"
		}
		if exists("/var/lib/ly-route/config.json") {
			paths.Config = "/var/lib/ly-route/config.json"
		}
		return paths
	}
	return statePaths{Database: "/var/lib/ly-route/orchestrator/ly-route.db", Config: "/var/lib/ly-route/orchestrator/config.json"}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func openStore(ctx context.Context, path string, productID product.ID) (*persistence.Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return nil, err
		}
	}
	return persistence.OpenForProduct(ctx, path, productID)
}

func env(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

type vppctlInterfaceTelemetry struct {
	binary string
}

func (collector vppctlInterfaceTelemetry) Interfaces(ctx context.Context) ([]map[string]any, error) {
	binary := strings.TrimSpace(collector.binary)
	if binary == "" {
		binary = "vppctl"
	}
	output, err := exec.CommandContext(ctx, binary, "show", "interface").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("vppctl show interface failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return parseVPPInterfaceTelemetry(string(output)), nil
}

func parseVPPInterfaceTelemetry(output string) []map[string]any {
	var items []map[string]any
	var current map[string]any
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "lyroute-") {
			linuxName := strings.TrimPrefix(fields[0], "lyroute-")
			current = map[string]any{
				"id":            linuxName,
				"name":          linuxName,
				"vpp_interface": fields[0],
				"active_path":   "af_xdp",
				"work_mode":     "af_xdp",
				"admin_state":   "up",
				"runtime_state": "running",
				"rx_bps":        0,
				"tx_bps":        0,
				"rx_pps":        0,
				"tx_pps":        0,
				"rx_bytes":      0,
				"tx_bytes":      0,
				"rx_packets":    0,
				"tx_packets":    0,
				"sessions":      0,
			}
			if len(fields) > 2 {
				current["link_state"] = fields[2]
			}
			items = append(items, current)
		}
		if current != nil {
			applyVPPCounterLine(current, line)
		}
	}
	return items
}

func applyVPPCounterLine(item map[string]any, line string) {
	counters := map[string]string{
		"rx packets": "rx_packets",
		"tx packets": "tx_packets",
		"rx bytes":   "rx_bytes",
		"tx bytes":   "tx_bytes",
	}
	text := strings.ToLower(line)
	for label, key := range counters {
		if !strings.Contains(text, label) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err == nil {
			item[key] = value
		}
	}
}

// dumpEffectiveConfig prints the environment the control plane actually reads,
// with secret values masked, so "which value won" no longer needs shell
// archaeology during support.
func dumpEffectiveConfig() {
	fmt.Printf("version: %s\n", Version())
	keys := []string{
		"LY_ROUTE_PRODUCT_PROFILE", "LY_ROUTE_API_HOST", "LY_ROUTE_API_PORT",
		"LY_ROUTE_DB_PATH", "LY_ROUTE_CONFIG_PATH", "LY_ROUTE_ARTIFACT_MANIFEST",
		"LY_ROUTE_ADMIN_USERNAME", "LY_ROUTE_ADMIN_PASSWORD", "LY_ROUTE_ADMIN_PASSWORD_HASH",
		"LY_ROUTE_READONLY_USERNAME", "LY_ROUTE_READONLY_PASSWORD",
		"LY_ROUTE_SESSION_COOKIE_SECURE", "LY_ROUTE_FORCE_PASSWORD_CHANGE",
		"LY_ROUTE_INITIAL_ADMIN_PASSWORD_FILE", "LY_ROUTE_CONTROL_ENV_FILE",
		"LY_ROUTE_APPLY_TIMEOUT", "LY_ROUTE_PPROF", "LY_ROUTE_VERSION",
	}
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		if !set {
			fmt.Printf("%s=<unset>\n", key)
			continue
		}
		if key == "LY_ROUTE_ADMIN_PASSWORD" || key == "LY_ROUTE_READONLY_PASSWORD" || strings.Contains(key, "HASH") {
			fmt.Printf("%s=<redacted:%d chars>\n", key, len(value))
			continue
		}
		fmt.Printf("%s=%s\n", key, value)
	}
}
