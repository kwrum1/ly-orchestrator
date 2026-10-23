package orchestratorapi

import (
	"errors"
	"net/http"
)

var (
	ErrAccessControllerUnavailable = errors.New("orchestrator API access controller is unavailable")
	ErrAuthenticationRequired      = errors.New("authentication required")
	ErrAdminRequired               = errors.New("admin role required")
	ErrPasswordChangeRequired      = errors.New("password change is required before continuing")
)

type Permission string

const (
	PermissionRead       Permission = "read"
	PermissionAdminWrite Permission = "admin_write"
)

type AccessController interface {
	Authorize(*http.Request, Permission) error
}

func (handler *Handler) authorize(writer http.ResponseWriter, request *http.Request, permission Permission) bool {
	if err := handler.access.Authorize(request, permission); err != nil {
		switch {
		case errors.Is(err, ErrAuthenticationRequired):
			writeAPIError(writer, http.StatusUnauthorized, err)
		case errors.Is(err, ErrAdminRequired), errors.Is(err, ErrPasswordChangeRequired):
			writeAPIError(writer, http.StatusForbidden, err)
		default:
			writeAPIError(writer, http.StatusForbidden, ErrAdminRequired)
		}
		return false
	}
	return true
}

// AuditRecorder is implemented by access controllers that can persist the
// actor and request ID for orchestrator mutations.  It is optional so the
// standalone handler tests can keep using a minimal AccessController.
type AuditRecorder interface {
	RecordAudit(*http.Request, string, string, string, string)
}
