package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/productupdate"
)

const maxUpgradePackageBytes int64 = 1 << 30

type productUpdateService struct {
	server *Server
}

func newOrchestratorUpdateService(server *Server) *productUpdateService {
	return &productUpdateService{server: server}
}

type upgradeTaskPayload struct {
	Task        orchestratorapi.UpgradeTask `json:"task"`
	PackagePath string                      `json:"package_path,omitempty"`
	ResultPath  string                      `json:"result_path,omitempty"`
	BackupDir   string                      `json:"backup_dir,omitempty"`
	Started     bool                        `json:"started,omitempty"`
}

type conversionTaskPayload struct {
	Task orchestratorapi.ConversionTask `json:"task"`
}

type updateResult struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

func (service *productUpdateService) Version(context.Context) (orchestratorapi.UpgradeVersion, error) {
	server := service.server
	return orchestratorapi.UpgradeVersion{
		Product:          server.profile.ID(),
		Version:          server.version,
		Commit:           envOrDefault("LY_ROUTE_COMMIT", envOrDefault("LY_ROUTE_GIT_COMMIT", "")),
		Suite:            envOrDefault("LY_ROUTE_SUITE", "bookworm"),
		Arch:             goruntime.GOARCH,
		UpgradeSupported: server.profile.ID() == product.Orchestrator().ID(),
	}, nil
}

func (service *productUpdateService) UpgradeTasks(ctx context.Context) ([]orchestratorapi.UpgradeTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	if service.server.store == nil {
		return nil, errors.New("orchestrator upgrade store is unavailable")
	}
	if err := service.reconcileUpgradeTasks(ctx); err != nil {
		return nil, err
	}
	records, err := service.server.store.ProductTasks(ctx, persistence.ProductTaskUpgrade)
	if err != nil {
		return nil, err
	}
	tasks := make([]orchestratorapi.UpgradeTask, 0, len(records))
	for _, record := range records {
		var payload upgradeTaskPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode upgrade task %s: %w", record.ID, err)
		}
		tasks = append(tasks, payload.Task)
	}
	return tasks, nil
}

func (service *productUpdateService) UpgradeTask(ctx context.Context, id string) (orchestratorapi.UpgradeTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	if err := service.reconcileUpgradeTasks(ctx); err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	payload, err := service.loadUpgradeTask(ctx, id)
	if err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	return payload.Task, nil
}

