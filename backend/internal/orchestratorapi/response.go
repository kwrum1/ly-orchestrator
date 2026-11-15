package orchestratorapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/productupdate"
	"ly-route/backend/internal/runtime/vpp"
)

type topologyResponse struct {
	Item     orchestrator.TopologyView `json:"item"`
	Checksum string                    `json:"checksum"`
}

type groupsResponse struct {
	Items    []orchestrator.GroupView `json:"items"`
	Checksum string                   `json:"checksum"`
}

type groupResponse struct {
	Item     orchestrator.GroupView `json:"item"`
	Checksum string                 `json:"checksum"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return
	}
}

func writeAPIError(writer http.ResponseWriter, status int, err error) {
	code := codeForError(err)
	if code == "internal_error" && status < 500 {
		// An unmapped 4xx is still a client problem. Reporting it as an internal
		// error made operator input look like an appliance fault.
		code = codeForStatus(status)
	}
	writeJSON(writer, status, errorResponse{Error: errorBody{Code: code, Message: err.Error()}})
}

func codeForStatus(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 405:
		return "method_not_allowed"
	case 409:
		return "conflict"
	case 413:
		return "payload_too_large"
	case 415:
		return "unsupported_media_type"
	case 422:
		return "invalid_request"
	case 429:
		return "rate_limited"
	default:
		return "internal_error"
	}
}

// isPolicyValidationError reports whether err is an operator-input problem that
// must be answered with 422 and its field/reason, never a generic 500.
func isPolicyValidationError(err error) bool {
	for _, target := range []error{
		orchestrator.ErrInvalidPolicyVersion,
		orchestrator.ErrInvalidIPObject,
		orchestrator.ErrDuplicateIPObject,
		orchestrator.ErrInvalidPolicyGroup,
		orchestrator.ErrDuplicatePolicyGroup,
		orchestrator.ErrInvalidPolicyPosition,
		orchestrator.ErrInvalidPolicyRule,
		orchestrator.ErrInvalidRuleSequence,
		orchestrator.ErrDuplicateRuleSequence,
		orchestrator.ErrInvalidPolicyMatch,
		orchestrator.ErrInvalidPolicyAction,
		orchestrator.ErrDeletedPolicyReference,
		orchestrator.ErrGroupInUse,
		orchestrator.ErrPolicyLoop,
		orchestrator.ErrEmptyPolicyDefault,
		orchestrator.ErrInvalidPolicyFlow,
		orchestrator.ErrInvalidPolicyPrelude,
		orchestrator.ErrInvalidPolicyCompile,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func codeForError(err error) string {
	switch {
	case errors.Is(err, orchestrator.ErrInvalidSchemaVersion):
		return "invalid_schema_version"
	case errors.Is(err, orchestrator.ErrMissingLAN):
		return "missing_lan"
	case errors.Is(err, orchestrator.ErrDuplicateLAN):
		return "duplicate_lan"
	case errors.Is(err, orchestrator.ErrMissingWAN):
		return "missing_wan"
	case errors.Is(err, orchestrator.ErrDuplicateWAN):
		return "duplicate_wan"
	case errors.Is(err, orchestrator.ErrGroupSize):
		return "invalid_group_size"
	case errors.Is(err, orchestrator.ErrGroupDirection):
		return "invalid_group_direction"
	case errors.Is(err, orchestrator.ErrGroupBond):
		// Inline group bonds are supported; only reusing a logical LAN/WAN
		// bond as a group port is forbidden.
		return "group_bond_forbidden"
	case errors.Is(err, orchestrator.ErrManagementMembership):
		return "management_interface_forbidden"
	case errors.Is(err, orchestrator.ErrLogicalMembership):
		return "logical_interface_forbidden"
	case errors.Is(err, orchestrator.ErrSharedInterface):
		return "interface_already_owned"
	case errors.Is(err, orchestrator.ErrDuplicateGroup):
		return "duplicate_group"
	case errors.Is(err, orchestrator.ErrGroupInUse):
		return "group_in_use"
	case errors.Is(err, orchestrator.ErrTopologyNotFound):
		return "topology_not_found"
	case errors.Is(err, orchestrator.ErrPolicyNotFound):
		// A fresh appliance has no policy yet; that is a normal state, not an
		// internal error the console should paint as a failure.
		return "policy_not_found"
	case errors.Is(err, orchestrator.ErrTopologyConflict):
		return "topology_conflict"
	case errors.Is(err, orchestrator.ErrGroupNotFound):
		return "group_not_found"
	case errors.Is(err, ErrAuthenticationRequired):
		return "unauthorized"
	case errors.Is(err, ErrPasswordChangeRequired):
		return "password_change_required"
	case errors.Is(err, ErrAdminRequired):
		return "forbidden"
	case errors.Is(err, orchestrator.ErrInvalidName), errors.Is(err, orchestrator.ErrInvalidInterface), errors.Is(err, orchestrator.ErrInvalidBond), errors.Is(err, orchestrator.ErrInvalidPortRole):
		return "invalid_topology"
	case isPolicyValidationError(err):
		return "invalid_policy"
	case errors.Is(err, ErrTaskNotFound):
		return "task_not_found"
	case errors.Is(err, ErrTaskConflict):
		return "task_conflict"
	case errors.Is(err, ErrUpgradeUnsupported):
		return "upgrade_unsupported"
	case errors.Is(err, ErrInvalidUpgradePackage):
		return "invalid_upgrade_package"
	case errors.Is(err, ErrConversionExecutionBlocked):
		return "conversion_execution_blocked"
	case errors.Is(err, productupdate.ErrInvalidConversionManifest):
		return "invalid_conversion_manifest"
	case errors.Is(err, productupdate.ErrConversionNotApplicable):
		return "conversion_not_applicable"
	default:
		var locked *vpp.DataplaneLockedError
		if errors.As(err, &locked) {
			return "prerequisite_unmet"
		}
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			return "unknown_field"
		}
		if strings.Contains(strings.ToLower(err.Error()), "invalid json") {
			return "invalid_json"
		}
		return "internal_error"
	}
}

// ErrTransparentApplyInProgress marks a read that arrived while an apply holds
// the dataplane mutex. It is a transient state, not a failure: an appliance that
// is genuinely applying configuration must not be reported as degraded.
var ErrTransparentApplyInProgress = errors.New("transparent orchestrator runtime is applying")

// TopologyNotConfiguredReason is the operator-facing explanation for a fresh
// appliance that has no saved topology. It is intentionally actionable: the
// fix is to assign LAN and WAN ports on the 网卡设置 page and apply, so the
// message must not read like a hardware fault.
const TopologyNotConfiguredReason = "当前未配置 LAN 接口与 WAN 接口"
