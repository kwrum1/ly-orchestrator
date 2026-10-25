package orchestratorapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/productupdate"
)

func (handler *Handler) actor(request *http.Request) (string, string) {
	if provider, ok := handler.access.(ActorProvider); ok {
		if actor, role, valid := provider.Actor(request); valid {
			return actor, role
		}
	}
	return "system", "system"
}

func updateTransactionID(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("X-Request-ID")); value != "" {
		return value
	}
	return fmt.Sprintf("orchestrator-update-%d", time.Now().UnixNano())
}

func (handler *Handler) handleUpgrades(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		version, err := handler.updates.Version(request.Context())
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		tasks, err := handler.updates.UpgradeTasks(request.Context())
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"version": version, "items": tasks})
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		if err := request.ParseMultipartForm(1024 << 20); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("upgrade package upload is required: %w", err))
			return
		}
		file, header, err := request.FormFile("package")
		if err != nil {
			file, header, err = request.FormFile("firmware")
		}
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("upgrade package upload is required"))
			return
		}
		defer file.Close()
		actor, role := handler.actor(request)
		task, err := handler.updates.StageUpgrade(request.Context(), UpgradeStageRequest{
			Actor: actor, Role: role, TransactionID: updateTransactionID(request),
			Filename: header.Filename, Package: file,
		})
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"item": task})
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleUpgradeItem(writer http.ResponseWriter, request *http.Request) {
	remainder := strings.Trim(strings.TrimPrefix(request.URL.Path, UpgradesPath+"/"), "/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeAPIError(writer, http.StatusNotFound, ErrTaskNotFound)
		return
	}
	taskID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch request.Method {
	case http.MethodGet:
		if action != "" {
			writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
			return
		}
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		task, err := handler.updates.UpgradeTask(request.Context(), taskID)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"item": task})
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		actor, role := handler.actor(request)
		actionRequest := UpgradeActionRequest{Actor: actor, Role: role, TransactionID: updateTransactionID(request), TaskID: taskID}
		if action == "install" {
			var body struct {
				ConfirmInstall bool `json:"confirm_install"`
				Reboot         bool `json:"reboot"`
			}
			if request.Body != nil {
				if err := decodeStrictJSON(writer, request, &body); err != nil && !errors.Is(err, io.EOF) {
					writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
					return
				}
			}
			actionRequest.Confirm = body.ConfirmInstall
			actionRequest.Reboot = body.Reboot
			task, err := handler.updates.InstallUpgrade(request.Context(), actionRequest)
			if err != nil {
				writeUpdateError(writer, err)
				return
			}
			handler.writeAcceptedUpgrade(writer, request, task, "install", func(ctx context.Context) error {
				return handler.updates.StartUpgrade(ctx, actionRequest)
			})
			return
		}
		if action == "rollback" {
			task, err := handler.updates.RollbackUpgrade(request.Context(), actionRequest)
			if err != nil {
				writeUpdateError(writer, err)
				return
			}
			handler.writeAcceptedUpgrade(writer, request, task, "rollback", func(ctx context.Context) error {
				return handler.updates.StartRollback(ctx, actionRequest)
			})
			return
		}
		writeAPIError(writer, http.StatusNotFound, ErrTaskNotFound)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

type conversionRequest struct {
	Manifest json.RawMessage `json:"manifest"`
	Prepare  bool            `json:"prepare"`
}

func (handler *Handler) handleConversions(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		tasks, err := handler.updates.ConversionTasks(request.Context())
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": tasks})
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		var body conversionRequest
		if err := decodeStrictJSON(writer, request, &body); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		manifest, err := productupdate.ParseManifest(body.Manifest)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		validation, err := handler.updates.ValidateConversion(request.Context(), manifest)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		if !validation.Valid {
			writeJSON(writer, http.StatusUnprocessableEntity, map[string]any{"validation": validation})
			return
		}
		actor, role := handler.actor(request)
		task, err := handler.updates.CreateConversion(request.Context(), ConversionCreateRequest{
			Actor: actor, Role: role, TransactionID: updateTransactionID(request),
			Manifest: manifest, Prepare: body.Prepare,
		})
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"item": task})
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleConversionItem(writer http.ResponseWriter, request *http.Request) {
	remainder := strings.Trim(strings.TrimPrefix(request.URL.Path, ConversionsPath+"/"), "/")
	if remainder == "validate" && request.Method == http.MethodPost {
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		var body conversionRequest
		if err := decodeStrictJSON(writer, request, &body); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		manifest, err := productupdate.ParseManifest(body.Manifest)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		validation, err := handler.updates.ValidateConversion(request.Context(), manifest)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"validation": validation})
		return
	}
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeAPIError(writer, http.StatusNotFound, ErrTaskNotFound)
		return
	}
	taskID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch request.Method {
	case http.MethodGet:
		if action != "" {
			writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
			return
		}
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		task, err := handler.updates.ConversionTask(request.Context(), taskID)
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"item": task})
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		actor, role := handler.actor(request)
		actionRequest := ConversionActionRequest{Actor: actor, Role: role, TransactionID: updateTransactionID(request), TaskID: taskID}
		var task ConversionTask
		var err error
		switch action {
		case "prepare":
			task, err = handler.updates.PrepareConversion(request.Context(), actionRequest)
		case "rollback":
			task, err = handler.updates.RollbackConversion(request.Context(), actionRequest)
		case "apply":
			task, err = handler.updates.ApplyConversion(request.Context(), actionRequest)
		default:
			writeAPIError(writer, http.StatusNotFound, ErrTaskNotFound)
			return
		}
		if err != nil {
			writeUpdateError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"item": task})
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) writeAcceptedUpgrade(writer http.ResponseWriter, request *http.Request, task UpgradeTask, action string, start func(context.Context) error) {
	pollPath := UpgradesPath + "/" + task.ID
	writer.Header().Set("Location", pollPath)
	writer.Header().Set("Retry-After", "2")
	writer.Header().Set("X-LY-Route-Task-ID", task.ID)
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"item": task, "task_id": task.ID, "status": task.State, "action": action, "poll": pollPath,
	})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	base := context.WithoutCancel(request.Context())
	go func() {
		// Give the HTTP response a chance to leave the server before the
		// installer restarts the control API.
		time.Sleep(250 * time.Millisecond)
		ctx, cancel := context.WithTimeout(base, 30*time.Second)
		defer cancel()
		_ = start(ctx)
	}()
}

func writeUpdateError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTaskNotFound):
		writeAPIError(writer, http.StatusNotFound, err)
	case errors.Is(err, ErrTaskConflict), errors.Is(err, productupdate.ErrConversionNotApplicable):
		writeAPIError(writer, http.StatusConflict, err)
	case errors.Is(err, ErrUpgradeUnsupported):
		writeAPIError(writer, http.StatusConflict, err)
	case errors.Is(err, ErrInvalidUpgradePackage):
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
	case errors.Is(err, ErrConversionExecutionBlocked):
		writeAPIError(writer, http.StatusNotImplemented, err)
	case errors.Is(err, productupdate.ErrInvalidConversionManifest):
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
	default:
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
	}
}
