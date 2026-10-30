package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
)

// Host-originated service traffic must reach off-subnet destinations through the
// dataplane.
//
// The kernel's default route points at the management gateway, so a DNS upstream
// that lives on a WAN segment (or any public resolver) would leave the
// management NIC and die: smartdns then answers SERVFAIL and the operator sees
// "DNS is configured but nothing resolves". The routed probe tap (lyprobe) is a
// NAT44 inside interface, so a host route through it turns those flows into the
// same NATed path a LAN client uses.
//
// Routes are only added for upstreams that are not already directly connected,
// and only when the tap exists (vpp-apply owns it); this is idempotent and safe
// to run on every apply.

const (
	serviceProbeTapHostInterface = "lyprobe"
	serviceProbeTapVPPAddress    = "169.254.255.1"
	serviceProbeTapHostAddress   = "169.254.255.2"
)

// runHostRouteCommand is a test seam around `ip route`.
var runHostRouteCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}

// dataplaneDefaultRouteQuery is a test seam around reading VPP's main FIB.
var dataplaneDefaultRouteQuery = func(ctx context.Context) (string, error) {
	return execCommandOutput(ctx, "vppctl", "show", "ip", "fib", "table", "0", "0.0.0.0/0")
}

// dataplaneForwardsOffSubnet reports whether VPP's main FIB has a usable default
// route.
//
// A gateway whose WAN line has no gateway address (a plain static /24, or a
// DHCP line before the lease arrives) gets no default route: VPP synthesises
// 0.0.0.0/0 as `dpo-drop`.  Routing a DNS upstream into that dataplane turns
// every SmartDNS query into a silent black hole - clients time out or get
// SERVFAIL while the kernel default route (the management path) would have
// worked.  In that case the probe-tap route must not exist.
func dataplaneForwardsOffSubnet(ctx context.Context) bool {
	output, err := dataplaneDefaultRouteQuery(ctx)
	if err != nil {
		return false
	}
	text := strings.ToLower(output)
	if strings.Contains(text, "dpo-drop") || strings.Contains(text, "entry-flags:drop") {
		return false
	}
	return strings.Contains(text, "ipv4 via ") || strings.Contains(text, "dpo-receive")
}

// ensureServiceHostRoutes routes every enabled DNS upstream through the dataplane
// when the kernel has no direct route to it. Returns operator-facing warnings for
// anything it could not install or clean (best effort: never fails an apply).
//
// The probe tap is only a valid next hop while the dataplane has a forwarding
// path. When the WAN role is removed, or the WAN has no default route, a route
// left over from the previous generation black-holes every upstream query
// (SmartDNS then keeps timing out and LAN clients lose DNS). In that case the
// stale host route is removed so the upstream falls back to the kernel default
// route (the management path).
func (server *Server) ensureServiceHostRoutes(ctx context.Context, dataplaneCanForward bool) []string {
	items, err := server.desiredItems(ctx, "dns_upstream")
	if err != nil {
		return []string{"dns upstream routes: cannot read upstreams: " + err.Error()}
	}
	forwards := dataplaneCanForward && dataplaneForwardsOffSubnet(ctx)
	var warnings []string
	seen := map[string]bool{}
	for _, item := range items {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		for _, entry := range stringSliceField(item, "servers") {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(entry))
			if parseErr != nil || !address.Is4() || address.IsLoopback() || address.IsUnspecified() {
				continue
			}
			if seen[address.String()] || kernelRouteIsDirect(address) {
				continue
			}
			seen[address.String()] = true
			if !forwards || !kernelInterfaceExists(serviceProbeTapHostInterface) {
				if err := removeServiceProbeRoute(ctx, address); err != nil {
					warnings = append(warnings, fmt.Sprintf("dns upstream %s: %v", address, err))
				}
				continue
			}
			args := []string{"route", "replace", address.String() + "/32", "via", serviceProbeTapVPPAddress,
				"dev", serviceProbeTapHostInterface, "src", serviceProbeTapHostAddress}
			if output, routeErr := runHostRouteCommand(ctx, args...); routeErr != nil {
				warnings = append(warnings, fmt.Sprintf("dns upstream %s: %s", address, strings.TrimSpace(string(output))))
			}
		}
	}
	return warnings
}

// removeServiceProbeRoute drops a host route only when it is the product-owned
// probe route; an operator route to the same address is left untouched.
func removeServiceProbeRoute(ctx context.Context, address netip.Addr) error {
	show, err := runHostRouteCommand(ctx, "route", "show", address.String()+"/32")
	if err != nil {
		return nil
	}
	text := string(show)
	if !strings.Contains(text, "via "+serviceProbeTapVPPAddress) || !strings.Contains(text, "dev "+serviceProbeTapHostInterface) {
		return nil
	}
	output, err := runHostRouteCommand(ctx, "route", "del", address.String()+"/32", "via", serviceProbeTapVPPAddress, "dev", serviceProbeTapHostInterface)
	if err != nil {
		return fmt.Errorf("remove stale probe route: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// kernelRouteIsDirect reports whether a kernel interface already owns a prefix
// that contains the address, in which case no host route is needed.
func kernelRouteIsDirect(address netip.Addr) bool {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, entry := range addresses {
			prefix, parseErr := netip.ParsePrefix(entry.String())
			if parseErr != nil {
				continue
			}
			if prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}
