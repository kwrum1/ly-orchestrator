package orchestratorapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestOrchestrationApplyErrorMapsDataplanePrerequisite(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeOrchestrationApplyError(recorder, &vpp.DataplaneLockedError{})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"prerequisite_unmet"`) || strings.Contains(body, "common_dataplane_tier") {
		t.Fatalf("body = %s", body)
	}
}

func TestCodeForErrorRecognizesJSONFieldNames(t *testing.T) {
	if code := codeForError(fmt.Errorf("invalid JSON: json: unknown field \"extra\"")); code != "unknown_field" {
		t.Fatalf("unknown field code = %q", code)
	}
	if code := codeForError(errors.New("invalid JSON: unexpected end of JSON input")); code != "invalid_json" {
		t.Fatalf("invalid json code = %q", code)
	}
}
