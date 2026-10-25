package vpp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProbeSource records where an observed capability report came from. Operators
// use it to tell a live probe apart from a cached report or a reconstruction of
// the committed dataplane.
type ProbeSource string

const (
	ProbeSourceRuntime      ProbeSource = "runtime_probe"
	ProbeSourceCachedReport ProbeSource = "cached_report"
	ProbeSourceCommitted    ProbeSource = "committed_state"
	ProbeSourceAbsent       ProbeSource = "unavailable"
)

const (
	// DefaultProbeCacheTTL bounds how often the control plane repeats a probe.
	// The probe creates and removes VPP interfaces, so it must not run on every
	// status or plan request.
	DefaultProbeCacheTTL = 30 * time.Second
	// DefaultProbeTimeout bounds a single probe run. The probe performs VPP
	// interface create/delete cycles and must not hang an apply transaction.
	DefaultProbeTimeout = 120 * time.Second
)

// ProbeTarget is the interface context a probe runs against.
type ProbeTarget struct {
	ManagementInterface string
	ManagementShared    bool
	DataInterfaces      []string
}

func (target ProbeTarget) cacheKey(proofPath string) string {
	interfaces := append([]string(nil), target.DataInterfaces...)
	sort.Strings(interfaces)
	return strings.Join([]string{
		strings.TrimSpace(target.ManagementInterface),
		fmt.Sprintf("%t", target.ManagementShared),
		strings.Join(interfaces, ","),
		strings.TrimSpace(proofPath),
	}, "\x00")
}

func (target ProbeTarget) environment(proofPath string) []string {
	env := append(os.Environ(),
		"LY_ROUTE_MANAGEMENT_INTERFACE="+strings.TrimSpace(target.ManagementInterface),
		"LY_ROUTE_MANAGEMENT_SHARED="+fmt.Sprintf("%t", target.ManagementShared),
		"LY_ROUTE_VPP_DATA_INTERFACES="+strings.Join(target.DataInterfaces, ","),
		"LY_ROUTE_VPP_CAPABILITY_PROOF="+strings.TrimSpace(proofPath),
	)
	return env
}

// Observation is the result of one capability observation. Degraded is true
// whenever the live probe did not produce the report; callers must surface
// Reason as a degradation and must never convert it into dataplane_locked.
type Observation struct {
	Report   CapabilityReport
	Source   ProbeSource
	Observed time.Time
	Degraded bool
	Reason   string
}

// NativePathRequest turns the observation into a selection request. The report's
// own management interface is authoritative when present; otherwise the caller's
// value is used (the committed-state reconstruction carries no management
// identity).
func (observation Observation) NativePathRequest(management string, interfaces []string, now time.Time, managementShared bool) NativePathRequest {
	report := observation.Report
	prerequisites := []PrerequisiteResult{}
	reportManagement := strings.TrimSpace(report.ManagementInterface)
	if reportManagement == "" {
		report.ManagementInterface = strings.TrimSpace(management)
	}
	return nativePathRequestFromReport(report, prerequisites, management, interfaces, now, managementShared)
}

// CapabilityProber actively observes dataplane capability. arch-v1 replaces the
// pre-generated proof file as the source of truth: control runs the probe at
// startup and around apply, then reads the report the probe produced.
type CapabilityProber struct {
	// Command is the probe executable configured by
	// LY_ROUTE_VPP_PROBE_COMMAND (runtime-check.sh).
	Command string
	// ReportPath is the report the probe writes
	// (LY_ROUTE_VPP_CAPABILITY_PROOF).
	ReportPath string
	// CommittedStatePath is the committed dataplane state written by the last
	// successful transaction (dataplane/active.json). It is the fallback that
	// keeps forwarding alive when a probe cannot run.
	CommittedStatePath string
	CacheTTL           time.Duration
	Timeout            time.Duration
	Now                func() time.Time
	// Run executes the probe command with the supplied environment and is
	// injectable for tests.
	Run func(ctx context.Context, command string, env []string) error

	mu       sync.Mutex
	cached   *Observation
	cachedAt time.Time
	cacheKey string
}

// NewCapabilityProber builds a prober from the appliance environment.
func NewCapabilityProber(command, reportPath, committedStatePath string) *CapabilityProber {
	return &CapabilityProber{
		Command:            strings.TrimSpace(command),
		ReportPath:         strings.TrimSpace(reportPath),
		CommittedStatePath: strings.TrimSpace(committedStatePath),
	}
}

// Invalidate drops the cached observation so the next Observe performs a live
// probe. Apply uses this before committing a generation.
func (prober *CapabilityProber) Invalidate() {
	if prober == nil {
		return
	}
	prober.mu.Lock()
	prober.cached = nil
	prober.cachedAt = time.Time{}
	prober.cacheKey = ""
	prober.mu.Unlock()
}

