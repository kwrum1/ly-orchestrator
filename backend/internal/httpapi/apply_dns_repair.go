package httpapi

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/vpp"
)

// dnsRepairInterval bounds how often a committed apply asks the DNS data-path
// heal to re-register the VPP adapters. The heal is idempotent; the interval
// only prevents a burst of applies from forking it repeatedly.
const dnsRepairInterval = 5 * time.Second

const dnsRepairUnit = "ly-route-dns-vpp-datapath.service"

var (
	dnsRepairMu   sync.Mutex
	dnsRepairLast time.Time

	dnsRepairUnitMu      sync.Mutex
	dnsRepairUnitChecked bool
	dnsRepairUnitAt      time.Time
	dnsRepairUnitLoaded  bool
)

func dnsRepairEnabled() bool {
	if os.Getenv("LY_ROUTE_DNS_REPAIR_ON_APPLY") == "disabled" {
		return false
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// dnsRepairUnitPresent reports whether the heal unit is installed on this
// appliance. The orchestrator product does not ship it, and blindly starting a
// missing unit made every apply log "exit status 5" every few seconds. The
// answer is cached: package installs are rare and a slow `systemctl show` must
// not run on every apply.
func dnsRepairUnitPresent() bool {
	dnsRepairUnitMu.Lock()
	defer dnsRepairUnitMu.Unlock()
	now := time.Now()
	if dnsRepairUnitChecked && now.Sub(dnsRepairUnitAt) < 5*time.Minute {
		return dnsRepairUnitLoaded
	}
	out, err := exec.Command("systemctl", "show", "-p", "LoadState", "--value", dnsRepairUnit).Output()
	dnsRepairUnitLoaded = err == nil && strings.TrimSpace(string(out)) == "loaded"
	dnsRepairUnitChecked = true
	dnsRepairUnitAt = now
	return dnsRepairUnitLoaded
}

// requestDNSPostApplyRepair runs the DNS data-path heal after a committed
// apply. The heal re-attaches interception, rebuilds the diagnostics probe TAP
// and repoints the session rules at the live adapter index, which a fresh
// apply can invalidate.
func (server *Server) requestDNSPostApplyRepair() {
	// The heal is a gateway-only unit; the orchestrator has no interception
	// adapters to re-register.
	if server.profile.ID() != product.Gateway().ID() {
		return
	}
	if !dnsRepairEnabled() || !dnsRepairUnitPresent() {
		return
	}
	now := time.Now()
	dnsRepairMu.Lock()
	if !dnsRepairLast.IsZero() && now.Sub(dnsRepairLast) < dnsRepairInterval {
		dnsRepairMu.Unlock()
		return
	}
	dnsRepairLast = now
	dnsRepairMu.Unlock()

	cmd := exec.Command("systemctl", "start", "--no-block", dnsRepairUnit)
	if err := cmd.Start(); err != nil {
		log.Printf("runtime apply: could not start the DNS data-path heal: %v", err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("runtime apply: DNS data-path heal start failed: %v", err)
		}
	}()
}

// writeRouteABFIntent records the ABF policy ids the current desired state owns.
// The DNS data-path heal sweeps attached policies in the route range that are
// not listed here: deleting a route policy does not tear its ABF policy down,
// and a leftover policy hijacks traffic addressed to the gateway itself.
func (server *Server) writeRouteABFIntent() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	items, err := server.desiredItems(ctx, "route_policy")
	if err != nil {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if id := strings.TrimSpace(stringField(item, "id")); id != "" {
			ids = append(ids, strconv.Itoa(vpp.RouteABFPolicyID(id)))
		}
	}
	_ = os.MkdirAll("/run/ly-route", 0o755)
	_ = os.WriteFile("/run/ly-route/route-abf-ids", []byte(strings.Join(ids, " ")), 0o644)
}
