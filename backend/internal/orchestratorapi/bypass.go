package orchestratorapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	BypassPath       = "/api/v1/orchestrator/bypass"
	BypassConfigPath = "/api/v1/orchestrator/bypass/config"
)

// BypassFlowTarget selects the passive-probe traffic of one policy group.
//
// A flow target is matched against the client-to-server direction
// (destination address + destination port); the plugin then watches for the
// reverse packet (same five-tuple swapped) as the success signal.  UDP targets
// therefore only make sense for request/response services such as DNS, NTP or
// QUIC; one-way UDP streams must not be configured.
type BypassFlowTarget struct {
	DstIP    string `json:"dst_ip"`
	DstPort  int    `json:"dst_port"`
	Protocol string `json:"protocol,omitempty"`
}

// BypassTCPTarget is an optional active TCP connect probe.  Source, when set,
// is bound as the local address.  A host-side probe is only possible when the
// appliance has a reachable source address/route towards the target; the data
// plane group ports are not host interfaces.
type BypassTCPTarget struct {
	Target string `json:"target"`
	Source string `json:"source,omitempty"`
}

// BypassGroupConfig is the per-group fault-detection configuration.  Two
// groups are detected and bypassed independently: one group may bypass while
// the other keeps enforcing the configured policy.
type BypassGroupConfig struct {
	// Name is the orchestration group name (topology group, e.g. "grp-a").
	Name string `json:"name"`
	// Interfaces replaces the topology-derived group-port set when non-empty.
	Interfaces []string `json:"interfaces,omitempty"`
	// TCPTargets are optional active TCP connect probes.
	TCPTargets []BypassTCPTarget `json:"tcp_targets,omitempty"`
	// FlowTargets are the passive request/response tuples observed in the
	// data plane for this group.
	FlowTargets []BypassFlowTarget `json:"flow_targets,omitempty"`
	// WindowSeconds is the passive analysis window (default 10s).
	WindowSeconds float64 `json:"window_seconds,omitempty"`
	// FailureThreshold is the number of consecutive failed interface/TCP
	// rounds before bypass.  A passive window with requests but no success is
	// conclusive on its own and enters bypass when the window closes.
	FailureThreshold int `json:"failure_threshold,omitempty"`
	// RecoveryThreshold is the success count inside the window that restores
	// the policy (default 3).
	RecoveryThreshold int `json:"recovery_threshold,omitempty"`
}

// BypassConfig is the persisted fault-detection configuration.
type BypassConfig struct {
	// Enabled turns automatic fault detection on or off. A manual force is
	// independent of this switch.
	Enabled bool `json:"enabled"`
	// IntervalSeconds is the probe interval (default 2s).
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
	// TimeoutMilliseconds is the TCP/ICMP connect timeout (default 1000ms).
	TimeoutMilliseconds int `json:"timeout_ms,omitempty"`
	// FailureThreshold is the number of consecutive failed probe rounds that
	// enter bypass (default 3).
	FailureThreshold int `json:"failure_threshold,omitempty"`
	// RecoveryThreshold is the number of consecutive successful probe rounds
	// that leave bypass (default 3).
	RecoveryThreshold int `json:"recovery_threshold,omitempty"`
	// Interfaces replaces the topology-derived interface set when non-empty.
	Interfaces []string `json:"interfaces,omitempty"`
	// TCPTargets are host:port endpoints probed with a TCP connect.
	TCPTargets []string `json:"tcp_targets,omitempty"`
	// ICMPTargets are addresses probed with the system ping binary when
	// available.
	ICMPTargets []string `json:"icmp_targets,omitempty"`
	// Groups enables per-group detection.  When empty the legacy global
	// detector above is used unchanged.
	Groups []BypassGroupConfig `json:"groups,omitempty"`
}

// BypassProbe is one probe result in the current round.
type BypassProbe struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Target    string    `json:"target"`
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// BypassFlowStats is the plugin readback for one passive flow target.
type BypassFlowStats struct {
	Group     string `json:"group,omitempty"`
	DstIP     string `json:"dst_ip"`
	DstPort   int    `json:"dst_port"`
	Protocol  string `json:"protocol"`
	Attempts  uint64 `json:"attempts"`
	Syn       uint64 `json:"syn,omitempty"`
	SynAck    uint64 `json:"synack,omitempty"`
	RST       uint64 `json:"rst,omitempty"`
	Responses uint64 `json:"responses"`
	Success   uint64 `json:"success"`
	Timeouts  uint64 `json:"timeouts"`
	// ICMPUnreachable counts ICMP destination-unreachable messages that quote
	// a request for this target (a definitive failure signal).
	ICMPUnreachable uint64 `json:"icmp_unreachable,omitempty"`
}

