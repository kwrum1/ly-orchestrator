package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

// Connectivity diagnostics.
//
// The probe runs in the kernel and is bound with `ping -I`. For a LAN port the
// routed address lives on the LCP tap (lylan-<port>), not on the placeholder
// netdev, so the tap is used as the source interface; the probe then really
// leaves through that port and returns through the same line.
//
// VPP's own `ping` CLI is deliberately NOT used: on this image (VPP 26.06 with
// AF_PACKET attachments) it takes the whole dataplane down with SIGSEGV
// (reproduced: vpp.service NRestarts 2 -> 3, every later vppctl call returns
// "connect: connection refused"). A diagnostic must never kill forwarding.

const (
	diagnosticsPingDefaultCount   = 4
	diagnosticsPingMaxCount       = 20
	diagnosticsPingDefaultTimeout = 2000
	diagnosticsPingMinTimeoutMS   = 500
	diagnosticsPingMaxTimeoutMS   = 10000

	// Host-originated probes that leave the selected LAN's own subnet cannot be
	// bound to the LCP tap: lylan-* is L2-patched to the physical port, so the
	// kernel would ARP for the far target on the LAN and never reach VPP's
	// routing/NAT. They are routed through this real routed TAP instead.
	diagnosticsProbeTapHostInterface = "lyprobe"
	diagnosticsProbeTapVPPAddress    = "169.254.255.1"
	diagnosticsProbeTapHostAddress   = "169.254.255.2"
)

// diagnosticsProbeTapMu serialises the transient /32 route. Two diagnostics can
// run concurrently, but they must not race on the host routing table.
var diagnosticsProbeTapMu sync.Mutex

type diagnosticsPingRequest struct {
	Target      string `json:"target"`
	InterfaceID string `json:"interface_id"`
	Count       int    `json:"count"`
	TimeoutMS   int    `json:"timeout_ms"`
}

type diagnosticsPingPacket struct {
	Seq    int     `json:"seq"`
	Status string  `json:"status"`
	RTTms  float64 `json:"rtt_ms,omitempty"`
	TTL    int     `json:"ttl,omitempty"`
	Bytes  int     `json:"bytes,omitempty"`
}

type diagnosticsPingResult struct {
	Target         string                  `json:"target"`
	Address        string                  `json:"address"`
	InterfaceID    string                  `json:"interface_id,omitempty"`
	Interface      string                  `json:"interface,omitempty"`
	ProbeInterface string                  `json:"probe_interface,omitempty"`
	SourceAddress  string                  `json:"source_address,omitempty"`
	Method         string                  `json:"method"`
	Command        string                  `json:"command"`
	Count          int                     `json:"count"`
	TimeoutMS      int                     `json:"timeout_ms"`
	Sent           int                     `json:"sent"`
	Received       int                     `json:"received"`
	LossPercent    float64                 `json:"loss_percent"`
	MinRTTms       float64                 `json:"min_rtt_ms,omitempty"`
	AvgRTTms       float64                 `json:"avg_rtt_ms,omitempty"`
	MaxRTTms       float64                 `json:"max_rtt_ms,omitempty"`
	Packets        []diagnosticsPingPacket `json:"packets"`
	Output         string                  `json:"output"`
	StartedAt      time.Time               `json:"started_at"`
	DurationMS     int64                   `json:"duration_ms"`
}

var (
	diagnosticsPingReplyPattern = regexp.MustCompile(`^(\d+) bytes from ([^:]+): icmp_seq=(\d+) ttl=(\d+) time=([0-9.]+)\s*ms`)
	diagnosticsPingStatsPattern = regexp.MustCompile(`(\d+)(?: packets)? (?:transmitted|sent), (\d+) received, ([0-9.]+)% packet loss`)
)

