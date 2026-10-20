package productupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"ly-route/backend/internal/product"
)

const (
	SchemaVersion         = 1
	ConversionPackageType = "ly-route-conversion"
	UpgradePackageType    = "ly-route-upgrade"
)

var (
	ErrInvalidConversionManifest = errors.New("invalid conversion manifest")
	ErrConversionNotApplicable   = errors.New("conversion is not applicable")
)

// DefaultServiceUnits are the units whose enable/active state must survive a
// product conversion.  The list intentionally excludes application services
// that do not exist in both products.
var DefaultServiceUnits = []string{
	"ly-route-control-api.service",
	"ly-route-vpp-apply.service",
	"ly-route-runtime-check.service",
	"ly-route-recovery.service",
	"ly-route-firstboot.service",
	"vpp.service",
	"nginx.service",
}

type Manifest struct {
	SchemaVersion     int               `json:"schema_version"`
	PackageType       string            `json:"package_type"`
	FromProduct       product.ID        `json:"from_product"`
	ToProduct         product.ID        `json:"to_product"`
	FromVersion       string            `json:"from_version,omitempty"`
	ToVersion         string            `json:"to_version"`
	BaseVersion       string            `json:"base_version"`
	Suite             string            `json:"suite,omitempty"`
	Arch              string            `json:"arch,omitempty"`
	IncludesDataplane bool              `json:"includes_dataplane,omitempty"`
	Dataplane         DataplaneSpec     `json:"dataplane"`
	Config            ConfigSpec        `json:"config"`
	Rollback          RollbackSpec      `json:"rollback"`
	Services          ServicesSpec      `json:"services"`
	Checksums         map[string]string `json:"checksums,omitempty"`
}

type DataplaneSpec struct {
	Preserve       bool     `json:"preserve"`
	VerifyPackages []string `json:"verify_packages,omitempty"`
	RequiredPlugin string   `json:"required_plugin,omitempty"`
}

type ConfigSpec struct {
	Migration        string `json:"migration"`
	PreserveDatabase bool   `json:"preserve_database"`
	SnapshotRequired bool   `json:"snapshot_required"`
}

type RollbackSpec struct {
	Supported      bool `json:"supported"`
	BackupRequired bool `json:"backup_required"`
}

type ServicesSpec struct {
	PreserveEnableState bool     `json:"preserve_enable_state"`
	Units               []string `json:"units,omitempty"`
}

type Step struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Reversible  bool   `json:"reversible"`
}

type Plan struct {
	FromProduct          product.ID `json:"from_product"`
	ToProduct            product.ID `json:"to_product"`
	FromVersion          string     `json:"from_version,omitempty"`
	ToVersion            string     `json:"to_version"`
	BaseVersion          string     `json:"base_version"`
	DataplaneMode        string     `json:"dataplane_mode"`
	VerifyPackages       []string   `json:"verify_packages,omitempty"`
	RequiredPlugin       string     `json:"required_plugin,omitempty"`
	ConfigMigration      string     `json:"config_migration"`
	PreserveDatabase     bool       `json:"preserve_database"`
	SnapshotRequired     bool       `json:"snapshot_required"`
	RollbackSupported    bool       `json:"rollback_supported"`
	BackupRequired       bool       `json:"backup_required"`
	PreserveServiceState bool       `json:"preserve_service_state"`
	ServiceUnits         []string   `json:"service_units"`
	Steps                []Step     `json:"steps"`
	Warnings             []string   `json:"warnings,omitempty"`
}

type Validation struct {
	Valid  bool     `json:"valid"`
	Issues []string `json:"issues,omitempty"`
	Plan   Plan     `json:"plan"`
}

func ParseManifest(data []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidConversionManifest, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: manifest must contain one JSON value", ErrInvalidConversionManifest)
	}
	return manifest, nil
}