func (service *productUpdateService) StageUpgrade(ctx context.Context, request orchestratorapi.UpgradeStageRequest) (orchestratorapi.UpgradeTask, error) {
	server := service.server
	if server.store == nil {
		return orchestratorapi.UpgradeTask{}, errors.New("orchestrator upgrade store is unavailable")
	}
	if server.profile.ID() != product.Orchestrator().ID() {
		return orchestratorapi.UpgradeTask{}, orchestratorapi.ErrUpgradeUnsupported
	}
	stageDir := strings.TrimSpace(server.orchestratorUpgradeDir)
	if stageDir == "" {
		stageDir = "/var/lib/ly-route/orchestrator/upgrades"
	}
	if err := os.MkdirAll(stageDir, 0o750); err != nil {
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("create upgrade staging directory: %w", err)
	}
	taskID := "upgrade-" + newRequestID()
	file, err := os.CreateTemp(stageDir, taskID+"-*.tar.zst")
	if err != nil {
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("create upgrade staging file: %w", err)
	}
	packagePath := file.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(packagePath)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(request.Package, maxUpgradePackageBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		err = fmt.Errorf("stage upgrade package: %w", errors.Join(copyErr, closeErr))
		return orchestratorapi.UpgradeTask{}, err
	}
	if written > maxUpgradePackageBytes {
		err = errors.New("upgrade package exceeds size limit")
		return orchestratorapi.UpgradeTask{}, err
	}
	manifest, inspectErr := inspectUpgradePackage(packagePath, product.Orchestrator().ID())
	if inspectErr != nil {
		err = fmt.Errorf("%w: %s", orchestratorapi.ErrInvalidUpgradePackage, sanitizeUpgradeError(inspectErr))
		server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "stage", "failure", err.Error(), request.TransactionID)
		return orchestratorapi.UpgradeTask{}, err
	}
	toVersion := strings.TrimSpace(manifest.Version)
	if toVersion == "" {
		toVersion = strings.TrimSpace(manifest.ToVersion)
	}
	if toVersion == "" {
		toVersion = strings.TrimSpace(manifest.Commit)
	}
	if toVersion == "" {
		toVersion = "unknown"
	}
	now := server.now().UTC()
	task := orchestratorapi.UpgradeTask{
		ID: taskID, Product: product.Orchestrator().ID(),
		FromVersion: server.version, ToVersion: toVersion,
		State:       orchestratorapi.UpgradeStateStaged,
		PackageName: filepath.Base(request.Filename), PackageSHA256: hex.EncodeToString(hash.Sum(nil)),
		PackageSize: written, Services: append([]string(nil), manifest.Services...),
		Actor: request.Actor, CreatedAt: now, UpdatedAt: now,
	}
	payload := upgradeTaskPayload{Task: task, PackagePath: packagePath}
	if persistErr := service.saveUpgradeTask(ctx, payload); persistErr != nil {
		err = persistErr
		server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "stage", "failure", persistErr.Error(), request.TransactionID)
		return orchestratorapi.UpgradeTask{}, persistErr
	}
	server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "stage", "success", "", request.TransactionID)
	return task, nil
}

func (service *productUpdateService) InstallUpgrade(ctx context.Context, request orchestratorapi.UpgradeActionRequest) (orchestratorapi.UpgradeTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	if !request.Confirm {
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("%w: install requires confirm_install=true", orchestratorapi.ErrTaskConflict)
	}
	payload, err := service.loadUpgradeTask(ctx, request.TaskID)
	if err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	switch payload.Task.State {
	case orchestratorapi.UpgradeStateStaged, orchestratorapi.UpgradeStateFailed:
	case orchestratorapi.UpgradeStateInstalling, orchestratorapi.UpgradeStateRollingBack:
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	default:
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	}
	if payload.PackagePath == "" {
		return orchestratorapi.UpgradeTask{}, errors.New("staged upgrade package is missing")
	}
	stageDir := strings.TrimSpace(service.server.orchestratorUpgradeDir)
	if stageDir == "" {
		stageDir = filepath.Dir(payload.PackagePath)
	}
	payload.BackupDir = filepath.Join(stageDir, "backups", payload.Task.ID)
	payload.ResultPath = filepath.Join(stageDir, "results", payload.Task.ID+".json")
	if err := os.MkdirAll(payload.BackupDir, 0o700); err != nil {
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("create upgrade backup directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(payload.ResultPath), 0o750); err != nil {
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("create upgrade result directory: %w", err)
	}
	payload.Task.State = orchestratorapi.UpgradeStateInstalling
	payload.Task.BackupID = payload.Task.ID
	payload.Task.Reason = ""
	payload.Started = false
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveUpgradeTask(ctx, payload); err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "install", "accepted", "", request.TransactionID)
	return payload.Task, nil
}