// handleDiagnosticsPing runs one bound connectivity probe.
func (server *Server) handleDiagnosticsPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	session, ok := server.sessionFromRequest(r)
	if !ok {
		server.recordAudit("anonymous", "system", r.URL.Path, "diagnostics_ping", "denied", "authentication required", r)
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	request, err := diagnosticsPingRequestFromRequest(r)
	if err != nil {
		server.recordAudit(session.Username, session.Role, r.URL.Path, "diagnostics_ping", "failure", err.Error(), r)
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	result, statusCode, code, runErr := server.runDiagnosticsPing(r.Context(), request)
	if runErr != nil {
		server.recordAudit(session.Username, session.Role, r.URL.Path, "diagnostics_ping", "failure", runErr.Error(), r)
		writeError(w, r, statusCode, code, runErr.Error())
		return
	}
	server.recordAudit(session.Username, session.Role, r.URL.Path, "diagnostics_ping", "success",
		fmt.Sprintf("target=%s source_interface=%s source=%s sent=%d received=%d loss=%.0f%%",
			result.Address, result.Interface, result.SourceAddress, result.Sent, result.Received, result.LossPercent), r)
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "request_id": requestID(r)})
}

func diagnosticsPingRequestFromRequest(r *http.Request) (diagnosticsPingRequest, error) {
	var request diagnosticsPingRequest
	if r.Method == http.MethodGet {
		query := r.URL.Query()
		request.Target = strings.TrimSpace(query.Get("target"))
		request.InterfaceID = strings.TrimSpace(query.Get("interface_id"))
		if value := strings.TrimSpace(query.Get("count")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return request, fmt.Errorf("count must be an integer")
			}
			request.Count = parsed
		}
		if value := strings.TrimSpace(query.Get("timeout_ms")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return request, fmt.Errorf("timeout_ms must be an integer")
			}
			request.TimeoutMS = parsed
		}
	} else {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
			return request, fmt.Errorf("request body must be valid JSON")
		}
		request.Target = strings.TrimSpace(request.Target)
		request.InterfaceID = strings.TrimSpace(request.InterfaceID)
	}
	if request.Target == "" {
		return request, fmt.Errorf("target is required")
	}
	if len(request.Target) > 255 {
		return request, fmt.Errorf("target is too long")
	}
	if request.Count == 0 {
		request.Count = diagnosticsPingDefaultCount
	}
	if request.Count < 1 || request.Count > diagnosticsPingMaxCount {
		return request, fmt.Errorf("count must be between 1 and %d", diagnosticsPingMaxCount)
	}
	if request.TimeoutMS == 0 {
		request.TimeoutMS = diagnosticsPingDefaultTimeout
	}
	if request.TimeoutMS < diagnosticsPingMinTimeoutMS || request.TimeoutMS > diagnosticsPingMaxTimeoutMS {
		return request, fmt.Errorf("timeout_ms must be between %d and %d", diagnosticsPingMinTimeoutMS, diagnosticsPingMaxTimeoutMS)
	}
	return request, nil
}

