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

	"ly-route/backend/internal/runtime/vpp"
)

// dnsRepairInterval bounds how often a committed apply asks the DNS data-path
// heal to re-register the VPP adapters. The heal is idempotent; the interval
// only prevents a burst of applies from forking it repeatedly.
const dnsRepairInterval = 5 * time.Second

var (
	dnsRepairMu   sync.Mutex
	dnsRepairLast time.Time
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

// requestDNSPostApplyRepair runs the DNS data-path heal after a committed
// apply. The heal re-attaches interception, rebuilds the diagnostics probe TAP
// and repoints the session rules at the live adapter index, which a fresh
// apply can invalidate.
func (server *Server) requestDNSPostApplyRepair() {
	if !dnsRepairEnabled() {
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

	cmd := exec.Command("systemctl", "start", "--no-block", "ly-route-dns-vpp-datapath.service")
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
