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

	// Generation is the committed policy generation that remains configured
	// behind the bypass, and BypassPackets is the plugin counter.
	Generation    string `json:"generation,omitempty"`
	BypassPackets uint64 `json:"bypass_packets,omitempty"`
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
}

// BypassController is implemented by the control-plane fault detector.
type BypassController interface {
	BypassStatus(context.Context) (BypassState, error)
	SetBypass(context.Context, bool, string) (BypassState, error)
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
		state, err := handler.bypass.SetBypass(request.Context(), *body.Enabled, body.Reason)
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		handler.recordAudit(request, BypassPath, "update", "success", fmt.Sprintf("enabled=%t", *body.Enabled))
		writeJSON(writer, http.StatusOK, state)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleBypassConfig(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
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
}