func (server *Server) runDiagnosticsPing(ctx context.Context, request diagnosticsPingRequest) (diagnosticsPingResult, int, string, error) {
	result := diagnosticsPingResult{
		Target:    request.Target,
		Count:     request.Count,
		TimeoutMS: request.TimeoutMS,
		Method:    "linux",
		Packets:   []diagnosticsPingPacket{},
	}

	address, err := resolveDiagnosticsTarget(ctx, request.Target)
	if err != nil {
		return result, http.StatusBadRequest, "invalid_target", err
	}
	result.Address = address.String()

	sourceInterface := ""
	dataplaneProbe := false
	if request.InterfaceID != "" {
		linux := server.resolveInterfaceID(ctx, request.InterfaceID)
		if !server.knownInterface(ctx, linux) {
			return result, http.StatusUnprocessableEntity, "unknown_interface",
				fmt.Errorf("interface %q does not exist on this device", request.InterfaceID)
		}
		result.InterfaceID = server.displayInterfaceName(ctx, linux)
		probeInterface, ok := diagnosticsProbeInterface(linux)
		if !ok {
			// Dataplane-owned port without a kernel address (a WAN line): the
			// probe still has to be emitted on that line, so send it through the
			// routed probe tap and let VPP route + NAT it out of the selected
			// port. Refusing here made "select the WAN interface" a dead end.
			result.Interface = diagnosticsProbeTapHostInterface
			dataplaneProbe = true
		} else {
			sourceInterface = probeInterface
			result.Interface = probeInterface
			result.SourceAddress = kernelInterfaceAddress(probeInterface, address.Is6())
			if !address.Is6() {
				if prefix, hasPrefix := kernelInterfacePrefix(probeInterface, false); !hasPrefix || !prefix.Contains(address) {
					// Off-subnet target: the LCP tap is L2-patched, so binding to
					// it would ARP on the LAN instead of routing. Send the probe
					// through the routed dataplane TAP and let VPP/NAT carry it.
					dataplaneProbe = true
				}
			}
		}
	}

	waitSeconds := int((time.Duration(request.TimeoutMS) * time.Millisecond).Round(time.Second) / time.Second)
	if waitSeconds < 1 {
		waitSeconds = 1
	}
	if waitSeconds > 10 {
		waitSeconds = 10
	}
	result.TimeoutMS = waitSeconds * 1000

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(request.Count+2)*time.Duration(waitSeconds)*time.Second+10*time.Second)
	defer cancel()

	var args []string
	if dataplaneProbe {
		args = []string{"-n", "-c", strconv.Itoa(request.Count), "-W", strconv.Itoa(waitSeconds), "-I", diagnosticsProbeTapHostInterface}
		diagnosticsProbeTapMu.Lock()
		defer diagnosticsProbeTapMu.Unlock()
		if err := ensureDiagnosticsProbeTap(execCtx); err != nil {
			return result, http.StatusServiceUnavailable, "dataplane_unavailable", err
		}
		routeArgs := []string{"route", "replace", address.String() + "/32", "via", diagnosticsProbeTapVPPAddress,
			"dev", diagnosticsProbeTapHostInterface, "src", diagnosticsProbeTapHostAddress}
		if output, routeErr := exec.CommandContext(execCtx, "ip", routeArgs...).CombinedOutput(); routeErr != nil {
			return result, http.StatusServiceUnavailable, "dataplane_unavailable",
				fmt.Errorf("could not route the probe through the dataplane: %s", strings.TrimSpace(string(output)))
		}
		defer func() { _ = exec.Command("ip", "route", "del", address.String()+"/32").Run() }()
		result.ProbeInterface = diagnosticsProbeTapHostInterface
		result.SourceAddress = diagnosticsProbeTapHostAddress
		result.Method = "dataplane_probe_tap"
	} else {
		args = []string{"-n", "-c", strconv.Itoa(request.Count), "-W", strconv.Itoa(waitSeconds)}
		if address.Is6() {
			args = append(args, "-6")
		}
		if sourceInterface != "" {
			args = append(args, "-I", sourceInterface)
		}
	}
	args = append(args, address.String())
	result.Command = "ping " + strings.Join(args, " ")

	started := time.Now()
	result.StartedAt = started.UTC()
	output, runErr := exec.CommandContext(execCtx, "ping", args...).CombinedOutput()
	result.Output = strings.TrimRight(string(output), "\n")
	result.DurationMS = time.Since(started).Milliseconds()
	if runErr != nil {
		// ping exits 1 when the peer does not answer; that is a real result.
		if _, isExit := runErr.(*exec.ExitError); !isExit {
			return result, http.StatusBadGateway, "ping_failed", fmt.Errorf("ping could not be executed: %s", runErr.Error())
		}
		if execCtx.Err() == context.DeadlineExceeded {
			return result, http.StatusGatewayTimeout, "ping_timeout", fmt.Errorf("the ping did not finish within the time budget")
		}
	}
	parseDiagnosticsPingOutput(&result)
	return result, http.StatusOK, "", nil
}

// knownInterface reports whether an identifier resolves to a port this boot has
// actually observed.
func (server *Server) knownInterface(ctx context.Context, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, identity := range server.interfaceIdentities(ctx) {
		if strings.EqualFold(identity.ID, value) || strings.EqualFold(identity.LinuxInterface, value) {
			return strings.TrimSpace(identity.LinuxInterface) != ""
		}
	}
	for _, item := range server.interfaceAliasSnapshot(ctx) {
		actual := strings.TrimSpace(firstNonEmpty(stringField(item, "system_name"), stringField(item, "id"), stringField(item, "name")))
		if actual == "" {
			continue
		}
		if strings.EqualFold(actual, value) {
			return true
		}
	}
	return false
}