func (service *productUpdateService) StartUpgrade(ctx context.Context, request orchestratorapi.UpgradeActionRequest) error {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	payload, err := service.loadUpgradeTask(ctx, request.TaskID)
	if err != nil {
		return err
	}
	if payload.Task.State != orchestratorapi.UpgradeStateInstalling {
		return fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	}
	if payload.Started {
		return nil
	}
	payload.Started = true
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveUpgradeTask(ctx, payload); err != nil {
		return err
	}
	invocation := orchestratorUpgradeScriptInvocation("install", payload.PackagePath, payload.Task.PackageSHA256, "/usr/lib/ly-route", payload.BackupDir, payload.ResultPath, request.Reboot)
	invocation.Unit = orchestratorUpgradeUnitName(payload.Task.ID)
	if err := service.server.orchestratorInstallStart(invocation); err != nil {
		payload.Task.State = orchestratorapi.UpgradeStateFailed
		payload.Task.Reason = sanitizeUpgradeError(err)
		payload.Started = false
		payload.Task.UpdatedAt = service.server.now().UTC()
		_ = service.saveUpgradeTask(context.WithoutCancel(ctx), payload)
		service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "install", "failure", payload.Task.Reason, request.TransactionID)
		return err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "install", "success", "", request.TransactionID)
	return nil
}

func (service *productUpdateService) RollbackUpgrade(ctx context.Context, request orchestratorapi.UpgradeActionRequest) (orchestratorapi.UpgradeTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	payload, err := service.loadUpgradeTask(ctx, request.TaskID)
	if err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	switch payload.Task.State {
	case orchestratorapi.UpgradeStateInstalled, orchestratorapi.UpgradeStateFailed, orchestratorapi.UpgradeStateRollbackFailed:
	case orchestratorapi.UpgradeStateInstalling, orchestratorapi.UpgradeStateRollingBack:
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	default:
		return orchestratorapi.UpgradeTask{}, fmt.Errorf("%w: task has no installed payload to roll back", orchestratorapi.ErrTaskConflict)
	}
	if payload.BackupDir == "" {
		return orchestratorapi.UpgradeTask{}, errors.New("upgrade backup directory is unavailable")
	}
	payload.Task.State = orchestratorapi.UpgradeStateRollingBack
	payload.Task.Reason = ""
	payload.Started = false
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveUpgradeTask(ctx, payload); err != nil {
		return orchestratorapi.UpgradeTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "rollback", "accepted", "", request.TransactionID)
	return payload.Task, nil
}

func (service *productUpdateService) StartRollback(ctx context.Context, request orchestratorapi.UpgradeActionRequest) error {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	payload, err := service.loadUpgradeTask(ctx, request.TaskID)
	if err != nil {
		return err
	}
	if payload.Task.State != orchestratorapi.UpgradeStateRollingBack {
		return fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	}
	if payload.Started {
		return nil
	}
	payload.Started = true
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveUpgradeTask(ctx, payload); err != nil {
		return err
	}
	invocation := orchestratorUpgradeScriptInvocation("rollback", payload.PackagePath, payload.Task.PackageSHA256, "/usr/lib/ly-route", payload.BackupDir, payload.ResultPath, false)
	invocation.Unit = orchestratorUpgradeUnitName(payload.Task.ID) + "-rollback"
	if err := service.server.orchestratorInstallStart(invocation); err != nil {
		payload.Task.State = orchestratorapi.UpgradeStateRollbackFailed
		payload.Task.Reason = sanitizeUpgradeError(err)
		payload.Started = false
		payload.Task.UpdatedAt = service.server.now().UTC()
		_ = service.saveUpgradeTask(context.WithoutCancel(ctx), payload)
		service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "rollback", "failure", payload.Task.Reason, request.TransactionID)
		return err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.UpgradesPath, "rollback", "success", "", request.TransactionID)
	return nil
}

func (service *productUpdateService) loadUpgradeTask(ctx context.Context, id string) (upgradeTaskPayload, error) {
	record, err := service.server.store.ProductTask(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrProductTaskNotFound) {
			return upgradeTaskPayload{}, orchestratorapi.ErrTaskNotFound
		}
		return upgradeTaskPayload{}, err
	}
	if record.TaskType != persistence.ProductTaskUpgrade {
		return upgradeTaskPayload{}, orchestratorapi.ErrTaskNotFound
	}
	var payload upgradeTaskPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return upgradeTaskPayload{}, fmt.Errorf("decode upgrade task: %w", err)
	}
	return payload, nil
}

