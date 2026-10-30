package thermal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSysfsPrefersPackageSensor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/type"), "acpitz\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/temp"), "45000\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone1/type"), "x86_pkg_temp\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone1/temp"), "52000\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone2/type"), "acpitz\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone2/temp"), "61000\n")

	sample := ReadSysfs(root)
	if !sample.Known || sample.Sensor != "x86_pkg_temp" || sample.Celsius != 52 {
		t.Fatalf("sample = %+v, want x86_pkg_temp 52C", sample)
	}
}

func TestReadSysfsFallsBackToHottestZone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/type"), "acpitz\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/temp"), "45000\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone1/type"), "acpitz\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone1/temp"), "61000\n")

	sample := ReadSysfs(root)
	if !sample.Known || sample.Celsius != 61 || sample.Sensor != "acpitz" {
		t.Fatalf("sample = %+v, want hottest acpitz 61C", sample)
	}
}

func TestReadSysfsUnknownAndPlainDegrees(t *testing.T) {
	if sample := ReadSysfs(t.TempDir()); sample.Known {
		t.Fatalf("empty sysfs reported a temperature: %+v", sample)
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/type"), "cpu\n")
	writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/temp"), "73\n")
	sample := ReadSysfs(root)
	if !sample.Known || sample.Celsius != 73 {
		t.Fatalf("plain-degree sample = %+v, want 73C", sample)
	}
}

func TestGuardWarnBlockRecoverAndPeak(t *testing.T) {
	temperature := 60.0
	statePath := filepath.Join(t.TempDir(), "thermal-state.json")
	guard := NewWithReader(Config{WarnC: 85, BlockC: 95, StatePath: statePath}, func() Sample {
		return Sample{Celsius: temperature, Sensor: "x86_pkg_temp", Known: true}
	})

	if snapshot := guard.Snapshot(); snapshot.Status != StatusOK || snapshot.TemperatureC == nil || *snapshot.TemperatureC != 60 {
		t.Fatalf("ok snapshot = %+v", snapshot)
	}
	temperature = 86
	if snapshot := guard.Snapshot(); snapshot.Status != StatusWarn {
		t.Fatalf("warn snapshot = %+v, want warn", snapshot)
	}
	if _, blocked := guard.Blocked(); blocked {
		t.Fatal("warn temperature blocked a destructive operation")
	}
	temperature = 96
	snapshot, blocked := guard.Blocked()
	if !blocked || snapshot.Status != StatusBlocked {
		t.Fatalf("blocked snapshot = %+v blocked=%v", snapshot, blocked)
	}
	if snapshot.PeakC == nil || *snapshot.PeakC != 96 {
		t.Fatalf("peak = %v, want 96", snapshot.PeakC)
	}
	if snapshot.LastBlockedAt == nil || time.Since(*snapshot.LastBlockedAt) > time.Minute {
		t.Fatalf("last blocked = %v", snapshot.LastBlockedAt)
	}

	temperature = 55
	recovered := guard.Snapshot()
	if recovered.Status != StatusOK {
		t.Fatalf("recovered snapshot = %+v", recovered)
	}
	if recovered.LastBlockedAt == nil {
		t.Fatal("recovery cleared the last-blocked timestamp")
	}
}

func TestGuardUnknownSensorDoesNotBlock(t *testing.T) {
	guard := NewWithReader(Config{WarnC: 85, BlockC: 95}, func() Sample { return Sample{} })
	snapshot, blocked := guard.Blocked()
	if blocked || snapshot.Status != StatusOK || snapshot.TemperatureC != nil {
		t.Fatalf("unknown sensor snapshot = %+v blocked=%v", snapshot, blocked)
	}
}

func TestGuardDisabledDoesNotBlock(t *testing.T) {
	guard := NewWithReader(Config{WarnC: 0, BlockC: 0}, func() Sample {
		return Sample{Celsius: 120, Sensor: "acpitz", Known: true}
	})
	snapshot, blocked := guard.Blocked()
	if blocked || snapshot.Status != StatusOK || snapshot.Enabled {
		t.Fatalf("disabled snapshot = %+v blocked=%v", snapshot, blocked)
	}
	if snapshot.TemperatureC == nil || *snapshot.TemperatureC != 120 {
		t.Fatalf("disabled guard stopped recording temperature: %+v", snapshot)
	}
}

func TestGuardPersistsStateAndRestoresPeak(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nested", "thermal-state.json")
	guard := NewWithReader(Config{WarnC: 85, BlockC: 95, StatePath: statePath}, func() Sample {
		return Sample{Celsius: 97000.0 / 1000, Sensor: "x86_pkg_temp", Known: true}
	})
	guard.Blocked()

	payload, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var persisted Snapshot
	if err := json.Unmarshal(payload, &persisted); err != nil {
		t.Fatalf("state JSON: %v", err)
	}
	if persisted.Status != StatusBlocked || persisted.BlockC != 95 || persisted.WarnC != 85 {
		t.Fatalf("persisted state = %+v", persisted)
	}
	if persisted.PeakC == nil || *persisted.PeakC != 97 || persisted.LastBlockedAt == nil {
		t.Fatalf("persisted peak/last-block = %+v", persisted)
	}

	reloaded := NewWithReader(Config{WarnC: 85, BlockC: 95, StatePath: statePath}, func() Sample { return Sample{} })
	snapshot := reloaded.Snapshot()
	if snapshot.PeakC == nil || *snapshot.PeakC != 97 {
		t.Fatalf("reloaded peak = %v, want 97", snapshot.PeakC)
	}
}

func TestApplyBootCap(t *testing.T) {
	root := t.TempDir()
	noTurbo := filepath.Join(root, "devices/system/cpu/intel_pstate/no_turbo")
	writeFile(t, noTurbo, "0\n")
	guard := New(Config{SysfsRoot: root, CapTurbo: true, StatePath: ""})
	if err := guard.ApplyBootCap(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(noTurbo)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "1\n" {
		t.Fatalf("no_turbo = %q, want 1", payload)
	}

	writeFile(t, noTurbo, "0\n")
	disabled := New(Config{SysfsRoot: root, CapTurbo: false, StatePath: ""})
	if err := disabled.ApplyBootCap(); err != nil {
		t.Fatal(err)
	}
	payload, _ = os.ReadFile(noTurbo)
	if string(payload) != "0\n" {
		t.Fatalf("disabled cap changed no_turbo to %q", payload)
	}

	missing := New(Config{SysfsRoot: t.TempDir(), CapTurbo: true, StatePath: ""})
	if err := missing.ApplyBootCap(); err != nil {
		t.Fatalf("missing intel_pstate should not fail: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	values := map[string]string{
		"LY_ROUTE_THERMAL_WARN_C":     "80",
		"LY_ROUTE_THERMAL_BLOCK_C":    "90",
		"LY_ROUTE_THERMAL_CAP_TURBO":  "0",
		"LY_ROUTE_THERMAL_STATE":      "/tmp/thermal.json",
		"LY_ROUTE_THERMAL_SYSFS_ROOT": "/tmp/sys",
	}
	config := ConfigFromEnv(func(key string) string { return values[key] })
	if config.WarnC != 80 || config.BlockC != 90 || config.CapTurbo {
		t.Fatalf("config = %+v", config)
	}
	if config.StatePath != "/tmp/thermal.json" || config.SysfsRoot != "/tmp/sys" {
		t.Fatalf("paths = %+v", config)
	}

	values = map[string]string{"LY_ROUTE_THERMAL_BLOCK_C": "0"}
	config = ConfigFromEnv(func(key string) string { return values[key] })
	if config.BlockC != 0 || config.WarnC != DefaultWarnC || !config.CapTurbo {
		t.Fatalf("block=0 config = %+v", config)
	}

	values = map[string]string{"LY_ROUTE_THERMAL_WARN_C": "bogus", "LY_ROUTE_THERMAL_BLOCK_C": "-3"}
	config = ConfigFromEnv(func(key string) string { return values[key] })
	if config.WarnC != DefaultWarnC || config.BlockC != DefaultBlockC {
		t.Fatalf("invalid config = %+v", config)
	}

	values = map[string]string{"LY_ROUTE_THERMAL_WARN_C": "98", "LY_ROUTE_THERMAL_BLOCK_C": "95"}
	config = ConfigFromEnv(func(key string) string { return values[key] })
	if config.WarnC != 90 {
		t.Fatalf("warn >= block was not clamped: %+v", config)
	}
}
