package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	controlapi "ly-route/backend/internal/api"
	"ly-route/backend/internal/product"
)

// Data-plane DNS health.
//
// /api/v1/health used to answer purely from systemd unit state, so an appliance
// could report "ok" while every client query to the LAN gateway timed out: the
// VPP DNS adapter process was healthy, the namespace existed, and VPP even
// handed the query to the adapter, but the answer never came back (a missing
// table-100 return route or a detached interception silently drops both
// directions in ip4-local).  Unit state can never prove that path.
//
// The probe therefore asks a real question through the dataplane: it sends one
// DNS query from the host into a dedicated VPP probe TAP (lydnsprobe, addressed
// with 169.254.254.2/30 on the host side) to VPP's own address on port 53.  VPP
// keeps that TAP in FIB table 100, so the packet is resolved exactly like a
// client query - through table 100, the session rules and the adapter - and the
// answer has to come back through the same TAP.  The TAP is steered by moving
// the *interface* into table 100; it is deliberately not attached through the
// interception plugin, because the shipped plugin has a single attachment slot
// and attaching a probe TAP silently detached the LAN port (field incident).
// The older `lyprobe` tap is a different interface: it is the product's routed
// egress tap for host-originated DNS upstream traffic and must stay in the main
// FIB.  Anything shorter than a full round trip (interception, return route,
// namespace, session rule, adapter, upstream) is reported as degraded with the
// stage that failed.
const (
	dnsDatapathDefaultConfigPath = "/etc/ly-route/vcl.conf"
	dnsDatapathDefaultStatePath  = "/run/ly-route/dns-datapath.json"

	// The VPP/ host addressing of the diagnostics probe TAP.  A query to
	// VPP's own side of that /30 needs no host route: it is connected, so the
	// probe never mutates the host routing table.
	dnsDatapathProbeDevice = "lyprobe-dns"
	dnsDatapathProbeTarget = "169.254.254.1:53"

	// Slightly longer than the adapter's own 3 s per-query budget, so a dead
	// upstream surfaces as the adapter's SERVFAIL answer (an actionable
	// reason) instead of a bare timeout.
	dnsDatapathProbeTimeout  = 3500 * time.Millisecond
	dnsDatapathProbeCacheTTL = 12 * time.Second
	dnsDatapathStateMaxAge   = 5 * time.Minute
)

// dnsDatapathState is the verdict written by
// /usr/lib/ly-route/dns-vpp-datapath-heal.  It mirrors what the repair helper
// verified in VPP, which the control API cannot inspect by itself (session
// rules, FIB table 100, plugin attachments).
type dnsDatapathState struct {
	OK         bool              `json:"ok"`
	Configured bool              `json:"configured"`
	Reason     string            `json:"reason"`
	Checks     map[string]string `json:"checks"`
	Interfaces []string          `json:"interfaces"`
	Prefixes   []string          `json:"prefixes"`
	Applied    []string          `json:"applied"`
	// ProbeDevice is the host interface the heal actually created. A tap that
	// had to be re-created under a VPP-allocated id still keeps the configured
	// host name, but the state file is the only authority for what the data
	// path verified; the probe must bind to that device, not to a constant.
	ProbeDevice string `json:"probe_device,omitempty"`
	GeneratedAt string `json:"generated_at"`
}

type dnsProbeResult struct {
	OK     bool
	Reason string
	Answer string
	RTT    time.Duration
	At     time.Time
}

