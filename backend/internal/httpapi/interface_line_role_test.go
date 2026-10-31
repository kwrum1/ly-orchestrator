package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Regression: the first "mark this port LAN/WAN" save used to be refused with
// "set its role in 网卡设置 first" because the merged document already carried a
// display_name derived from the port id, which the line-role guard mistook for
// an explicit line creation. Only fields supplied by this request may count.
func TestInterfaceRoleSetOnUnmarkedPortIsAccepted(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := interfaceRoleTransitionPort(t, server)

	// The port already has a desired document (for example created by a partial
	// PATCH earlier) with no role but with the derived label.
	payload := interfaceContractPayload(port, "enp4s0", "", "")
	payload["display_name"] = port
	seedInterfaceContractDocument(t, server, port, payload)

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":"lan"}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("first role assignment status = %d, want 200: %s", response.Code, response.Body.String())
	}
	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after role assignment: found=%v err=%v", found, err)
	}
	if role, _ := interfaceDesiredRole(item); role != "lan" {
		t.Fatalf("role = %q, want lan: %#v", role, item)
	}
}

// The guard must still refuse an explicit line creation on a port that has no
// role yet. This is the original intent of the check and must not regress with
// the caller-fields fix.
func TestInterfaceLineCreationOnUnmarkedPortIsRejected(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	port := interfaceRoleTransitionPort(t, server)

	payload := interfaceContractPayload(port, "enp4s0", "", "")
	payload["display_name"] = port
	seedInterfaceContractDocument(t, server, port, payload)

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":"lan","display_name":"lan-main","cidr":"192.168.89.1/24"}`, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("explicit line on unmarked port status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "is not marked as LAN yet") {
		t.Fatalf("rejection is not actionable: %s", body)
	}
}
