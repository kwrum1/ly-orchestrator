// Package thermal provides the appliance-wide CPU temperature guard.
//
// The guard reads Linux thermal zones, classifies the sample against operator
// configured warn/block thresholds and exposes a small, JSON-friendly snapshot
// for /api/v1/health and /var/lib/ly-route/thermal-state.json. Destructive
// operations ask Blocked before they start so a hot N5100 never loses the
// control plane halfway through an apply.
package thermal

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StatusOK      = "ok"
	StatusWarn    = "warn"
	StatusBlocked = "blocked"

	DefaultWarnC     = 85.0
	DefaultBlockC    = 95.0
	DefaultStatePath = "/var/lib/ly-route/thermal-state.json"
	DefaultSysfsRoot = "/sys"

	// persistInterval bounds how often an unchanged sample rewrites the state
	// file. Status transitions and block events are persisted immediately.
	persistInterval = 5 * time.Second
)

// Config is the complete thermal policy. Thresholds at or below zero disable
// that level: BlockC=0 is the documented escape hatch for a device whose
// cooling is so poor that even a recovery apply would otherwise be refused.
type Config struct {
	WarnC       float64
	BlockC      float64
	StatePath   string
	SysfsRoot   string
	CapTurbo    bool
	NoTurboPath string
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// Sample is one temperature observation. Known=false means no usable thermal
// zone was found, which is explicitly not a blocking condition.
type Sample struct {
	Celsius float64 `json:"celsius"`
	Sensor  string  `json:"sensor,omitempty"`
	Known   bool    `json:"known"`
}

// Reader returns one observation. It is a function so tests can vary the
// temperature without touching /sys.
type Reader func() Sample

// Snapshot is both the health payload and the persisted state document.
// TemperatureC and PeakC are pointers so an absent sensor is distinct from
// 0 degrees Celsius.
type Snapshot struct {
	Enabled       bool       `json:"enabled"`
	Status        string     `json:"status"`
	TemperatureC  *float64   `json:"temperature_c"`
	Sensor        string     `json:"sensor,omitempty"`
	WarnC         float64    `json:"warn_c"`
	BlockC        float64    `json:"block_c"`
	PeakC         *float64   `json:"peak_c"`
	LastBlockedAt *time.Time `json:"last_blocked_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Guard owns the cached policy state. All exported methods are safe for
// concurrent use by health polls, apply handlers and the firmware installer.
type Guard struct {
	mu        sync.Mutex
	cfg       Config
	read      Reader
	peakC     float64
	peakSeen  bool
	lastBlock time.Time
	lastStat  string
	persistAt time.Time
}

// DefaultConfig returns the conservative production policy.
func DefaultConfig() Config {
	return Config{
		WarnC:     DefaultWarnC,
		BlockC:    DefaultBlockC,
		StatePath: DefaultStatePath,
		SysfsRoot: DefaultSysfsRoot,
		CapTurbo:  true,
	}
}

// ConfigFromEnv builds a policy from LY_ROUTE_THERMAL_* variables. An invalid
// or negative value falls back to the default; an explicit 0 disables that
// threshold. LY_ROUTE_THERMAL_BLOCK_C=0 therefore turns blocking off while
// temperature recording and warning keep working.
func ConfigFromEnv(getenv func(string) string) Config {
	config := DefaultConfig()
	if getenv == nil {
		return config
	}
	config.WarnC = thresholdFromEnv(getenv, "LY_ROUTE_THERMAL_WARN_C", DefaultWarnC)
	config.BlockC = thresholdFromEnv(getenv, "LY_ROUTE_THERMAL_BLOCK_C", DefaultBlockC)
	if value := strings.TrimSpace(getenv("LY_ROUTE_THERMAL_STATE")); value != "" {
		config.StatePath = value
	}
	if value := strings.TrimSpace(getenv("LY_ROUTE_THERMAL_SYSFS_ROOT")); value != "" {
		config.SysfsRoot = value
	}
	if value := strings.TrimSpace(getenv("LY_ROUTE_THERMAL_NO_TURBO_PATH")); value != "" {
		config.NoTurboPath = value
	}
	config.CapTurbo = boolFromEnv(getenv, "LY_ROUTE_THERMAL_CAP_TURBO", true)
	if config.BlockC > 0 && config.WarnC > 0 && config.WarnC >= config.BlockC {
		config.WarnC = config.BlockC - 5
		if config.WarnC < 1 {
			config.WarnC = 1
		}
	}
	return config
}

func thresholdFromEnv(getenv func(string) string, key string, fallback float64) float64 {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return fallback
	}
	return value
}

func boolFromEnv(getenv func(string) string, key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(getenv(key)))
	switch raw {
	case "":
		return fallback
	case "0", "false", "no", "off", "disabled":
		return false
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return fallback
	}
}

// New builds a guard reading the configured sysfs root.
func New(config Config) *Guard {
	return NewWithReader(config, nil)
}

// NewWithReader builds a guard with an injected temperature source. A nil
// reader keeps the production sysfs reader.
func NewWithReader(config Config, read Reader) *Guard {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	if strings.TrimSpace(config.SysfsRoot) == "" {
		config.SysfsRoot = DefaultSysfsRoot
	}
	if read == nil {
		root := config.SysfsRoot
		read = func() Sample { return ReadSysfs(root) }
	}
	guard := &Guard{cfg: config, read: read}
	guard.loadPersisted()
	return guard
}

func (guard *Guard) loadPersisted() {
	if strings.TrimSpace(guard.cfg.StatePath) == "" {
		return
	}
	payload, err := os.ReadFile(guard.cfg.StatePath)
	if err != nil {
		return
	}
	var previous Snapshot
	if err := json.Unmarshal(payload, &previous); err != nil {
		return
	}
	if previous.PeakC != nil {
		guard.peakC = *previous.PeakC
		guard.peakSeen = true
	}
	if previous.LastBlockedAt != nil {
		guard.lastBlock = previous.LastBlockedAt.UTC()
	}
}

// Snapshot evaluates the current temperature and returns the refreshed state.
func (guard *Guard) Snapshot() Snapshot {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.evaluateLocked()
}

// Blocked evaluates the current temperature and reports whether a destructive
// operation must be refused. A blocked observation also records the block
// timestamp for the health payload and state file.
func (guard *Guard) Blocked() (Snapshot, bool) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	snapshot := guard.evaluateLocked()
	if snapshot.Status != StatusBlocked {
		return snapshot, false
	}
	// A fresh process (or a state file without a timestamp) must still record
	// when this block was first observed.
	if guard.lastBlock.IsZero() {
		guard.lastBlock = snapshot.UpdatedAt
		blocked := guard.lastBlock
		snapshot.LastBlockedAt = &blocked
		guard.persistLocked(snapshot)
	}
	return snapshot, true
}

// ApplyBootCap disables Intel Turbo Boost at startup when configured. A
// missing intel_pstate file (AMD host, VM, kernel without the driver) is not
// an error: the governor itself is deliberately left untouched.
func (guard *Guard) ApplyBootCap() error {
	if !guard.cfg.CapTurbo {
		return nil
	}
	path := strings.TrimSpace(guard.cfg.NoTurboPath)
	if path == "" {
		path = filepath.Join(guard.cfg.SysfsRoot, "devices/system/cpu/intel_pstate/no_turbo")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(payload)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return err
	}
	guard.cfg.Logf("thermal: capped turbo at boot via %s", path)
	return nil
}

func (guard *Guard) evaluateLocked() Snapshot {
	sample := guard.read()
	now := guard.cfg.Now().UTC()
	status := StatusOK
	var temperature *float64
	if sample.Known {
		value := round1(sample.Celsius)
		temperature = &value
		if !guard.peakSeen || value > guard.peakC {
			guard.peakC = value
			guard.peakSeen = true
		}
		status = guard.classify(value)
	}
	snapshot := Snapshot{
		Enabled:      guard.cfg.WarnC > 0 || guard.cfg.BlockC > 0,
		Status:       status,
		TemperatureC: temperature,
		Sensor:       sample.Sensor,
		WarnC:        guard.cfg.WarnC,
		BlockC:       guard.cfg.BlockC,
		UpdatedAt:    now,
	}
	if guard.peakSeen {
		peak := guard.peakC
		snapshot.PeakC = &peak
	}
	if status == StatusBlocked && guard.lastStat != StatusBlocked {
		guard.lastBlock = now
	}
	if !guard.lastBlock.IsZero() {
		blocked := guard.lastBlock
		snapshot.LastBlockedAt = &blocked
	}
	previous := guard.lastStat
	guard.lastStat = status
	guard.logTransition(previous, snapshot)
	guard.persistIfNeeded(snapshot, previous)
	return snapshot
}

func (guard *Guard) classify(celsius float64) string {
	if guard.cfg.BlockC > 0 && celsius >= guard.cfg.BlockC {
		return StatusBlocked
	}
	if guard.cfg.WarnC > 0 && celsius >= guard.cfg.WarnC {
		return StatusWarn
	}
	return StatusOK
}

func (guard *Guard) logTransition(previous string, snapshot Snapshot) {
	if previous == snapshot.Status {
		return
	}
	temperature := "unknown"
	if snapshot.TemperatureC != nil {
		temperature = fmt.Sprintf("%.1fC", *snapshot.TemperatureC)
	}
	switch snapshot.Status {
	case StatusBlocked:
		guard.cfg.Logf("thermal: BLOCKED temperature=%s sensor=%s block=%.0fC; destructive apply/firmware operations are refused", temperature, snapshot.Sensor, snapshot.BlockC)
	case StatusWarn:
		guard.cfg.Logf("thermal: warn temperature=%s sensor=%s warn=%.0fC block=%.0fC", temperature, snapshot.Sensor, snapshot.WarnC, snapshot.BlockC)
	default:
		guard.cfg.Logf("thermal: normal temperature=%s sensor=%s", temperature, snapshot.Sensor)
	}
}

func (guard *Guard) persistIfNeeded(snapshot Snapshot, previous string) {
	if strings.TrimSpace(guard.cfg.StatePath) == "" {
		return
	}
	if previous == snapshot.Status && !guard.persistAt.IsZero() && snapshot.UpdatedAt.Sub(guard.persistAt) < persistInterval {
		return
	}
	guard.persistLocked(snapshot)
}

func (guard *Guard) persistLocked(snapshot Snapshot) {
	path := strings.TrimSpace(guard.cfg.StatePath)
	if path == "" {
		return
	}
	guard.persistAt = snapshot.UpdatedAt
	payload, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		guard.cfg.Logf("thermal: encode state: %v", err)
		return
	}
	payload = append(payload, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			guard.cfg.Logf("thermal: create state directory %s: %v", dir, err)
			return
		}
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o644); err != nil {
		guard.cfg.Logf("thermal: write state: %v", err)
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		guard.cfg.Logf("thermal: replace state: %v", err)
	}
}

// ReadSysfs reads every thermal zone under root. x86_pkg_temp (the package
// sensor that actually follows a heavy apply) wins when present; otherwise the
// hottest valid zone is reported. No usable zone yields Known=false.
func ReadSysfs(root string) Sample {
	zones, err := filepath.Glob(filepath.Join(root, "class/thermal/thermal_zone*"))
	if err != nil || len(zones) == 0 {
		return Sample{}
	}
	var best Sample
	var packageSample Sample
	for _, zone := range zones {
		value, ok := readZoneTemp(filepath.Join(zone, "temp"))
		if !ok {
			continue
		}
		sensor := readZoneType(filepath.Join(zone, "type"))
		if sensor == "" {
			sensor = filepath.Base(zone)
		}
		sample := Sample{Celsius: value, Sensor: sensor, Known: true}
		if sensorIsPackage(sensor) {
			if !packageSample.Known || sample.Celsius > packageSample.Celsius {
				packageSample = sample
			}
			continue
		}
		if !best.Known || sample.Celsius > best.Celsius {
			best = sample
		}
	}
	if packageSample.Known {
		return packageSample
	}
	return best
}

func sensorIsPackage(sensor string) bool {
	normalized := strings.ToLower(strings.TrimSpace(sensor))
	return strings.Contains(normalized, "x86_pkg_temp") ||
		strings.Contains(normalized, "pkg_temp") ||
		strings.Contains(normalized, "package")
}

func readZoneType(path string) string {
	payload, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(payload))
}

func readZoneTemp(path string) (float64, bool) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(string(payload)), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	// Linux thermal sysfs reports millidegrees Celsius. Some lab fixtures and
	// a few out-of-tree drivers expose plain degrees; values above 200 cannot
	// be a sane plain-degree CPU reading.
	if math.Abs(value) > 200 {
		value /= 1000
	}
	return value, true
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

// UnknownSnapshot is the health shape when no guard was wired at all.
func UnknownSnapshot() Snapshot {
	return Snapshot{Status: StatusOK, UpdatedAt: time.Now().UTC()}
}
