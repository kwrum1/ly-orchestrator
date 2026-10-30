package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// TestSmartQoSStatusIsRetired pins the arch-v1 contract: the built-in Smart QoS
// endpoint is gone and answers 410 with a machine-readable retirement reason.
// It must never report a live state or a dataplane lock, and no HTTP method may
// reintroduce a mutable control.
func TestSmartQoSStatusIsRetired(t *testing.T) {
	server := New(WithClock(fixedClock()))
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		response := request(t, server, method, "/api/v1/flow-control/smart-qos")
		if response.Code != http.StatusGone {
			t.Fatalf("%s smart QoS status = %d: %s", method, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, "feature_retired") {
			t.Fatalf("retired smart QoS response missing feature_retired: %s", body)
		}
		if !strings.Contains(body, "traffic-control") {
			t.Fatalf("retired smart QoS response missing replacement: %s", body)
		}
		for _, forbidden := range []string{`"runtime_state":"locked"`, `"enabled":true`, "dataplane_locked", `"mutable":false`} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("retired smart QoS response must not contain %q: %s", forbidden, body)
			}
		}
	}
}