// diagnosticsProbeInterface picks the kernel interface whose address a bound
// probe must use. A port taken over by VPP keeps only a link-local placeholder
// in the kernel; its routed address is on the LCP tap (lylan-<port>).
func diagnosticsProbeInterface(linux string) (string, bool) {
	linux = strings.TrimSpace(linux)
	if linux == "" {
		return "", false
	}
	candidates := []string{vpp.LANControlPlaneHostInterface(linux), linux}
	for _, name := range candidates {
		if !kernelInterfaceExists(name) {
			continue
		}
		if kernelInterfaceAddress(name, false) != "" || kernelInterfaceAddress(name, true) != "" {
			return name, true
		}
	}
	return "", false
}

func kernelInterfaceExists(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	if _, err := net.InterfaceByName(name); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(sysfsRoot(), "class", "net", name))
	return err == nil
}

// kernelInterfaceAddress returns the first non-link-local address of the family.
func kernelInterfaceAddress(name string, ipv6 bool) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, entry := range addresses {
		prefix, parseErr := netip.ParsePrefix(entry.String())
		if parseErr != nil {
			continue
		}
		if prefix.Addr().Is6() != ipv6 || prefix.Addr().IsLinkLocalUnicast() {
			continue
		}
		return prefix.Addr().String()
	}
	return ""
}

// kernelInterfacePrefix returns the first non-link-local routed prefix of the
// interface. It is used to decide whether a target is on the selected LAN's own
// segment (plain kernel ping works) or has to be routed by the dataplane.
func kernelInterfacePrefix(name string, ipv6 bool) (netip.Prefix, bool) {
	if strings.TrimSpace(name) == "" {
		return netip.Prefix{}, false
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Prefix{}, false
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}, false
	}
	for _, entry := range addresses {
		prefix, parseErr := netip.ParsePrefix(entry.String())
		if parseErr != nil {
			continue
		}
		if prefix.Addr().Is6() != ipv6 || prefix.Addr().IsLinkLocalUnicast() {
			continue
		}
		return prefix.Masked(), true
	}
	return netip.Prefix{}, false
}

// ensureDiagnosticsProbeTap makes sure the routed host probe TAP exists and
// owns its /30 host address. The TAP itself is created by vpp-apply (it must
// survive VPP restarts and applies); the diagnostic never creates dataplane
// interfaces.
func ensureDiagnosticsProbeTap(ctx context.Context) error {
	if err := ensureProbeTap(ctx); err != nil {
		return err
	}
	if kernelInterfaceAddress(diagnosticsProbeTapHostInterface, false) == diagnosticsProbeTapHostAddress {
		return nil
	}
	output, err := exec.CommandContext(ctx, "ip", "addr", "replace",
		diagnosticsProbeTapHostAddress+"/30", "dev", diagnosticsProbeTapHostInterface).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not address host probe tap %s: %s", diagnosticsProbeTapHostInterface, strings.TrimSpace(string(output)))
	}
	return nil
}

func resolveDiagnosticsTarget(ctx context.Context, target string) (netip.Addr, error) {
	if address, err := netip.ParseAddr(strings.TrimSpace(target)); err == nil {
		return address.Unmap(), nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(lookupCtx, "ip", strings.TrimSpace(target))
	if err != nil || len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf("target %q is neither an IP address nor a resolvable host name", target)
	}
	return addresses[0].Unmap(), nil
}

