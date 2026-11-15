package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The control plane listens on the management LAN, so credential guessing must
// cost something: the fifth failure blocks that caller for a minute.
func TestLoginIsThrottledAfterRepeatedFailures(t *testing.T) {
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "correct-horse"}),
		WithClock(fixedClock()),
	)
	attempt := func(password string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"username":"admin","password":"` + password + `"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body)
		request.RemoteAddr = "192.0.2.10:5555"
		recorder := httptest.NewRecorder()
		server.handleLogin(recorder, request)
		return recorder
	}

	for attemptIndex := 0; attemptIndex < loginFailureLimit; attemptIndex++ {
		if recorder := attempt("wrong-password"); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401: %s", attemptIndex+1, recorder.Code, recorder.Body.String())
		}
	}
	blocked := attempt("correct-horse")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked attempt = %d, want 429: %s", blocked.Code, blocked.Body.String())
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("a throttled login must tell the client when to retry")
	}

	// A different client is unaffected.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"correct-horse"}`))
	request.RemoteAddr = "192.0.2.11:5555"
	recorder := httptest.NewRecorder()
	server.handleLogin(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("other client = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
}

// A successful login clears the failure history so a legitimate operator is not
// penalised for earlier typos.
func TestSuccessfulLoginClearsFailures(t *testing.T) {
	server := New(
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "correct-horse"}),
		WithClock(fixedClock()),
	)
	request := func(password string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"username":"admin","password":"` + password + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body)
		req.RemoteAddr = "192.0.2.20:5555"
		recorder := httptest.NewRecorder()
		server.handleLogin(recorder, req)
		return recorder
	}
	for attemptIndex := 0; attemptIndex < loginFailureLimit-1; attemptIndex++ {
		request("wrong-password")
	}
	if recorder := request("correct-horse"); recorder.Code != http.StatusOK {
		t.Fatalf("login after typos = %d, want 200", recorder.Code)
	}
	server.loginMu.Lock()
	remaining := len(server.loginFailures)
	server.loginMu.Unlock()
	if remaining != 0 {
		t.Fatalf("failure history not cleared: %d entries", remaining)
	}
}
