package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	controlapi "ly-route/backend/internal/api"
)

// dataplaneReadiness is the operator-facing answer to "can this appliance
// forward right now?". It is deliberately derived from live VPP interface
// presence, not from a capability proof: a proof says the hardware can be
// attached, while a restarting VPP can still own zero data ports.
type dataplaneReadiness struct {
	Configured    []string `json:"configured_interfaces"`
	Realized      []string `json:"realized_interfaces"`
	Missing       []string `json:"missing_interfaces"`
	VPPResponding bool     `json:"-"`
	Reason        string   `json:"reason,omitempty"`
}

// Ready reports whether every configured data port is realized in VPP. An
// appliance with no configured data ports has nothing to forward and is
// therefore ready (there is no dataplane to wait for).
func (readiness dataplaneReadiness) Ready() bool {
	if len(readiness.Configured) == 0 {
		return true
	}
	return len(readiness.Missing) == 0 && len(readiness.Realized) > 0
}

func (server *Server) dataplaneReadiness(ctx context.Context) dataplaneReadiness {
	return server.dataplaneReadinessFor(ctx, server.runtimeDataInterfaces(ctx))
}

// dataplaneReadinessFor proves each configured Linux data port is present in
// VPP. It accepts the product's stable lyroute-<linux> name, an unchanged
// Linux name, and a DPDK hardware port located by PCI address. The last case
// matters because a DPDK attachment legitimately keeps VPP's
// GigabitEthernetX/Y/Z name while the plan addresses the Linux identity.
func (server *Server) dataplaneReadinessFor(ctx context.Context, interfaces []string) dataplaneReadiness {
	readiness := dataplaneReadiness{Configured: []string{}, Realized: []string{}, Missing: []string{}}
	seen := map[string]bool{}
	for _, raw := range interfaces {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		readiness.Configured = append(readiness.Configured, name)
	}
	if len(readiness.Configured) == 0 {
		return readiness
	}

	vppNames := server.vppInterfaceNames(ctx)
	readiness.VPPResponding = len(vppNames) > 0
	var hardware string
	hardwareLoaded := false
	for _, name := range readiness.Configured {
		if vppNames["lyroute-"+name] || vppNames[name] {
			readiness.Realized = append(readiness.Realized, name)
			continue
		}
		// A DPDK-bound NIC has no Linux netdev, so the inventory/ownership
		// record is the only place that still knows its PCI address.
		item, ok := readInventoryInterface(name)
		if !ok || strings.TrimSpace(item.PCIAddress) == "" {
			readiness.Missing = append(readiness.Missing, name)
			continue
		}
		if !hardwareLoaded {
			hardware, _ = execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "hardware-interfaces")
			hardwareLoaded = true
		}
		vppInterface := findVPPInterfaceByPCI(hardware, item.PCIAddress)
		if vppInterface != "" && vppNames[vppInterface] {
			readiness.Realized = append(readiness.Realized, name)
			continue
		}
		readiness.Missing = append(readiness.Missing, name)
	}
	if len(readiness.Missing) == 0 {
		return readiness
	}
	if !readiness.VPPResponding {
		readiness.Reason = "VPP is restarting or not answering; the configured data ports are not attached yet"
		return readiness
	}
	readiness.Reason = fmt.Sprintf("dataplane is not ready: %d of %d data ports are not attached to VPP", len(readiness.Missing), len(readiness.Configured))
	return readiness
}

// dataplaneHealthCapability exposes readiness as an ordinary capability
// dependency. It returns false when no data ports are configured so an
// appliance without a dataplane (or before its first data port is configured)
// still reports an honest `ok`.
func dataplaneHealthCapability(readiness dataplaneReadiness) (controlapi.CapabilityState, bool) {
	if len(readiness.Configured) == 0 {
		return controlapi.CapabilityState{}, false
	}
	if readiness.Ready() {
		return controlapi.CapabilityState{Name: "dataplane", Available: true, State: controlapi.CapabilityAvailable}, true
	}
	return controlapi.CapabilityState{Name: "dataplane", Available: false, State: controlapi.CapabilityDegraded, Reason: readiness.Reason}, true
}

// dataplaneTakeoverExpected reports whether a dataplane transaction has ever
// committed one of the configured data ports. On a truly first boot there is no
// committed dataplane yet: the apply transaction itself will attach the ports,
// so the readiness gate must not block it. Once a generation committed,
// forwarding was established, and missing VPP interfaces mean VPP is restarting
// (or the attachment was lost) rather than "not yet configured".
func (server *Server) dataplaneTakeoverExpected(ctx context.Context) bool {
	configured := map[string]bool{}
	for _, name := range server.runtimeDataInterfaces(ctx) {
		name = strings.TrimSpace(name)
		if name != "" {
			configured[name] = true
		}
	}
	if len(configured) == 0 {
		return false
	}
	// active.json is written only by a committed dataplane transaction. An
	// ownership/verification record can survive a failed or in-progress apply
	// and must not turn a first attach into a 45-second readiness wait.
	state, err := loadActiveDataplaneAttachments()
	if err != nil {
		return false
	}
	for _, name := range state {
		if configured[name] {
			return true
		}
	}
	return false
}

type activeDataplaneState struct {
	Path struct {
		Tier        string `json:"tier"`
		Attachments []struct {
			LinuxInterface string `json:"linux_interface"`
		} `json:"attachments"`
	} `json:"path"`
}

func loadActiveDataplaneAttachments() ([]string, error) {
	path := envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state activeDataplaneState
	if err := json.Unmarshal(content, &state); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(state.Path.Attachments))
	for _, attachment := range state.Path.Attachments {
		name := strings.TrimSpace(attachment.LinuxInterface)
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// waitForVPPResponsive is the apply-side preflight. It waits until VPP answers
// again, but deliberately does not wait for the data ports themselves: the
// apply transaction owns the attach operations and is what restores a lost
// attachment. Waiting for the ports first would deadlock a recovery apply.
// VPP answering is still the critical barrier: a restart that refuses the CLI
// would otherwise make every interface-addressed command a silent no-op.
func (server *Server) waitForVPPResponsive(ctx context.Context, timeout, interval time.Duration) dataplaneReadiness {
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		readiness := server.dataplaneReadiness(ctx)
		if readiness.VPPResponding || ctx.Err() != nil || !time.Now().Before(deadline) {
			return readiness
		}
		select {
		case <-ctx.Done():
			return readiness
		case <-time.After(interval):
		}
	}
}
