package httpapi

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/thermal"
)

type thermalErrorBody struct {
	Error struct {
		Code         string           `json:"code"`
		Message      string           `json:"message"`
		TemperatureC *float64         `json:"temperature_c"`
		WarnC        *float64         `json:"warn_c"`
		BlockC       *float64         `json:"block_c"`
		Suggestion   string           `json:"suggestion"`
		Thermal      thermal.Snapshot `json:"thermal"`
	} `json:"error"`
}

func thermalTestServer(t *testing.T, config thermal.Config, temperature *float64, known bool) (*Server, *thermal.Guard) {
	t.Helper()
	if config.StatePath == "" {
		config.StatePath = filepath.Join(t.TempDir(), "thermal-state.json")
	}
	guard := thermal.NewWithReader(config, func() thermal.Sample {
		if !known {
			return thermal.Sample{}
		}
		return thermal.Sample{Celsius: *temperature, Sensor: "x86_pkg_temp", Known: true}
	})
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithThermalGuard(guard),
	)
	return server, guard
}

func allowApply(server *Server) {
	server.applyWorkOverride = func(_ context.Context, _ string, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "committed", "runtime_state": "running"})
	}
}

func TestThermalBlockRefusesRuntimeApplyWithoutTransaction(t *testing.T) {
	temperature := 96.0
	server, _ := thermalTestServer(t, thermal.Config{WarnC: 85, BlockC: 95}, &temperature, true)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked apply status = %d, want 503: %s", response.Code, response.Body.String())
	}
	var body thermalErrorBody
	decode(t, response, &body)
	if body.Error.Code != "thermal_throttle" {
		t.Fatalf("error code = %q, want thermal_throttle: %s", body.Error.Code, response.Body.String())
	}
	if body.Error.TemperatureC == nil || *body.Error.TemperatureC != 96 || body.Error.BlockC == nil || *body.Error.BlockC != 95 {
		t.Fatalf("error temperature/threshold missing: %s", response.Body.String())
	}
	if body.Error.Suggestion == "" {
		t.Fatalf("error suggestion missing: %s", response.Body.String())
	}
	if body.Error.Thermal.Status != thermal.StatusBlocked || body.Error.Thermal.TemperatureC == nil {
		t.Fatalf("error thermal snapshot = %+v", body.Error.Thermal)
	}
	if strings.Contains(response.Body.String(), `"transaction_id"`) {
		t.Fatalf("blocked apply created a transaction: %s", response.Body.String())
	}
	if server.applyState().InProgress {
		t.Fatal("blocked apply left the apply gate in progress")
	}
	if !server.runtimeApplyMu.TryLock() {
		t.Fatal("blocked apply took the apply gate")
	}
	server.runtimeApplyMu.Unlock()
	if server.lastRuntime != nil || server.lastFailedApply != nil {
		t.Fatalf("blocked apply recorded runtime evidence: last=%+v failed=%+v", server.lastRuntime, server.lastFailedApply)
	}

	// Read-only/status access must keep working while blocked.
	health := requestBody(t, server, http.MethodGet, "/api/v1/health", "")
	if health.Code != http.StatusOK {
		t.Fatalf("health while blocked = %d: %s", health.Code, health.Body.String())
	}
	var healthBody HealthResponse
	decode(t, health, &healthBody)
	if healthBody.Thermal.Status != thermal.StatusBlocked || healthBody.Thermal.TemperatureC == nil || *healthBody.Thermal.TemperatureC != 96 {
		t.Fatalf("health thermal = %+v", healthBody.Thermal)
	}
	if status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/status", "", cookie); status.Code != http.StatusOK {
		t.Fatalf("runtime status while blocked = %d: %s", status.Code, status.Body.String())
	}
}

