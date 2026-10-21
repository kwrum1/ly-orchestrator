package productupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/product"
)

func validConversionManifest() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		PackageType:   ConversionPackageType,
		FromProduct:   product.Gateway().ID(),
		ToProduct:     product.Orchestrator().ID(),
		FromVersion:   "26.06+lyroute1",
		ToVersion:     "26.06+lyroute1",
		BaseVersion:   "26.06",
		Suite:         "bookworm",
		Arch:          "amd64",
		Dataplane: DataplaneSpec{
			Preserve:       true,
			VerifyPackages: []string{"vpp", "vpp-plugin-core", "vpp-plugin-dpdk"},
			RequiredPlugin: "ly_route_orchestrator_plugin.so",
		},
		Config:    ConfigSpec{Migration: "preserve", PreserveDatabase: false, SnapshotRequired: true},
		Rollback:  RollbackSpec{Supported: true, BackupRequired: true},
		Services:  ServicesSpec{PreserveEnableState: true, Units: []string{"ly-route-control-api.service", "vpp.service"}},
		Checksums: map[string]string{"usr/lib/ly-route/ly-route-control": strings.Repeat("a", 64)},
	}
}

func TestConversionManifestAcceptsPreservedDataplane(t *testing.T) {
	manifest := validConversionManifest()
	validation := manifest.Validate(product.Gateway().ID(), "26.06+lyroute1")
	if !validation.Valid {
		t.Fatalf("valid manifest issues = %v", validation.Issues)
	}
	if validation.Plan.DataplaneMode != "preserve" || !validation.Plan.PreserveServiceState {
		t.Fatalf("plan = %#v", validation.Plan)
	}
	if len(validation.Plan.Steps) == 0 {
		t.Fatal("plan has no steps")
	}
	if validation.Plan.ServiceUnits[0] != "ly-route-control-api.service" {
		t.Fatalf("service units = %v", validation.Plan.ServiceUnits)
	}
}

func TestConversionManifestRejectsFakeOrMissingDataplane(t *testing.T) {
	manifest := validConversionManifest()
	manifest.Dataplane = DataplaneSpec{}
	validation := manifest.Validate(product.Gateway().ID(), "26.06")
	if validation.Valid || !containsIssue(validation.Issues, "dataplane.preserve") {
		t.Fatalf("missing dataplane validation = %#v", validation)
	}

	manifest = validConversionManifest()
	manifest.Dataplane = DataplaneSpec{Preserve: false}
	manifest.IncludesDataplane = true
	validation = manifest.Validate(product.Gateway().ID(), "26.06")
	if validation.Valid || !containsIssue(validation.Issues, "includes_dataplane requires") {
		t.Fatalf("fake includes_dataplane validation = %#v", validation)
	}

	manifest = validConversionManifest()
	manifest.Dataplane.VerifyPackages = nil
	validation = manifest.Validate(product.Gateway().ID(), "26.06")
	if validation.Valid || !containsIssue(validation.Issues, "verify_packages") {
		t.Fatalf("preserve without verification = %#v", validation)
	}
}

func TestConversionManifestRejectsWrongProductVersionAndResetWithoutSnapshot(t *testing.T) {
	manifest := validConversionManifest()
	manifest.FromProduct = product.Orchestrator().ID()
	validation := manifest.Validate(product.Gateway().ID(), "26.06")
	if validation.Valid || !containsIssue(validation.Issues, "server is gateway") {
		t.Fatalf("wrong from product validation = %#v", validation)
	}

	manifest = validConversionManifest()
	manifest.BaseVersion = "25.10"
	validation = manifest.Validate(product.Gateway().ID(), "26.06+lyroute1")
	if validation.Valid || !containsIssue(validation.Issues, "base_version") {
		t.Fatalf("wrong base version validation = %#v", validation)
	}

	manifest = validConversionManifest()
	manifest.Config.Migration = "reset"
	manifest.Config.SnapshotRequired = false
	validation = manifest.Validate(product.Gateway().ID(), "26.06")
	if validation.Valid || !containsIssue(validation.Issues, "snapshot_required") {
		t.Fatalf("reset without snapshot validation = %#v", validation)
	}
}

func TestConversionManifestParserRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	if _, err := ParseManifest([]byte(`{"schema_version":1,"unknown":true}`)); !errors.Is(err, ErrInvalidConversionManifest) {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := ParseManifest([]byte(`{"schema_version":1}{"schema_version":2}`)); !errors.Is(err, ErrInvalidConversionManifest) {
		t.Fatalf("trailing data error = %v", err)
	}
}

func TestSystemctlControllerCapturesAndRestoresEnableState(t *testing.T) {
	commands := [][]string{}
	controller := SystemctlController{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		unit := args[len(args)-1]
		switch unit {
		case "enabled.service":
			return []byte("enabled\n"), nil
		case "disabled.service":
			return []byte("disabled\n"), nil
		case "masked.service":
			return []byte("masked\n"), nil
		case "missing.service":
			return []byte("Failed to get unit file state: No such file or directory\n"), errors.New("exit status 1")
		default:
			return []byte("static\n"), nil
		}
	}}
	state, err := controller.Capture(context.Background(), []string{"enabled.service", "disabled.service", "masked.service", "static.service", "missing.service"})
	if err != nil {
		t.Fatal(err)
	}
	if state["enabled.service"] != "enabled" || state["disabled.service"] != "disabled" || state["masked.service"] != "masked" || state["static.service"] != "static" || state["missing.service"] != "missing" {
		t.Fatalf("captured state = %#v", state)
	}
	commands = nil
	restore := map[string]string{"enabled.service": "enabled", "disabled.service": "disabled", "masked.service": "masked", "static.service": "static", "missing.service": "missing"}
	if err := controller.Restore(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	for _, command := range commands {
		if len(command) > 1 && command[1] != "is-enabled" {
			mutations++
		}
	}
	if mutations != 0 {
		t.Fatalf("restore commands = %#v, want no-op for unchanged units", commands)
	}
}

func TestSystemctlControllerRestoresChangedStateOnly(t *testing.T) {
	commands := [][]string{}
	controller := SystemctlController{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "is-enabled" {
			switch args[len(args)-1] {
			case "enabled.service":
				return []byte("disabled\n"), nil
			case "disabled.service":
				return []byte("enabled\n"), nil
			case "masked.service":
				return []byte("disabled\n"), nil
			}
		}
		return nil, nil
	}}
	state := map[string]string{"enabled.service": "enabled", "disabled.service": "disabled", "masked.service": "masked", "static.service": "static"}
	if err := controller.Restore(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	for _, command := range commands {
		if len(command) > 1 && command[1] != "is-enabled" {
			mutations++
		}
	}
	if mutations != 3 {
		t.Fatalf("restore commands = %#v, want enable/disable/mask only", commands)
	}
}

func TestPackagedConversionFixtureIsValid(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "packaging", "fixtures", "product-conversion", "gateway-to-orchestrator.json"))
	if err != nil {
		t.Fatalf("read conversion fixture: %v", err)
	}
	manifest, err := ParseManifest(body)
	if err != nil {
		t.Fatalf("parse conversion fixture: %v", err)
	}
	validation := manifest.Validate(product.Gateway().ID(), "26.06+lyroute1")
	if !validation.Valid {
		t.Fatalf("conversion fixture issues = %v", validation.Issues)
	}
	if validation.Plan.DataplaneMode != "preserve" || validation.Plan.ToProduct != product.Orchestrator().ID() {
		t.Fatalf("conversion fixture plan = %#v", validation.Plan)
	}
}

func TestSystemctlControllerTimesOutHungCommands(t *testing.T) {
	controller := SystemctlController{Timeout: 10 * time.Millisecond, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	state, err := controller.Capture(context.Background(), []string{"vpp.service"})
	if err != nil {
		t.Fatal(err)
	}
	if state["vpp.service"] != "missing" {
		t.Fatalf("timed out capture = %#v, want missing", state)
	}
}

func containsIssue(issues []string, fragment string) bool {
	for _, issue := range issues {
		if strings.Contains(issue, fragment) {
			return true
		}
	}
	return false
}