func parseDiagnosticsPingOutput(result *diagnosticsPingResult) {
	result.Packets = []diagnosticsPingPacket{}
	seen := map[int]bool{}
	reported := false
	for _, line := range strings.Split(result.Output, "\n") {
		line = strings.TrimSpace(line)
		if match := diagnosticsPingReplyPattern.FindStringSubmatch(line); match != nil {
			size, _ := strconv.Atoi(match[1])
			seq, _ := strconv.Atoi(match[3])
			ttl, _ := strconv.Atoi(match[4])
			rtt, _ := strconv.ParseFloat(match[5], 64)
			result.Packets = append(result.Packets, diagnosticsPingPacket{Seq: seq, Status: "ok", RTTms: rtt, TTL: ttl, Bytes: size})
			seen[seq] = true
			continue
		}
		if match := diagnosticsPingStatsPattern.FindStringSubmatch(line); match != nil {
			result.Sent, _ = strconv.Atoi(match[1])
			result.Received, _ = strconv.Atoi(match[2])
			result.LossPercent, _ = strconv.ParseFloat(match[3], 64)
			reported = true
		}
	}
	if !reported {
		result.Received = len(result.Packets)
		if result.Count > result.Received {
			result.Sent = result.Count
		} else {
			result.Sent = result.Received
		}
	}
	if result.Sent > 0 {
		result.LossPercent = float64(result.Sent-result.Received) * 100 / float64(result.Sent)
		if result.LossPercent < 0 {
			result.LossPercent = 0
		}
	}
	// Missing sequence numbers are real losses; keep a row for each probe so the
	// table matches what actually happened.
	for seq := 1; seq <= result.Sent; seq++ {
		if !seen[seq] {
			result.Packets = append(result.Packets, diagnosticsPingPacket{Seq: seq, Status: "timeout"})
		}
	}
	sort.Slice(result.Packets, func(left, right int) bool { return result.Packets[left].Seq < result.Packets[right].Seq })
	minimum, maximum, total, samples := 0.0, 0.0, 0.0, 0
	for _, packet := range result.Packets {
		if packet.Status != "ok" {
			continue
		}
		if samples == 0 || packet.RTTms < minimum {
			minimum = packet.RTTms
		}
		if samples == 0 || packet.RTTms > maximum {
			maximum = packet.RTTms
		}
		total += packet.RTTms
		samples++
	}
	if samples > 0 {
		result.MinRTTms = minimum
		result.MaxRTTms = maximum
		result.AvgRTTms = total / float64(samples)
	}
}

// probeTapVPPInterface is the VPP name of the routed host probe tap. The tap id
// must stay below 1000 (VPP rejects larger ids with "cannot find free interface
// id") and 999 is outside the DNS service tap range (8000+).
const probeTapVPPInterface = "tap999"

// ensureProbeTap creates or repairs the routed host probe tap. It is
// idempotent and every apply calls it, so a VPP restart cannot leave the
// dataplane without the probe path (diagnostics ping, DNS host routes).
//
// The VPP tap name is not assumed: after an upgrade an older tap that still
// claims host-if-name "lyprobe" can survive as an orphan and keep the /30
// address, while the tap actually paired with the kernel has none. ARP then
// never resolves and every probe is dropped. Resolve the paired tap by its host
// MAC, delete stale taps with the same host name, move the address to the live
// tap and verify it in VPP.
func ensureProbeTap(ctx context.Context) error {
	taps := probeTaps(ctx)
	hostMAC := kernelInterfaceHardwareAddress(diagnosticsProbeTapHostInterface)
	live := pairedProbeTap(taps, hostMAC)
	if live == "" {
		_, _ = execCommandOutput(ctx, "vppctl", "create", "tap", "id", "999", "host-if-name", diagnosticsProbeTapHostInterface, "no-gso")
		taps = probeTaps(ctx)
		live = pairedProbeTap(taps, hostMAC)
	}
	if live == "" {
		// The kernel side may not be visible yet on a very first boot; fall back
		// to the canonical name so the address can still be installed.
		live = probeTapVPPInterface
	}
	for _, tap := range taps {
		if tap.HostInterface == diagnosticsProbeTapHostInterface && tap.Name != live {
			// Orphan from an earlier codepath: the host side is paired with the
			// live tap, so this interface can never forward.
			_, _ = execCommandOutput(ctx, "vppctl", "delete", "tap", tap.Name)
		}
	}
	address := diagnosticsProbeTapVPPAddress + "/30"
	if owner := interfaceOwningAddress(ctx, address); owner != "" && owner != live {
		if !strings.HasPrefix(owner, "tap") {
			return fmt.Errorf("probe address %s is owned by non-tap interface %s", diagnosticsProbeTapVPPAddress, owner)
		}
		_, _ = execCommandOutput(ctx, "vppctl", "delete", "tap", owner)
	}
	for _, args := range [][]string{
		{"set", "interface", "state", live, "up"},
		{"set", "interface", "ip", "address", live, address},
		// The kernel probe sources from the /30 host address; without NAT44
		// inside the WAN peer has no route back to 169.254.255.0/30.
		{"set", "interface", "nat44", "in", live},
	} {
		_, _ = execCommandOutput(ctx, "vppctl", args...)
	}
	if out, err := execCommandOutput(ctx, "vppctl", "show", "interface", "address", live); err != nil || !strings.Contains(out, diagnosticsProbeTapVPPAddress+"/30") {
		return fmt.Errorf("probe tap %s does not own %s after repair", live, diagnosticsProbeTapVPPAddress)
	}
	if _, err := execCommandOutput(ctx, "ip", "addr", "replace",
		diagnosticsProbeTapHostAddress+"/30", "dev", diagnosticsProbeTapHostInterface); err != nil {
		if !kernelInterfaceExists(diagnosticsProbeTapHostInterface) {
			return fmt.Errorf("host probe tap %s is missing and could not be created: %w", diagnosticsProbeTapHostInterface, err)
		}
	}
	_, _ = execCommandOutput(ctx, "ip", "link", "set", diagnosticsProbeTapHostInterface, "up")
	if !kernelInterfaceExists(diagnosticsProbeTapHostInterface) {
		return fmt.Errorf("host probe tap %s is missing after create", diagnosticsProbeTapHostInterface)
	}
	return nil
}

