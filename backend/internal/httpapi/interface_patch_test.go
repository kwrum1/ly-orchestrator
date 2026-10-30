package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// PATCH /api/v1/interfaces/<product id> must merge the stored document (keyed by
// the kernel netdev name) instead of recreating it from the partial payload.
// Losing the address here is what took LAN30 down after the UI acceptance case
// toggled its role.
func TestInterfacePatchByProductIDRetiresStaleAddressOnRoleChange(t *testing.T) {
	server, cookie := inventoryTestServer(t)
	ctx := withInterfaceIdentities(t.Context())
	var identity interfaceIdentity
	found := false
	for _, candidate := range server.interfaceIdentities(ctx) {
		if candidate.ID != "" && candidate.LinuxInterface != "" && !strings.EqualFold(candidate.ID, candidate.LinuxInterface) {
			identity, found = candidate, true
			break
		}
	}
	if !found {
		t.Skip("host exposes no distinct product id / netdev pair")
	}

	created := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+identity.LinuxInterface,
		`{"id":"`+identity.LinuxInterface+`","name":"`+identity.ID+`","display_name":"uiacc-lan","gateway_role":"lan","cidr":"192.0.2.1/24","kind":"interface","type":"interface"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("seed interface: %d %s", created.Code, created.Body.String())
	}

	patched := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+identity.ID,
		`{"id":"`+identity.ID+`","name":"`+identity.ID+`","gateway_role":"wan","mode_role":{"gateway":"wan","bridge":null},"candidate_scopes":["wan"],"role_configured":true}`, cookie)
	if patched.Code != http.StatusOK {
		t.Fatalf("patch by product id: %d %s", patched.Code, patched.Body.String())
	}
	body := patched.Body.String()
	if !strings.Contains(body, `"gateway_role":"wan"`) {
		t.Fatalf("role not applied: %s", body)
	}
	if strings.Contains(body, `"cidr":"192.0.2.1/24"`) {
		t.Fatalf("old LAN address followed the port into its new direction: %s", body)
	}
	// The label is the LAN line identity. Keeping it rendered a ghost /lan row
	// on the port's new WAN role with an empty address.
	if strings.Contains(body, `"display_name":"uiacc-lan"`) {
		t.Fatalf("old LAN label followed the port into its new direction: %s", body)
	}
}
