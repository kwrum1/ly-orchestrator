package orchestratorapi

import (
	"errors"
	"net/http"
	"time"
)

const OperationsPath = "/api/v1/orchestrator/operations/current"

type OperationState struct {
	Name      string     `json:"name"`
	Resource  string     `json:"resource,omitempty"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

type OperationTracker interface {
	BeginOperation(name, resource string)
	EndOperation()
}

type OperationProvider interface {
	CurrentOperation() OperationState
}

func (handler *Handler) SetOperationTracker(tracker OperationTracker) {
	handler.operations = tracker
}

func (handler *Handler) handleCurrentOperation(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, PermissionRead) {
		return
	}
	provider, ok := handler.operations.(OperationProvider)
	if !ok {
		writeJSON(writer, http.StatusOK, map[string]any{"operation": OperationState{State: "idle"}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"operation": provider.CurrentOperation()})
}