func TestThermalRecoveryAllowsApplyAgain(t *testing.T) {
	temperature := 96.0
	server, _ := thermalTestServer(t, thermal.Config{WarnC: 85, BlockC: 95}, &temperature, true)
	allowApply(server)
	cookie := loginAdmin(t, server)

	if response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("hot apply status = %d, want 503: %s", response.Code, response.Body.String())
	}

	temperature = 62
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"committed"`) {
		t.Fatalf("cool apply status = %d: %s", response.Code, response.Body.String())
	}
}

func TestThermalWarnDoesNotBlockApply(t *testing.T) {
	temperature := 86.0
	server, _ := thermalTestServer(t, thermal.Config{WarnC: 85, BlockC: 95}, &temperature, true)
	allowApply(server)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("warn apply status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var healthBody HealthResponse
	decode(t, requestBody(t, server, http.MethodGet, "/api/v1/health", ""), &healthBody)
	if healthBody.Thermal.Status != thermal.StatusWarn {
		t.Fatalf("health thermal = %+v, want warn", healthBody.Thermal)
	}
}

func TestThermalUnknownAndDisabledPoliciesDoNotBlock(t *testing.T) {
	for _, test := range []struct {
		name        string
		config      thermal.Config
		temperature float64
		known       bool
		wantEnabled bool
	}{
		{name: "unknown sensor", config: thermal.Config{WarnC: 85, BlockC: 95}, known: false, wantEnabled: true},
		{name: "disabled", config: thermal.Config{WarnC: 0, BlockC: 0}, temperature: 120, known: true, wantEnabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			temperature := test.temperature
			server, _ := thermalTestServer(t, test.config, &temperature, test.known)
			allowApply(server)
			cookie := loginAdmin(t, server)

			response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
			if response.Code != http.StatusOK {
				t.Fatalf("apply status = %d, want 200: %s", response.Code, response.Body.String())
			}
			var healthBody HealthResponse
			decode(t, requestBody(t, server, http.MethodGet, "/api/v1/health", ""), &healthBody)
			if healthBody.Thermal.Status != thermal.StatusOK {
				t.Fatalf("health thermal = %+v, want ok", healthBody.Thermal)
			}
			if healthBody.Thermal.Enabled != test.wantEnabled {
				t.Fatalf("health thermal enabled = %v, want %v", healthBody.Thermal.Enabled, test.wantEnabled)
			}
			if !test.known && healthBody.Thermal.TemperatureC != nil {
				t.Fatalf("unknown sensor reported temperature: %+v", healthBody.Thermal)
			}
		})
	}
}

func TestThermalBlockRefusesFirmwareInstallBeforeLaunch(t *testing.T) {
	temperature := 99.0
	server, _ := thermalTestServer(t, thermal.Config{WarnC: 85, BlockC: 95}, &temperature, true)
	cookie := loginAdmin(t, server)

	server.firmwareMu.Lock()
	server.firmwareStatus = FirmwareUpdateStatus{Staged: true, ImagePath: "/tmp/package.bin", ImageHash: "abc123"}
	server.firmwareMu.Unlock()
	launched := false
	server.firmwareInstallStart = func(firmwareInstallInvocation) error {
		launched = true
		return nil
	}

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/firmware/update/install", `{"confirm_install":true}`, cookie)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked firmware install status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if launched {
		t.Fatal("blocked firmware install launched the installer")
	}
	server.firmwareMu.Lock()
	installing := server.firmwareStatus.Installing
	server.firmwareMu.Unlock()
	if installing {
		t.Fatal("blocked firmware install marked the status as installing")
	}
}

func TestThermalBlockRefusesConfigApplyBeforeAsyncDispatch(t *testing.T) {
	temperature := 96.0
	server, _ := thermalTestServer(t, thermal.Config{WarnC: 85, BlockC: 95}, &temperature, true)
	cookie := loginAdmin(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/apply", `{}`, cookie)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked config apply status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if server.configApplyInProgress() {
		t.Fatal("blocked config apply created an async apply state")
	}
}