type probeTapInfo struct {
	Name          string
	HostInterface string
	HostMAC       string
}

// probeTaps parses `vppctl show tap`. Only interfaces that have a host side are
// returned.
func probeTaps(ctx context.Context) []probeTapInfo {
	output, err := execCommandOutput(ctx, "vppctl", "show", "tap")
	if err != nil {
		return nil
	}
	taps := make([]probeTapInfo, 0, 2)
	current := -1
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Interface: ") {
			name := strings.Fields(strings.TrimPrefix(trimmed, "Interface: "))
			if len(name) == 0 {
				current = -1
				continue
			}
			taps = append(taps, probeTapInfo{Name: name[0]})
			current = len(taps) - 1
			continue
		}
		if current < 0 {
			continue
		}
		if strings.HasPrefix(trimmed, "name \"") {
			taps[current].HostInterface = strings.Trim(strings.TrimPrefix(trimmed, "name "), "\"")
			continue
		}
		if strings.HasPrefix(trimmed, "host-mac-addr:") {
			taps[current].HostMAC = strings.TrimSpace(strings.TrimPrefix(trimmed, "host-mac-addr:"))
		}
	}
	return taps
}

func pairedProbeTap(taps []probeTapInfo, hostMAC string) string {
	for _, tap := range taps {
		if tap.HostInterface != diagnosticsProbeTapHostInterface {
			continue
		}
		if hostMAC != "" && strings.EqualFold(tap.HostMAC, hostMAC) {
			return tap.Name
		}
	}
	for _, tap := range taps {
		if tap.HostInterface == diagnosticsProbeTapHostInterface {
			return tap.Name
		}
	}
	return ""
}

func kernelInterfaceHardwareAddress(name string) string {
	iface, err := net.InterfaceByName(strings.TrimSpace(name))
	if err != nil {
		return ""
	}
	return iface.HardwareAddr.String()
}

// interfaceOwningAddress parses `vppctl show interface address` and returns the
// interface that owns an L3 address.
func interfaceOwningAddress(ctx context.Context, address string) string {
	output, err := execCommandOutput(ctx, "vppctl", "show", "interface", "address")
	if err != nil {
		return ""
	}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "):") {
			current = strings.Split(trimmed, " ")[0]
			continue
		}
		if current != "" && strings.HasPrefix(trimmed, "L3 ") && strings.TrimSpace(strings.TrimPrefix(trimmed, "L3 ")) == address {
			return current
		}
	}
	return ""
}
