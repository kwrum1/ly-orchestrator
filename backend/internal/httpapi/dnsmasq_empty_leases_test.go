package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// A freshly installed appliance has a Dnsmasq memfile that exists but contains no
// leases. That is a valid empty database and must read back as an empty list,
// not as an internal server error during first boot.
func TestDHCPLeasesEmptyDnsmasqFileReturnsEmptyList(t *testing.T) {
	tests := map[string]string{
		"zero byte":  "",
		"blank line": "\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dnsmasq.leases")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			server := New(WithDHCPLeases(serviceRuntime.DnsmasqLeaseCollector{Path: path}))
			response := request(t, server, http.MethodGet, "/api/v1/dhcp/leases")
			if response.Code != http.StatusOK {
				t.Fatalf("empty Dnsmasq lease API status = %d: %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, required := range []string{`"items":[]`, `"total":0`, `"name":"dnsmasq_leases"`, `"available":true`} {
				if !strings.Contains(body, required) {
					t.Fatalf("empty Dnsmasq lease API response missing %q: %s", required, body)
				}
			}
		})
	}
}