func (service *productUpdateService) saveUpgradeTask(ctx context.Context, payload upgradeTaskPayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode upgrade task: %w", err)
	}
	return service.server.store.SaveProductTask(ctx, persistence.ProductTask{
		ID: payload.Task.ID, Product: product.Orchestrator().ID(), TaskType: persistence.ProductTaskUpgrade,
		State: payload.Task.State, Payload: raw, CreatedAt: payload.Task.CreatedAt, UpdatedAt: payload.Task.UpdatedAt,
	})
}

func (service *productUpdateService) reconcileUpgradeTasks(ctx context.Context) error {
	if service.server.store == nil {
		return errors.New("orchestrator upgrade store is unavailable")
	}
	records, err := service.server.store.ProductTasks(ctx, persistence.ProductTaskUpgrade)
	if err != nil {
		return err
	}
	for _, record := range records {
		var payload upgradeTaskPayload
		if json.Unmarshal(record.Payload, &payload) != nil {
			continue
		}
		if payload.ResultPath == "" || (payload.Task.State != orchestratorapi.UpgradeStateInstalling && payload.Task.State != orchestratorapi.UpgradeStateRollingBack) {
			continue
		}
		if age := service.server.now().UTC().Sub(payload.Task.UpdatedAt); age > 30*time.Second {
			originalState := payload.Task.State
			if !payload.Started {
				payload.Task.State = orchestratorapi.UpgradeStateFailed
				if originalState == orchestratorapi.UpgradeStateRollingBack {
					payload.Task.State = orchestratorapi.UpgradeStateRollbackFailed
				}
				payload.Task.Reason = "installer was accepted but did not start before the control process restarted"
				payload.Task.UpdatedAt = service.server.now().UTC()
				_ = service.saveUpgradeTask(ctx, payload)
				continue
			}
			if age > 15*time.Minute {
				payload.Task.State = orchestratorapi.UpgradeStateFailed
				if originalState == orchestratorapi.UpgradeStateRollingBack {
					payload.Task.State = orchestratorapi.UpgradeStateRollbackFailed
				}
				payload.Task.Reason = "installer did not report a result"
				payload.Task.UpdatedAt = service.server.now().UTC()
				_ = service.saveUpgradeTask(ctx, payload)
				continue
			}
		}
		body, readErr := os.ReadFile(payload.ResultPath)
		if readErr != nil {
			continue
		}
		var result updateResult
		if json.Unmarshal(body, &result) != nil {
			result = updateResult{State: "failed", Reason: "upgrade result file is malformed"}
		}
		next := payload.Task.State
		switch payload.Task.State {
		case orchestratorapi.UpgradeStateInstalling:
			switch result.State {
			case orchestratorapi.UpgradeStateInstalled, orchestratorapi.UpgradeStateFailed:
				next = result.State
			}
		case orchestratorapi.UpgradeStateRollingBack:
			switch result.State {
			case orchestratorapi.UpgradeStateRolledBack, orchestratorapi.UpgradeStateRollbackFailed:
				next = result.State
			}
		}
		if next == payload.Task.State && result.Reason == "" {
			continue
		}
		payload.Task.State = next
		payload.Task.Reason = result.Reason
		payload.Task.UpdatedAt = service.server.now().UTC()
		for attempt := 0; attempt < 3; attempt++ {
			if err := service.saveUpgradeTask(ctx, payload); err == nil {
				break
			}
		}
	}
	return nil
}

func sanitizeUpgradeError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for _, replacement := range []struct{ old, new string }{
		{"signal: killed", "upgrade validation process was terminated"},
		{"signal: terminated", "upgrade process was terminated"},
		{"context canceled", "upgrade operation was canceled"},
	} {
		text = strings.ReplaceAll(text, replacement.old, replacement.new)
	}
	return text
}