// dnsDatapathCapability reports the data-plane DNS capability.  It is only
// published when the appliance actually ships the VPP DNS adapter, so products
// and test fixtures without it keep reporting an honest "ok".
func (server *Server) dnsDatapathCapability(ctx context.Context) (controlapi.CapabilityState, bool) {
	if !server.dnsDatapathConfigured() {
		return controlapi.CapabilityState{}, false
	}
	if state, err := readDNSDatapathState(server.dnsDatapathStatePath()); err == nil && state.Configured {
		if state.Checks["probe"] == "disabled" {
			// The appliance was deliberately configured without the data-plane
			// probe TAP; there is nothing to send a query through.
			return controlapi.CapabilityState{Name: "dns_datapath", Available: true, State: controlapi.CapabilityAvailable}, true
		}
		if !state.OK {
			reason := strings.TrimSpace(state.Reason)
			if reason == "" {
				reason = "the VPP DNS data path is not verified"
			}
			return controlapi.CapabilityState{
				Name:      "dns_datapath",
				Available: false,
				State:     controlapi.CapabilityDegraded,
				Reason:    reason,
			}, true
		}
	}
	result := server.runDNSDatapathProbe(ctx)
	if result.OK {
		return controlapi.CapabilityState{Name: "dns_datapath", Available: true, State: controlapi.CapabilityAvailable}, true
	}
	reason := strings.TrimSpace(result.Reason)
	if reason == "" {
		reason = "the data-plane DNS probe did not complete"
	}
	return controlapi.CapabilityState{
		Name:      "dns_datapath",
		Available: false,
		State:     controlapi.CapabilityDegraded,
		Reason:    reason,
	}, true
}

func (server *Server) dnsDatapathConfigured() bool {
	// Only the gateway product runs a VPP DNS adapter; an orchestrator that
	// happens to carry the shipped config file must not gain a probe that can
	// never succeed.
	if !server.profile.AllowsService(product.ServiceVPP) || !server.profile.AllowsService(product.ServiceSmartDNS) {
		return false
	}
	path := strings.TrimSpace(server.dnsDatapathConfigPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_DATAPATH_CONFIG"))
	}
	if path == "" {
		path = dnsDatapathDefaultConfigPath
	}
	_, err := os.Stat(path)
	return err == nil
}

func (server *Server) dnsDatapathStatePath() string {
	if path := strings.TrimSpace(server.dnsDatapathStateFile); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.Getenv("LY_ROUTE_DNS_DATAPATH_STATE")); path != "" {
		return path
	}
	return dnsDatapathDefaultStatePath
}

// runDNSDatapathProbe runs the probe at most once per cache window so a health
// poll cannot turn into a flood of DNS queries, and so a single timeout is not
// mistaken for a flap by concurrent callers.
// dnsProbeDevice resolves the host interface the probe must bind to. The heal
// records the device it actually verified; the constant is only a fallback for
// images that predate that field.
func (server *Server) dnsProbeDevice() string {
	if state, err := readDNSDatapathState(server.dnsDatapathStatePath()); err == nil {
		if device := strings.TrimSpace(state.ProbeDevice); device != "" {
			return device
		}
	}
	return dnsDatapathProbeDevice
}

func (server *Server) runDNSDatapathProbe(ctx context.Context) dnsProbeResult {
	server.dnsProbeMu.Lock()
	defer server.dnsProbeMu.Unlock()
	if server.dnsProbeCached != nil && time.Since(server.dnsProbeCached.At) < dnsDatapathProbeCacheTTL {
		return *server.dnsProbeCached
	}
	device := server.dnsProbeDevice()
	// The probe owns its budget: an 8s request deadline must not stretch a
	// wedged resolver into an 8s hang, and a short request must not cut it off.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dnsDatapathProbeTimeout)
	defer cancel()
	var result dnsProbeResult
	var err error
	if override := server.dnsDatapathProbe; override != nil {
		result, err = override(probeCtx)
	} else {
		result, err = exchangeDNSDatapathProbe(probeCtx, device)
	}
	if err != nil {
		result = dnsProbeResult{OK: false, Reason: err.Error()}
	}
	result.At = time.Now()
	server.dnsProbeCached = &result
	return result
}

func readDNSDatapathState(path string) (dnsDatapathState, error) {
	var state dnsDatapathState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.GeneratedAt != "" {
		if generated, parseErr := time.Parse(time.RFC3339, state.GeneratedAt); parseErr == nil {
			if time.Since(generated) > dnsDatapathStateMaxAge {
				// A stale verdict is not evidence of a healthy path; fall back
				// to the live probe instead of trusting it.
				state.Configured = false
			}
		}
	}
	return state, nil
}

