package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// A freshly installed appliance has a Kea memfile that exists but contains no
// leases. That is a valid empty database and must read back as an empty list,
// not as an internal server error during first boot.
func TestDHCPLeasesEmptyKeaDatabaseReturnsEmptyList(t *testing.T) {
	tests := map[string]string{
		"zero byte":   "",
		"blank line":  "\n",
		"header only": "address,hwaddr,client_id,valid_lifetime,expire,subnet_id,fqdn_fwd,fqdn_rev,hostname,state,user_context\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kea-leases4.csv")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			server := New(WithDHCPLeases(serviceRuntime.KeaMemfileLeaseCollector{Path: path}))
			response := request(t, server, http.MethodGet, "/api/v1/dhcp/leases")
			if response.Code != http.StatusOK {
				t.Fatalf("empty Kea lease API status = %d: %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, required := range []string{`"items":[]`, `"total":0`, `"name":"kea_leases"`, `"available":true`} {
				if !strings.Contains(body, required) {
					t.Fatalf("empty Kea lease API response missing %q: %s", required, body)
				}
			}
		})
	}
}
