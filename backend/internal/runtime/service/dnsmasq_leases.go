package service

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDnsmasqLeaseFile = "/var/lib/misc/dnsmasq.leases"
	maxLeaseFileSize        = 64 << 20
)

// DnsmasqLeaseCollector reads dnsmasq's lease file. Each line is
// "<expiry> <mac> <ip> <hostname> <client-id>"; expiry 0 means an infinite
// lease, and "*" means the field is absent.
type DnsmasqLeaseCollector struct {
	Path string
	Now  func() time.Time
}

func (collector DnsmasqLeaseCollector) Leases(ctx context.Context) ([]map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(collector.Path)
	if path == "" {
		path = DefaultDnsmasqLeaseFile
	}
	now := time.Now().UTC()
	if collector.Now != nil {
		now = collector.Now().UTC()
	}
	content, err := readStableLeaseFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read dnsmasq lease file: %w", err)
	}
	leases, err := parseDnsmasqLeases(ctx, content, now)
	if err != nil {
		return nil, fmt.Errorf("parse dnsmasq lease file: %w", err)
	}
	return leases, nil
}

func readStableLeaseFile(ctx context.Context, path string) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !before.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		if before.Size() > maxLeaseFileSize {
			return nil, fmt.Errorf("lease file exceeds %d bytes", maxLeaseFileSize)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		after, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if before.Size() == after.Size() && before.ModTime() == after.ModTime() {
			return content, nil
		}
	}
	return nil, fmt.Errorf("lease file changed during three consecutive reads")
}

func parseDnsmasqLeases(ctx context.Context, content []byte, now time.Time) ([]map[string]any, error) {
	lines := strings.Split(string(content), "\n")
	type entry struct {
		address netip.Addr
		lease   map[string]any
	}
	entries := make([]entry, 0, len(lines))
	for index, raw := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, fmt.Errorf("line %d has %d fields; expected at least 4", index+1, len(fields))
		}
		expiry, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || expiry < 0 {
			return nil, fmt.Errorf("line %d has an invalid expiry", index+1)
		}
		address, err := netip.ParseAddr(fields[2])
		if err != nil || !address.Is4() {
			return nil, fmt.Errorf("line %d has an invalid DHCPv4 address", index+1)
		}
		if expiry > 0 && !time.Unix(expiry, 0).After(now) {
			continue
		}
		lease := map[string]any{
			"id":         address.String(),
			"ip_address": address.String(),
			"state":      "active",
		}
		if mac := strings.TrimSpace(fields[1]); mac != "" && mac != "*" {
			parsed, macErr := net.ParseMAC(mac)
			if macErr != nil {
				return nil, fmt.Errorf("line %d has an invalid hardware address", index+1)
			}
			lease["mac"] = strings.ToLower(parsed.String())
		}
		if hostname := strings.TrimSpace(fields[3]); hostname != "" && hostname != "*" {
			lease["hostname"] = hostname
		}
		if expiry == 0 {
			lease["infinite"] = true
		} else {
			lease["lease_end"] = time.Unix(expiry, 0).UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry{address: address, lease: lease})
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].address.Less(entries[right].address) })
	leases := make([]map[string]any, 0, len(entries))
	for _, item := range entries {
		leases = append(leases, item.lease)
	}
	return leases, nil
}