// exchangeDNSDatapathProbe performs one UDP DNS exchange through the VPP probe
// TAP.  It answers with the resolver's rcode, so NXDOMAIN (a working resolver)
// is a pass while SERVFAIL (a broken upstream) is a failure.
func exchangeDNSDatapathProbe(ctx context.Context, device string) (dnsProbeResult, error) {
	query, id, err := dnsProbeQuery()
	if err != nil {
		return dnsProbeResult{}, err
	}
	dialer := net.Dialer{
		Timeout: dnsDatapathProbeTimeout,
		Control: func(_, _ string, raw syscall.RawConn) error {
			var bindErr error
			if err := raw.Control(func(fd uintptr) {
				bindErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
			}); err != nil {
				return err
			}
			return bindErr
		},
	}
	started := time.Now()
	conn, err := dialer.DialContext(ctx, "udp4", dnsDatapathProbeTarget)
	if err != nil {
		return dnsProbeResult{}, fmt.Errorf("DNS data-path probe could not send through %s: %w", device, err)
	}
	defer conn.Close()
	deadline := started.Add(dnsDatapathProbeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return dnsProbeResult{}, fmt.Errorf("DNS data-path probe could not arm its deadline: %w", err)
	}
	if _, err := conn.Write(query); err != nil {
		return dnsProbeResult{}, fmt.Errorf("DNS data-path probe could not send through %s: %w", device, err)
	}
	buffer := make([]byte, 4096)
	for {
		read, err := conn.Read(buffer)
		if err != nil {
			if isTimeout(err) {
				return dnsProbeResult{}, fmt.Errorf("DNS data-path probe timed out after %s: no answer through %s (VPP interception, the DNS namespace, the session rules or the adapter is not delivering)", dnsDatapathProbeTimeout, device)
			}
			return dnsProbeResult{}, fmt.Errorf("DNS data-path probe failed while reading the answer: %w", err)
		}
		if read < 12 || binary.BigEndian.Uint16(buffer[0:2]) != id {
			continue
		}
		if buffer[2]&0x80 == 0 {
			continue
		}
		rcode := buffer[3] & 0x0f
		result := dnsProbeResult{RTT: time.Since(started), Answer: dnsProbeRCodeName(rcode)}
		if rcode == 2 {
			result.OK = false
			result.Reason = "DNS data-path probe reached the resolver but it answered SERVFAIL: the DNS adapter or its upstream is broken"
			return result, nil
		}
		result.OK = true
		return result, nil
	}
}

func isTimeout(err error) bool {
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		return true
	}
	return os.IsTimeout(err)
}

func dnsProbeRCodeName(rcode byte) string {
	switch rcode {
	case 0:
		return "NOERROR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 5:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", rcode)
	}
}

// dnsProbeQuery builds a unique A query for a name that cannot resolve, so the
// probe never depends on (or pollutes) a real cache entry.
func dnsProbeQuery() ([]byte, uint16, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, 0, fmt.Errorf("DNS data-path probe could not generate a query id: %w", err)
	}
	id := binary.BigEndian.Uint16(random[0:2])
	label := "probe-" + hex.EncodeToString(random[:])
	name := label + ".ly-route.invalid"
	question := make([]byte, 0, len(name)+6)
	for _, part := range strings.Split(name, ".") {
		if len(part) == 0 || len(part) > 63 {
			return nil, 0, fmt.Errorf("DNS data-path probe built an invalid question name %q", name)
		}
		question = append(question, byte(len(part)))
		question = append(question, part...)
	}
	question = append(question, 0, 0, 1, 0, 1)
	query := make([]byte, 12, 12+len(question))
	binary.BigEndian.PutUint16(query[0:2], id)
	binary.BigEndian.PutUint16(query[2:4], 0x0100) // recursion desired
	binary.BigEndian.PutUint16(query[4:6], 1)      // one question
	query = append(query, question...)
	return query, id, nil
}
