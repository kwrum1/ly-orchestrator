package httpapi

import (
	"ly-route/backend/internal/orchestratorapi"
)

func (server *Server) BeginOperation(name, resource string) {
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	now := server.now().UTC()
	server.operation = orchestratorapi.OperationState{Name: name, Resource: resource, State: "running", StartedAt: now}
}

func (server *Server) EndOperation() {
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	if server.operation.StartedAt.IsZero() {
		return
	}
	now := server.now().UTC()
	server.operation.State = "complete"
	server.operation.EndedAt = &now
}

func (server *Server) CurrentOperation() orchestratorapi.OperationState {
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	if server.operation.StartedAt.IsZero() {
		return orchestratorapi.OperationState{State: "idle"}
	}
	return server.operation
}

var _ orchestratorapi.OperationTracker = (*Server)(nil)
var _ orchestratorapi.OperationProvider = (*Server)(nil)