// Validate checks a conversion manifest against the running product/version.
// It never mutates the system and is safe for preflight requests.
func (manifest Manifest) Validate(currentProduct product.ID, currentVersion string) Validation {
	var issues []string
	if manifest.SchemaVersion != SchemaVersion {
		issues = append(issues, fmt.Sprintf("schema_version must be %d", SchemaVersion))
	}
	if strings.TrimSpace(manifest.PackageType) != ConversionPackageType {
		issues = append(issues, fmt.Sprintf("package_type must be %s", ConversionPackageType))
	}
	fromProfile, fromErr := product.ParseProfile(manifest.FromProduct.String())
	toProfile, toErr := product.ParseProfile(manifest.ToProduct.String())
	if fromErr != nil {
		issues = append(issues, "from_product must be gateway or orchestrator")
	}
	if toErr != nil {
		issues = append(issues, "to_product must be gateway or orchestrator")
	}
	if fromErr == nil && toErr == nil && fromProfile.ID() == toProfile.ID() {
		issues = append(issues, "from_product and to_product must differ")
	}
	if fromErr == nil && currentProduct.String() != "" && fromProfile.ID() != currentProduct {
		issues = append(issues, fmt.Sprintf("manifest converts from %s, server is %s", fromProfile.ID(), currentProduct))
	}
	if strings.TrimSpace(manifest.ToVersion) == "" {
		issues = append(issues, "to_version is required")
	}
	if strings.TrimSpace(manifest.BaseVersion) == "" {
		issues = append(issues, "base_version is required")
	} else if !sameVersionFamily(currentVersion, manifest.BaseVersion) {
		issues = append(issues, fmt.Sprintf("base_version %q does not match current version %q", manifest.BaseVersion, currentVersion))
	}
	if !manifest.Dataplane.Preserve && !manifest.IncludesDataplane {
		issues = append(issues, "dataplane.preserve must be true unless the payload explicitly includes the dataplane")
	}
	if manifest.IncludesDataplane && !manifest.Dataplane.Preserve && len(manifest.Dataplane.VerifyPackages) == 0 {
		issues = append(issues, "includes_dataplane requires preserve=true or an explicit verify_packages list")
	}
	if manifest.Dataplane.Preserve && len(manifest.Dataplane.VerifyPackages) == 0 {
		issues = append(issues, "dataplane.verify_packages is required when preserve=true")
	}
	switch manifest.Config.Migration {
	case "preserve", "migrate", "reset":
	default:
		issues = append(issues, "config.migration must be preserve, migrate or reset")
	}
	if manifest.Config.Migration != "preserve" && !manifest.Config.SnapshotRequired {
		issues = append(issues, "config.snapshot_required must be true for migrate/reset")
	}
	if !manifest.Rollback.Supported {
		issues = append(issues, "rollback.supported must be true")
	}
	if !manifest.Rollback.BackupRequired {
		issues = append(issues, "rollback.backup_required must be true")
	}
	if !manifest.Services.PreserveEnableState {
		issues = append(issues, "services.preserve_enable_state must be true")
	}
	if len(manifest.Checksums) == 0 {
		issues = append(issues, "checksums are required")
	} else if _, ok := manifest.Checksums["usr/lib/ly-route/ly-route-control"]; !ok {
		issues = append(issues, "checksums must include usr/lib/ly-route/ly-route-control")
	}
	units := append([]string(nil), manifest.Services.Units...)
	if len(units) == 0 {
		units = append(units, DefaultServiceUnits...)
	}
	dataplaneMode := "preserve"
	if manifest.IncludesDataplane {
		dataplaneMode = "preserve+payload"
		if !manifest.Dataplane.Preserve {
			dataplaneMode = "payload"
		}
	}
	plan := Plan{
		FromProduct:          manifest.FromProduct,
		ToProduct:            manifest.ToProduct,
		FromVersion:          manifest.FromVersion,
		ToVersion:            manifest.ToVersion,
		BaseVersion:          manifest.BaseVersion,
		DataplaneMode:        dataplaneMode,
		VerifyPackages:       append([]string(nil), manifest.Dataplane.VerifyPackages...),
		RequiredPlugin:       manifest.Dataplane.RequiredPlugin,
		ConfigMigration:      manifest.Config.Migration,
		PreserveDatabase:     manifest.Config.PreserveDatabase,
		SnapshotRequired:     manifest.Config.SnapshotRequired,
		RollbackSupported:    manifest.Rollback.Supported,
		BackupRequired:       manifest.Rollback.BackupRequired,
		PreserveServiceState: manifest.Services.PreserveEnableState,
		ServiceUnits:         units,
		Steps: []Step{
			{ID: "validate", Description: "validate conversion manifest and product identity", Reversible: true},
			{ID: "snapshot", Description: "create a pre-conversion config snapshot", Reversible: true},
			{ID: "capture_services", Description: "capture systemd enable state for shared services", Reversible: true},
			{ID: "backup", Description: "back up control payload, frontend and configuration", Reversible: true},
			{ID: "install_payload", Description: "install the conversion payload without replacing the base OS", Reversible: true},
			{ID: "migrate_config", Description: "apply the declared configuration migration", Reversible: manifest.Config.Migration == "preserve"},
			{ID: "verify_dataplane", Description: "verify preserved VPP packages and orchestrator plugin", Reversible: false},
			{ID: "health_check", Description: "verify control API health before commit", Reversible: false},
			{ID: "restore_services", Description: "restore captured systemd enable state", Reversible: true},
		},
	}
	if manifest.Config.Migration == "reset" {
		plan.Warnings = append(plan.Warnings, "configuration reset will discard product-specific config; a snapshot is still required")
	}
	return Validation{Valid: len(issues) == 0, Issues: issues, Plan: plan}
}

func sameVersionFamily(current, base string) bool {
	current = normalizeVersion(current)
	base = normalizeVersion(base)
	if current == "" || current == "dev" || base == "" {
		return true
	}
	return current == base
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "v")
	if index := strings.IndexAny(value, "+-"); index >= 0 {
		value = value[:index]
	}
	return value
}
