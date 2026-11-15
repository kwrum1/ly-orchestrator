// Command installerweb is the local API behind the graphical install kiosk.
//
// It does not reimplement installation. It reads the appliance inventory,
// validates the operator plan, and hands that plan to the proven text
// installer through LYROUTE_* environment variables, then streams the
// installer log back to the browser.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultAdminPassword = "LyRoute@Init2026"

type diskInfo struct {
	Name     string `json:"name"`
	Size     uint64 `json:"size"`
	Model    string `json:"model"`
	Live     bool   `json:"live"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

type nicInfo struct {
	Name   string `json:"name"`
	MAC    string `json:"mac"`
	PCI    string `json:"pci"`
	Driver string `json:"driver"`
	Link   bool   `json:"link"`
}

type defaults struct {
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
}

type inventory struct {
	Disks     []diskInfo `json:"disks"`
	NICs      []nicInfo  `json:"nics"`
	PayloadMB uint64     `json:"payload_mb"`
	Defaults  defaults   `json:"defaults"`
}

type installRequest struct {
	Disk          string `json:"disk"`
	ManagementMAC string `json:"management_mac"`
	CIDR          string `json:"cidr"`
	Gateway       string `json:"gateway"`
	Password      string `json:"password"`
}

type statusResponse struct {
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Elapsed   int64  `json:"elapsed_seconds"`
}

type logResponse struct {
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

type runner struct {
	mu        sync.Mutex
	script    string
	logPath   string
	payload   string
	state     string
	message   string
	startedAt time.Time
	running   bool
	done      chan struct{}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "loopback address to serve the installer API on")
	script := flag.String("script", "/usr/local/sbin/ly-route-auto-install", "installer engine to execute")
	logPath := flag.String("log", "/var/log/ly-route-installer.log", "installer log file")
	payload := flag.String("payload", "/opt/ly-route/install", "embedded appliance payload directory")
	flag.Parse()

	r := &runner{script: *script, logPath: *logPath, payload: *payload, state: "idle"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/installer/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/installer/v1/inventory", func(w http.ResponseWriter, _ *http.Request) {
		inv, err := r.inventory()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, inv)
	})
	mux.HandleFunc("POST /api/installer/v1/install", func(w http.ResponseWriter, req *http.Request) {
		var plan installRequest
		if err := json.NewDecoder(req.Body).Decode(&plan); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析安装计划：" + err.Error()})
			return
		}
		if err := r.start(plan); err != nil {
			code := http.StatusBadRequest
			if err == errBusy {
				code = http.StatusConflict
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "running"})
	})
	mux.HandleFunc("GET /api/installer/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.status())
	})
	mux.HandleFunc("POST /api/installer/v1/reboot", func(w http.ResponseWriter, _ *http.Request) {
		// Only reachable once the engine has finished; the kiosk offers it so an
		// operator can leave the installer without waiting for the timer.
		r.mu.Lock()
		done := !r.running && r.state != "idle"
		r.mu.Unlock()
		if !done {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "安装尚未结束"})
			return
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			_ = exec.Command("systemctl", "reboot", "--force").Run()
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "rebooting"})
	})
	mux.HandleFunc("GET /api/installer/v1/log", func(w http.ResponseWriter, req *http.Request) {
		offset, _ := strconv.ParseInt(req.URL.Query().Get("offset"), 10, 64)
		writeJSON(w, http.StatusOK, r.readLog(offset))
	})

	ui, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		log.Fatalf("embedded UI: %v", err)
	}
	mux.Handle("GET /", http.FileServerFS(ui))

	server := &http.Server{
		Addr:              *listen,
		Handler:           loopbackOnly(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("installer API listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "bad peer", http.StatusForbidden)
			return
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.IsLoopback() {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (r *runner) inventory() (inventory, error) {
	required := r.payloadSize()
	live := liveParent()
	out := inventory{PayloadMB: required / (1024 * 1024)}
	out.Defaults = defaults{CIDR: "192.168.88.254/24", Gateway: "192.168.88.1"}

	body, err := run("lsblk", "-bdnpo", "NAME,SIZE,MODEL,TYPE,RO", "-P")
	if err != nil {
		return out, err
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		fields := parsePairs(line)
		name := fields["NAME"]
		if name == "" || fields["TYPE"] != "disk" {
			continue
		}
		size, _ := strconv.ParseUint(fields["SIZE"], 10, 64)
		disk := diskInfo{Name: name, Size: size, Model: fields["MODEL"]}
		switch {
		case name == live:
			disk.Live = true
			disk.Reason = "安装介质本身"
		case fields["RO"] == "1":
			disk.Reason = "只读设备"
		case required > 0 && size < required:
			disk.Reason = "容量小于系统镜像"
		default:
			disk.Eligible = true
		}
		out.Disks = append(out.Disks, disk)
	}
	sort.Slice(out.Disks, func(i, j int) bool { return out.Disks[i].Name < out.Disks[j].Name })

	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return out, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" {
			continue
		}
		base := filepath.Join("/sys/class/net", name)
		device, err := filepath.EvalSymlinks(filepath.Join(base, "device"))
		if err != nil {
			continue
		}
		nic := nicInfo{
			Name:   name,
			MAC:    strings.TrimSpace(readString(filepath.Join(base, "address"))),
			PCI:    filepath.Base(device),
			Driver: driverName(base),
			Link:   strings.TrimSpace(readString(filepath.Join(base, "operstate"))) == "up",
		}
		out.NICs = append(out.NICs, nic)
	}
	sort.Slice(out.NICs, func(i, j int) bool { return out.NICs[i].Name < out.NICs[j].Name })
	return out, nil
}

var errBusy = exec.ErrNotFound

func (r *runner) start(plan installRequest) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("安装已经在进行中")
	}
	r.mu.Unlock()

	inv, err := r.inventory()
	if err != nil {
		return err
	}
	var disk *diskInfo
	for i := range inv.Disks {
		if inv.Disks[i].Name == plan.Disk {
			disk = &inv.Disks[i]
		}
	}
	if disk == nil || !disk.Eligible {
		return errors.New("请选择可用的安装磁盘")
	}
	found := ""
	for _, nic := range inv.NICs {
		if nic.MAC == plan.ManagementMAC {
			found = nic.Name
		}
	}
	if found == "" {
		return errors.New("请选择管理网口")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(plan.CIDR))
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 8 || prefix.Bits() > 30 {
		return errors.New("管理地址格式应为 IPv4/CIDR，例如 192.168.88.254/24")
	}
	gateway, err := netip.ParseAddr(strings.TrimSpace(plan.Gateway))
	if err != nil || !gateway.Is4() || !prefix.Contains(gateway) || gateway == prefix.Addr() {
		return errors.New("网关必须与管理地址同网段，且不能是管理地址本身")
	}
	password := strings.TrimSpace(plan.Password)
	if password == "" {
		password = defaultAdminPassword
	}
	if len(password) < 8 {
		return errors.New("管理员密码至少 8 位")
	}

	if err := os.Truncate(r.logPath, 0); err != nil && !os.IsNotExist(err) {
		return err
	}
	cmd := exec.Command(r.script)
	cmd.Env = append(os.Environ(),
		"LYROUTE_AUTOINSTALL=1",
		"LYROUTE_TARGET="+disk.Name,
		"LYROUTE_MANAGEMENT_MAC="+plan.ManagementMAC,
		"LYROUTE_IP="+strings.TrimSpace(plan.CIDR),
		"LYROUTE_GATEWAY="+strings.TrimSpace(plan.Gateway),
		"LYROUTE_CONFIRM=yes",
		"LYROUTE_ADMIN_PASSWORD="+password,
		// The kiosk owns the screen; keep the engine's own console chatter out
		// of it and collect it from the log instead.
		"LYROUTE_CONSOLE=/dev/null",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}

	r.mu.Lock()
	r.running = true
	r.state = "running"
	r.message = ""
	r.startedAt = time.Now()
	r.mu.Unlock()

	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.running = false
		r.state = "succeeded"
		r.message = "安装完成，正在重启"
		if err != nil {
			r.state = "failed"
			r.message = "安装失败：" + err.Error() + "（详见日志）"
		}
	}()
	return nil
}

func (r *runner) status() statusResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	resp := statusResponse{State: r.state, Message: r.message}
	if !r.startedAt.IsZero() {
		resp.StartedAt = r.startedAt.Format(time.RFC3339)
		resp.Elapsed = int64(time.Since(r.startedAt).Seconds())
	}
	return resp
}

func (r *runner) readLog(offset int64) logResponse {
	file, err := os.Open(r.logPath)
	if err != nil {
		return logResponse{Offset: offset}
	}
	defer file.Close()
	if _, err := file.Seek(offset, 0); err != nil {
		return logResponse{Offset: offset}
	}
	buffer := make([]byte, 64*1024)
	n, _ := file.Read(buffer)
	return logResponse{Offset: offset + int64(n), Data: string(buffer[:n])}
}

func (r *runner) payloadSize() uint64 {
	raw := strings.TrimSpace(readString(filepath.Join(r.payload, "image.size")))
	size, _ := strconv.ParseUint(raw, 10, 64)
	return size
}

func liveParent() string {
	source, err := run("findmnt", "-n", "-o", "SOURCE", "/run/live/medium")
	if err != nil {
		return ""
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	parent, err := run("lsblk", "-ndo", "PKNAME", source)
	if err != nil {
		return ""
	}
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return ""
	}
	return "/dev/" + parent
}

func run(name string, args ...string) (string, error) {
	output, err := exec.Command(name, args...).Output()
	return string(output), err
}

func readString(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(body)
}

// driverName reports the bound kernel driver without resolving the whole sysfs
// chain, which can end in a symlinked directory.
func driverName(base string) string {
	link, err := os.Readlink(filepath.Join(base, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func mustEval(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// parsePairs decodes the key="value" lines emitted by lsblk -P so model names
// that contain spaces cannot shift the layout.
func parsePairs(line string) map[string]string {
	out := map[string]string{}
	for len(line) > 0 {
		line = strings.TrimLeft(line, " ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 || eq+1 >= len(line) || line[eq+1] != '"' {
			break
		}
		key := line[:eq]
		line = line[eq+2:]
		var value strings.Builder
		closed := false
		for i := 0; i < len(line); i++ {
			if line[i] == '\\' && i+1 < len(line) {
				value.WriteByte(line[i+1])
				i++
				continue
			}
			if line[i] == '"' {
				line = line[i+1:]
				closed = true
				break
			}
			value.WriteByte(line[i])
		}
		if !closed {
			break
		}
		out[key] = value.String()
	}
	return out
}