func (service *productUpdateService) ValidateConversion(_ context.Context, manifest productupdate.Manifest) (productupdate.Validation, error) {
	return manifest.Validate(service.server.profile.ID(), service.server.version), nil
}

func (service *productUpdateService) CreateConversion(ctx context.Context, request orchestratorapi.ConversionCreateRequest) (orchestratorapi.ConversionTask, error) {
	validation := request.Manifest.Validate(service.server.profile.ID(), service.server.version)
	if !validation.Valid {
		return orchestratorapi.ConversionTask{}, fmt.Errorf("%w: %s", productupdate.ErrConversionNotApplicable, strings.Join(validation.Issues, "; "))
	}
	now := service.server.now().UTC()
	task := orchestratorapi.ConversionTask{
		ID: "conversion-" + newRequestID(), State: orchestratorapi.ConversionStateValidated,
		FromProduct: request.Manifest.FromProduct, ToProduct: request.Manifest.ToProduct,
		FromVersion: request.Manifest.FromVersion, ToVersion: request.Manifest.ToVersion,
		BaseVersion: request.Manifest.BaseVersion, Plan: validation.Plan, Issues: validation.Issues,
		Actor: request.Actor, CreatedAt: now, UpdatedAt: now,
	}
	payload := conversionTaskPayload{Task: task}
	if err := service.saveConversionTask(ctx, payload); err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.ConversionsPath, "validate", "success", "", request.TransactionID)
	if request.Prepare {
		return service.PrepareConversion(ctx, orchestratorapi.ConversionActionRequest{Actor: request.Actor, Role: request.Role, TransactionID: request.TransactionID, TaskID: task.ID})
	}
	return task, nil
}

func (service *productUpdateService) ConversionTasks(ctx context.Context) ([]orchestratorapi.ConversionTask, error) {
	if service.server.store == nil {
		return nil, errors.New("conversion task store is unavailable")
	}
	records, err := service.server.store.ProductTasks(ctx, persistence.ProductTaskConversion)
	if err != nil {
		return nil, err
	}
	tasks := make([]orchestratorapi.ConversionTask, 0, len(records))
	for _, record := range records {
		var payload conversionTaskPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode conversion task %s: %w", record.ID, err)
		}
		tasks = append(tasks, payload.Task)
	}
	return tasks, nil
}

func (service *productUpdateService) ConversionTask(ctx context.Context, id string) (orchestratorapi.ConversionTask, error) {
	payload, err := service.loadConversionTask(ctx, id)
	if err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	return payload.Task, nil
}

func (service *productUpdateService) PrepareConversion(ctx context.Context, request orchestratorapi.ConversionActionRequest) (orchestratorapi.ConversionTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	payload, err := service.loadConversionTask(ctx, request.TaskID)
	if err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	if payload.Task.State != orchestratorapi.ConversionStateValidated && payload.Task.State != orchestratorapi.ConversionStateFailed {
		return orchestratorapi.ConversionTask{}, fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	}
	snapshot, err := service.server.createConfigSnapshot(ctx, "conversion-"+payload.Task.ID, "conversion")
	if err != nil {
		return orchestratorapi.ConversionTask{}, fmt.Errorf("create conversion snapshot: %w", err)
	}
	if service.server.conversionServiceState == nil {
		return orchestratorapi.ConversionTask{}, errors.New("conversion service state controller is unavailable")
	}
	state, err := service.server.conversionServiceState.Capture(ctx, payload.Task.Plan.ServiceUnits)
	if err != nil {
		return orchestratorapi.ConversionTask{}, fmt.Errorf("capture service enable state: %w", err)
	}
	payload.Task.SnapshotID = snapshot.ID
	payload.Task.ServiceState = state
	payload.Task.State = orchestratorapi.ConversionStateReady
	payload.Task.Reason = ""
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveConversionTask(ctx, payload); err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.ConversionsPath, "prepare", "success", snapshot.ID, request.TransactionID)
	return payload.Task, nil
}

