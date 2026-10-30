package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/productupdate"
)

type conversionHTTPRequest struct {
	Manifest json.RawMessage `json:"manifest"`
	Prepare  bool            `json:"prepare"`
}

func (server *Server) registerGatewayConversionRoutes(mux *http.ServeMux) {
	service := newOrchestratorUpdateService(server)
	mux.HandleFunc(orchestratorapi.ConversionsPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !server.requireSession(w, r) {
				return
			}
			tasks, err := service.ConversionTasks(r.Context())
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": tasks, "request_id": requestID(r)})
		case http.MethodPost:
			if !server.requireConversionAdmin(w, r) {
				return
			}
			var body conversionHTTPRequest
			if err := decodeStrictJSON(r, &body); err != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
				return
			}
			manifest, err := productupdate.ParseManifest(body.Manifest)
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			validation, err := service.ValidateConversion(r.Context(), manifest)
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			if !validation.Valid {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"validation": validation, "request_id": requestID(r)})
				return
			}
			session, _ := server.sessionFromRequest(r)
			task, err := service.CreateConversion(r.Context(), orchestratorapi.ConversionCreateRequest{
				Actor: session.Username, Role: session.Role, TransactionID: requestID(r), Manifest: manifest, Prepare: body.Prepare,
			})
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"item": task, "request_id": requestID(r)})
		default:
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
	})
	mux.HandleFunc(orchestratorapi.ConversionsPath+"/", func(w http.ResponseWriter, r *http.Request) {
		remainder := strings.Trim(strings.TrimPrefix(r.URL.Path, orchestratorapi.ConversionsPath+"/"), "/")
		service := newOrchestratorUpdateService(server)
		if remainder == "validate" && r.Method == http.MethodPost {
			if !server.requireSession(w, r) {
				return
			}
			var body conversionHTTPRequest
			if err := decodeStrictJSON(r, &body); err != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
				return
			}
			manifest, err := productupdate.ParseManifest(body.Manifest)
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			validation, err := service.ValidateConversion(r.Context(), manifest)
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"validation": validation, "request_id": requestID(r)})
			return
		}
		parts := strings.Split(remainder, "/")
		if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
			writeError(w, r, http.StatusNotFound, "task_not_found", "conversion task not found")
			return
		}
		taskID, action := parts[0], ""
		if len(parts) == 2 {
			action = parts[1]
		}
		switch r.Method {
		case http.MethodGet:
			if action != "" {
				writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
				return
			}
			if !server.requireSession(w, r) {
				return
			}
			task, err := service.ConversionTask(r.Context(), taskID)
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"item": task, "request_id": requestID(r)})
		case http.MethodPost:
			if !server.requireConversionAdmin(w, r) {
				return
			}
			session, _ := server.sessionFromRequest(r)
			request := orchestratorapi.ConversionActionRequest{Actor: session.Username, Role: session.Role, TransactionID: requestID(r), TaskID: taskID}
			var task orchestratorapi.ConversionTask
			var err error
			switch action {
			case "prepare":
				task, err = service.PrepareConversion(r.Context(), request)
			case "rollback":
				task, err = service.RollbackConversion(r.Context(), request)
			case "apply":
				task, err = service.ApplyConversion(r.Context(), request)
			default:
				writeError(w, r, http.StatusNotFound, "task_not_found", "conversion task not found")
				return
			}
			if err != nil {
				writeConversionError(w, r, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"item": task, "request_id": requestID(r)})
		default:
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
	})
}

func (server *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := server.sessionFromRequest(r); !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return false
	}
	return true
}

func (server *Server) requireConversionAdmin(w http.ResponseWriter, r *http.Request) bool {
	session, ok := server.sessionFromRequest(r)
	if !ok {
		server.recordAudit("anonymous", "system", r.URL.Path, "conversion", "denied", "authentication required", r)
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return false
	}
	if session.Role != "admin" {
		server.recordAudit(session.Username, session.Role, r.URL.Path, "conversion", "denied", "readonly mutation denied", r)
		writeError(w, r, http.StatusForbidden, "forbidden", "admin role required")
		return false
	}
	if server.passwordChangeRequired(w, r, session, r.URL.Path, "conversion") {
		return false
	}
	return true
}

func writeConversionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, orchestratorapi.ErrTaskNotFound):
		writeError(w, r, http.StatusNotFound, "task_not_found", err.Error())
	case errors.Is(err, orchestratorapi.ErrTaskConflict), errors.Is(err, productupdate.ErrConversionNotApplicable):
		writeError(w, r, http.StatusConflict, "task_conflict", err.Error())
	case errors.Is(err, orchestratorapi.ErrConversionExecutionBlocked):
		writeError(w, r, http.StatusNotImplemented, "conversion_execution_blocked", err.Error())
	case errors.Is(err, productupdate.ErrInvalidConversionManifest):
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_conversion_manifest", err.Error())
	default:
		writeError(w, r, http.StatusUnprocessableEntity, "conversion_failed", fmt.Sprintf("%v", err))
	}
}