func (prober *CapabilityProber) now() time.Time {
	if prober != nil && prober.Now != nil {
		return prober.Now().UTC()
	}
	return time.Now().UTC()
}

func (prober *CapabilityProber) cacheTTL() time.Duration {
	if prober.CacheTTL > 0 {
		return prober.CacheTTL
	}
	return DefaultProbeCacheTTL
}

func (prober *CapabilityProber) timeout() time.Duration {
	if prober.Timeout > 0 {
		return prober.Timeout
	}
	return DefaultProbeTimeout
}

func (prober *CapabilityProber) run(ctx context.Context, command string, env []string) error {
	if prober.Run != nil {
		return prober.Run(ctx, command, env)
	}
	executable := command
	if !strings.Contains(command, " ") && !strings.ContainsAny(command, "|&;<>") {
		// A plain path is executed directly so the probe cannot be affected by
		// shell word splitting.
		cmd := exec.CommandContext(ctx, executable)
		cmd.Env = env
		return cmd.Run()
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Observe returns the current observed capability, reusing a recent result for
// the same interface context.
func (prober *CapabilityProber) Observe(ctx context.Context, target ProbeTarget) Observation {
	if prober == nil {
		return Observation{Source: ProbeSourceAbsent, Degraded: true, Reason: "dataplane capability probe is not configured"}
	}
	now := prober.now()
	key := target.cacheKey(prober.ReportPath)
	prober.mu.Lock()
	defer prober.mu.Unlock()
	if prober.cached != nil && prober.cacheKey == key && now.Sub(prober.cachedAt) < prober.cacheTTL() {
		return *prober.cached
	}
	observation := prober.observe(ctx, target)
	prober.cached = &observation
	prober.cachedAt = now
	prober.cacheKey = key
	return observation
}

func (prober *CapabilityProber) observe(ctx context.Context, target ProbeTarget) Observation {
	reasons := make([]string, 0, 2)
	if strings.TrimSpace(prober.Command) != "" {
		probeCtx, cancel := context.WithTimeout(ctx, prober.timeout())
		err := prober.run(probeCtx, prober.Command, target.environment(prober.ReportPath))
		cancel()
		if err == nil {
			if report, loadErr := loadCapabilityReport(prober.ReportPath); loadErr == nil && reportBelongsTo(report, target.ManagementInterface) {
				return Observation{Report: report, Source: ProbeSourceRuntime, Observed: prober.now()}
			} else if loadErr != nil {
				reasons = append(reasons, "runtime probe wrote no usable capability report: "+loadErr.Error())
			} else {
				reasons = append(reasons, "runtime probe report belongs to a different management interface")
			}
		} else {
			reasons = append(reasons, "runtime probe failed: "+err.Error())
		}
	} else {
		reasons = append(reasons, "no dataplane probe command is configured")
	}

	if report, err := loadCapabilityReport(prober.ReportPath); err == nil && reportBelongsTo(report, target.ManagementInterface) {
		return Observation{
			Report:   report,
			Source:   ProbeSourceCachedReport,
			Observed: prober.now(),
			Degraded: true,
			Reason:   strings.Join(append(reasons, "using the last observed capability report"), "; "),
		}
	}

	if report, ok := committedCapabilityReport(prober.CommittedStatePath); ok {
		return Observation{
			Report:   report,
			Source:   ProbeSourceCommitted,
			Observed: prober.now(),
			Degraded: true,
			Reason:   strings.Join(append(reasons, "using the committed dataplane state so live forwarding is preserved"), "; "),
		}
	}

	return Observation{
		Source:   ProbeSourceAbsent,
		Observed: prober.now(),
		Degraded: true,
		Reason:   strings.Join(append(reasons, "no observed or committed dataplane capability is available"), "; "),
	}
}

func reportBelongsTo(report CapabilityReport, management string) bool {
	reportManagement := strings.TrimSpace(report.ManagementInterface)
	if reportManagement == "" {
		return true
	}
	return reportManagement == strings.TrimSpace(management)
}

// loadCapabilityReport strictly decodes an observed capability report. It
// mirrors the legacy proof-file contract, including the permission check, so a
// writable report cannot smuggle a capability into the dataplane.
func loadCapabilityReport(path string) (CapabilityReport, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CapabilityReport{}, fmt.Errorf("capability report path is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return CapabilityReport{}, err
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil {
		return CapabilityReport{}, statErr
	}
	if !info.Mode().IsRegular() {
		return CapabilityReport{}, fmt.Errorf("capability report is not a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return CapabilityReport{}, fmt.Errorf("capability report must not be group or world writable")
	}
	var report CapabilityReport
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return CapabilityReport{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return CapabilityReport{}, fmt.Errorf("capability report has trailing data")
	}
	return report, nil
}

// committedStateFile mirrors the fields of the committed dataplane state that
// arch-v1 needs. It intentionally decodes only what it uses so unrelated state
// additions do not break the fallback.
type committedStateFile struct {
	Path struct {
		Tier        DataplaneTier `json:"tier"`
		Attachments []struct {
			LinuxInterface string        `json:"linux_interface"`
			VPPInterface   string        `json:"vpp_interface"`
			MACAddress     string        `json:"mac_address"`
			Tier           DataplaneTier `json:"tier"`
			Hook           NativeHook    `json:"hook"`
			Mode           NativeMode    `json:"mode"`
			PCIAddress     string        `json:"pci_address"`
			KernelDriver   string        `json:"kernel_driver"`
			IOMMUGroup     string        `json:"iommu_group"`
			Verification   string        `json:"verification"`
		} `json:"attachments"`
	} `json:"path"`
}

// committedCapabilityReport reconstructs an observed capability report from the
// last committed dataplane transaction. The committed path was fully validated
// when it was applied; rebuilding it here lets a probe outage preserve live
// forwarding instead of locking the dataplane.
func committedCapabilityReport(path string) (CapabilityReport, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CapabilityReport{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return CapabilityReport{}, false
	}
	defer file.Close()
	var state committedStateFile
	if err := json.NewDecoder(file).Decode(&state); err != nil {
		return CapabilityReport{}, false
	}
	if state.Path.Tier != DataplaneTierNative && state.Path.Tier != DataplaneTierDPDK {
		return CapabilityReport{}, false
	}
	if len(state.Path.Attachments) == 0 {
		return CapabilityReport{}, false
	}
	now := time.Now().UTC()
	report := CapabilityReport{Proofs: make([]InterfaceProof, 0, len(state.Path.Attachments))}
	for _, attachment := range state.Path.Attachments {
		name := strings.TrimSpace(attachment.LinuxInterface)
		if !interfaceNameSafe(name) {
			continue
		}
		tier := attachment.Tier
		if tier == "" {
			tier = state.Path.Tier
		}
		hook := attachment.Hook
		if hook == "" {
			hook = NativeHookAFXDP
		}
		mode := attachment.Mode
		if mode == "" {
			mode = NativeModeZeroCopy
		}
		dpdk := tier == DataplaneTierDPDK
		afCompatibility := hook == NativeHookAFPacket && mode == NativeModeAFPacket || hook == NativeHookTapBridge && mode == NativeModeTapBridge
		functionalMethod := ""
		verificationState := ""
		if dpdk {
			functionalMethod = "committed_state"
			verificationState = "verified"
			switch strings.ToLower(strings.TrimSpace(attachment.Verification)) {
			case "pending_link":
				// Ownership survived a link-down event: approved, not yet
				// proven with traffic. It must never lock the dataplane.
				verificationState = "pending_link"
				functionalMethod = "pending_link"
			case "failed":
				verificationState = "failed"
			}
		}
		proof := CapabilityProof{
			Tier:               tier,
			Hook:               hook,
			Mode:               mode,
			Transport:          dpdkTransportForMode(mode),
			VPPInterface:       strings.TrimSpace(attachment.VPPInterface),
			MACAddress:         strings.TrimSpace(attachment.MACAddress),
			Source:             ProofSourceActiveRuntimeReadback,
			RuntimeVerified:    true,
			Native:             !dpdk,
			HighPerformance:    !afCompatibility,
			AcceptanceOnly:     afCompatibility,
			FunctionalVerified: dpdk && verificationState == "verified",
			Verification:       verificationState,
			FunctionalMethod:   functionalMethod,
			// The committed generation is authoritative until the next
			// transaction replaces it, so the reconstructable proof is not
			// stamped with a short lease.
			ObservedAt:          now.Add(-time.Minute),
			ValidUntil:          now.Add(365 * 24 * time.Hour),
			PerformanceScore:    100,
			PCIAddress:          strings.TrimSpace(attachment.PCIAddress),
			KernelDriver:        strings.TrimSpace(attachment.KernelDriver),
			IOMMUGroup:          strings.TrimSpace(attachment.IOMMUGroup),
			IOMMUProtected:      dpdk && mode == NativeModeDPDKVFIO,
			VFIOAvailable:       dpdk && mode == NativeModeDPDKVFIO,
			UIOPCIAvailable:     dpdk && mode == NativeModeDPDKUIO,
			HugepagesAvailable:  dpdk,
			DPDKPluginAvailable: dpdk,
		}
		report.Proofs = append(report.Proofs, InterfaceProof{
			LinuxInterface: name,
			Proof:          proof,
			Candidates:     []CapabilityProof{proof},
		})
	}
	if len(report.Proofs) == 0 {
		return CapabilityReport{}, false
	}
	return report, true
}