func (service *productUpdateService) RollbackConversion(ctx context.Context, request orchestratorapi.ConversionActionRequest) (orchestratorapi.ConversionTask, error) {
	service.server.productTaskMu.Lock()
	defer service.server.productTaskMu.Unlock()
	payload, err := service.loadConversionTask(ctx, request.TaskID)
	if err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	if len(payload.Task.ServiceState) == 0 {
		return orchestratorapi.ConversionTask{}, fmt.Errorf("%w: conversion has no captured service state", orchestratorapi.ErrTaskConflict)
	}
	switch payload.Task.State {
	case orchestratorapi.ConversionStateReady, orchestratorapi.ConversionStateApplying, orchestratorapi.ConversionStateApplied, orchestratorapi.ConversionStateFailed:
	default:
		return orchestratorapi.ConversionTask{}, fmt.Errorf("%w: task is %s", orchestratorapi.ErrTaskConflict, payload.Task.State)
	}
	if service.server.conversionServiceState == nil {
		return orchestratorapi.ConversionTask{}, errors.New("conversion service state controller is unavailable")
	}
	if err := service.server.conversionServiceState.Restore(ctx, payload.Task.ServiceState); err != nil {
		payload.Task.State = orchestratorapi.ConversionStateFailed
		payload.Task.Reason = err.Error()
		payload.Task.UpdatedAt = service.server.now().UTC()
		_ = service.saveConversionTask(context.WithoutCancel(ctx), payload)
		service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.ConversionsPath, "rollback", "failure", err.Error(), request.TransactionID)
		return orchestratorapi.ConversionTask{}, err
	}
	payload.Task.State = orchestratorapi.ConversionStateRolledBack
	payload.Task.Reason = "service enable state restored; config snapshot " + payload.Task.SnapshotID
	payload.Task.UpdatedAt = service.server.now().UTC()
	if err := service.saveConversionTask(ctx, payload); err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.ConversionsPath, "rollback", "success", payload.Task.Reason, request.TransactionID)
	return payload.Task, nil
}

func (service *productUpdateService) ApplyConversion(ctx context.Context, request orchestratorapi.ConversionActionRequest) (orchestratorapi.ConversionTask, error) {
	if _, err := service.loadConversionTask(ctx, request.TaskID); err != nil {
		return orchestratorapi.ConversionTask{}, err
	}
	service.server.recordAuditWithTransaction(request.Actor, request.Role, orchestratorapi.ConversionsPath, "apply", "failure", "conversion execution is a backend skeleton in this release", request.TransactionID)
	return orchestratorapi.ConversionTask{}, orchestratorapi.ErrConversionExecutionBlocked
}

func (service *productUpdateService) loadConversionTask(ctx context.Context, id string) (conversionTaskPayload, error) {
	record, err := service.server.store.ProductTask(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrProductTaskNotFound) {
			return conversionTaskPayload{}, orchestratorapi.ErrTaskNotFound
		}
		return conversionTaskPayload{}, err
	}
	if record.TaskType != persistence.ProductTaskConversion {
		return conversionTaskPayload{}, orchestratorapi.ErrTaskNotFound
	}
	var payload conversionTaskPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return conversionTaskPayload{}, fmt.Errorf("decode conversion task: %w", err)
	}
	return payload, nil
}

func (service *productUpdateService) saveConversionTask(ctx context.Context, payload conversionTaskPayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode conversion task: %w", err)
	}
	return service.server.store.SaveProductTask(ctx, persistence.ProductTask{
		ID: payload.Task.ID, Product: service.server.profile.ID(), TaskType: persistence.ProductTaskConversion,
		State: payload.Task.State, Payload: raw, CreatedAt: payload.Task.CreatedAt, UpdatedAt: payload.Task.UpdatedAt,
	})
}

var _ orchestratorapi.UpdateService = (*productUpdateService)(nil)