// BypassGroupState is one policy group's bypass view.
type BypassGroupState struct {
	Group string `json:"group"`
	// Health is unknown (no traffic in the window), healthy, faulty or bypass.
	Health string `json:"health"`
	// Enabled is true while this group alone bypasses policy evaluation.
	Enabled bool      `json:"enabled"`
	Forced  bool      `json:"forced"`
	Cause   string    `json:"cause,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitempty"`

	ConsecutiveFailures  int       `json:"consecutive_failures"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	LastProbeOK          bool      `json:"last_probe_ok"`
	LastProbeAt          time.Time `json:"last_probe_at,omitempty"`

	Probes []BypassProbe `json:"probes,omitempty"`

	// WindowSeconds and the window counters come from the passive analyzer.
	WindowSeconds float64 `json:"window_seconds,omitempty"`
	FlowAttempts  uint64  `json:"flow_attempts"`
	FlowSuccess   uint64  `json:"flow_success"`
	FlowRST       uint64  `json:"flow_rst"`
	FlowTimeouts  uint64  `json:"flow_timeouts"`
	// FlowStats is the cumulative plugin readback per configured target.
	FlowStats []BypassFlowStats `json:"flow_stats,omitempty"`

	BypassPackets uint64 `json:"bypass_packets,omitempty"`
}

// BypassState is the persisted runtime view exposed by the API and /health.
type BypassState struct {
	// Enabled is true while the data plane is bypassing orchestration policy.
	Enabled bool `json:"enabled"`
	// Forced is true when an operator forced the bypass and automatic
	// recovery is suspended.
	Forced bool      `json:"forced"`
	Cause  string    `json:"cause,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitempty"`

	ConsecutiveFailures  int       `json:"consecutive_failures"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	LastProbeOK          bool      `json:"last_probe_ok"`
	LastProbeAt          time.Time `json:"last_probe_at,omitempty"`

	Probes []BypassProbe `json:"probes,omitempty"`
	Config BypassConfig  `json:"config"`

	// Groups is the per-group view.  The aggregate Enabled above is true when
	// the global bypass or any group bypass is active; a single bypassed group
	// is still a healthy appliance.
	Groups []BypassGroupState `json:"groups,omitempty"`

	// Generation is the committed policy generation that remains configured
	// behind the bypass, and BypassPackets is the plugin counter.
	Generation    string `json:"generation,omitempty"`
	BypassPackets uint64 `json:"bypass_packets,omitempty"`
}

// BypassGroupRuntimeState is the data-plane view of one group bypass.
type BypassGroupRuntimeState struct {
	Enabled bool
	Forced  bool
	Cause   string
	Reason  string
	Packets uint64
}

// BypassRuntimeState is the data-plane view of the bypass exposed to the
// detector, API and health checks without a second VPP round trip.
type BypassRuntimeState struct {
	Enabled    bool
	Forced     bool
	Cause      string
	Reason     string
	Since      time.Time
	Packets    uint64
	Generation string
	Groups     map[string]BypassGroupRuntimeState
	Flow       []BypassFlowStats
	// ProbeTargets is the number of passive targets the plugin reports.
	ProbeTargets int
}

// BypassController is implemented by the control-plane fault detector.
type BypassController interface {
	BypassStatus(context.Context) (BypassState, error)
	SetBypass(context.Context, bool, string) (BypassState, error)
	// SetGroupBypass toggles one group; an empty group keeps the legacy
	// global behaviour of SetBypass.
	SetGroupBypass(context.Context, string, bool, string) (BypassState, error)
	UpdateBypassConfig(context.Context, BypassConfig) (BypassState, error)
}

func (handler *Handler) SetBypassController(controller BypassController) {
	handler.bypass = controller
}

func (handler *Handler) registerBypassRoutes(mux *http.ServeMux) {
	if handler.bypass == nil {
		return
	}
	mux.HandleFunc(BypassPath, handler.handleBypass)
	mux.HandleFunc(BypassConfigPath, handler.handleBypassConfig)
}

func (handler *Handler) handleBypass(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		state, err := handler.bypass.BypassStatus(request.Context())
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, state)
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		var body struct {
			Enabled *bool  `json:"enabled"`
			Group   string `json:"group,omitempty"`
			Reason  string `json:"reason,omitempty"`
		}
		if err := decodeStrictJSON(writer, request, &body); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		if body.Enabled == nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, errors.New("enabled is required"))
			return
		}
		group := strings.TrimSpace(body.Group)
		var state BypassState
		var err error
		if group == "" {
			state, err = handler.bypass.SetBypass(request.Context(), *body.Enabled, body.Reason)
		} else {
			state, err = handler.bypass.SetGroupBypass(request.Context(), group, *body.Enabled, body.Reason)
		}
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		auditTarget := fmt.Sprintf("enabled=%t", *body.Enabled)
		if group != "" {
			auditTarget = fmt.Sprintf("group=%s enabled=%t", group, *body.Enabled)
		}
		handler.recordAudit(request, BypassPath, "update", "success", auditTarget)
		writeJSON(writer, http.StatusOK, state)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleBypassConfig(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		state, err := handler.bypass.BypassStatus(request.Context())
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, state.Config)
	case http.MethodPut:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		var config BypassConfig
		if err := decodeStrictJSON(writer, request, &config); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		state, err := handler.bypass.UpdateBypassConfig(request.Context(), config)
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		handler.recordAudit(request, BypassConfigPath, "update", "success", strings.TrimSpace(state.Reason))
		writeJSON(writer, http.StatusOK, state)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}
