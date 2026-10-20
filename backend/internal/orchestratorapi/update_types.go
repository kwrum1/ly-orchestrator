package orchestratorapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"ly-route/backend/internal/product"
	"ly-route/backend/internal/productupdate"
)

const (
	UpgradesPath    = "/api/v1/orchestrator/upgrades"
	ConversionsPath = "/api/v1/orchestrator/conversions"
)

const (
	UpgradeStateStaged         = "staged"
	UpgradeStateInstalling     = "installing"
	UpgradeStateInstalled      = "installed"
	UpgradeStateFailed         = "failed"
	UpgradeStateRollingBack    = "rolling_back"
	UpgradeStateRolledBack     = "rolled_back"
	UpgradeStateRollbackFailed = "rollback_failed"
)

const (
	ConversionStateValidated  = "validated"
	ConversionStateReady      = "ready"
	ConversionStateApplying   = "applying"
	ConversionStateApplied    = "applied"
	ConversionStateFailed     = "failed"
	ConversionStateRolledBack = "rolled_back"
)

var (
	ErrTaskNotFound               = errors.New("product task not found")
	ErrTaskConflict               = errors.New("product task state conflict")
	ErrUpgradeUnsupported         = errors.New("orchestrator upgrade is not supported")
	ErrConversionExecutionBlocked = errors.New("conversion execution is not implemented")
)

type ActorProvider interface {
	Actor(*http.Request) (string, string, bool)
}

type UpdateService interface {
	Version(context.Context) (UpgradeVersion, error)
	UpgradeTasks(context.Context) ([]UpgradeTask, error)
	UpgradeTask(context.Context, string) (UpgradeTask, error)
	StageUpgrade(context.Context, UpgradeStageRequest) (UpgradeTask, error)
	InstallUpgrade(context.Context, UpgradeActionRequest) (UpgradeTask, error)
	RollbackUpgrade(context.Context, UpgradeActionRequest) (UpgradeTask, error)

	ValidateConversion(context.Context, productupdate.Manifest) (productupdate.Validation, error)
	ConversionTasks(context.Context) ([]ConversionTask, error)
	ConversionTask(context.Context, string) (ConversionTask, error)
	CreateConversion(context.Context, ConversionCreateRequest) (ConversionTask, error)
	PrepareConversion(context.Context, ConversionActionRequest) (ConversionTask, error)
	RollbackConversion(context.Context, ConversionActionRequest) (ConversionTask, error)
	ApplyConversion(context.Context, ConversionActionRequest) (ConversionTask, error)
}

type UpgradeVersion struct {
	Product          product.ID `json:"product"`
	Version          string     `json:"version"`
	Commit           string     `json:"commit,omitempty"`
	Suite            string     `json:"suite,omitempty"`
	Arch             string     `json:"arch,omitempty"`
	UpgradeSupported bool       `json:"upgrade_supported"`
}

type UpgradeTask struct {
	ID            string     `json:"id"`
	Product       product.ID `json:"product"`
	FromVersion   string     `json:"from_version,omitempty"`
	ToVersion     string     `json:"to_version"`
	State         string     `json:"state"`
	PackageName   string     `json:"package_name,omitempty"`
	PackageSHA256 string     `json:"package_sha256"`
	PackageSize   int64      `json:"package_size"`
	Services      []string   `json:"services,omitempty"`
	Actor         string     `json:"actor,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	BackupID      string     `json:"backup_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type UpgradeStageRequest struct {
	Actor         string
	Role          string
	TransactionID string
	Filename      string
	Package       io.Reader
}

type UpgradeActionRequest struct {
	Actor         string
	Role          string
	TransactionID string
	TaskID        string
	Confirm       bool
	Reboot        bool
}

type ConversionTask struct {
	ID           string             `json:"id"`
	State        string             `json:"state"`
	FromProduct  product.ID         `json:"from_product"`
	ToProduct    product.ID         `json:"to_product"`
	FromVersion  string             `json:"from_version,omitempty"`
	ToVersion    string             `json:"to_version"`
	BaseVersion  string             `json:"base_version"`
	Plan         productupdate.Plan `json:"plan"`
	Issues       []string           `json:"issues,omitempty"`
	SnapshotID   string             `json:"snapshot_id,omitempty"`
	ServiceState map[string]string  `json:"service_state,omitempty"`
	Actor        string             `json:"actor,omitempty"`
	Reason       string             `json:"reason,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

type ConversionCreateRequest struct {
	Actor         string
	Role          string
	TransactionID string
	Manifest      productupdate.Manifest
	Prepare       bool
}

type ConversionActionRequest struct {
	Actor         string
	Role          string
	TransactionID string
	TaskID        string
}
