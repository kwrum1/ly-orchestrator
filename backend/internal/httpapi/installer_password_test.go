package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

const (
	testInstallerSHA512 = "$6$abcdefghijklmnop$DUA4amtXzBRzULLjp9wJVozOHihtD3Z1mcqvue0w/o24juqMZ5LRrDd7u3udSYsqZ6tGNeTR/75mN8LPw.Pnz1"
	testInstallerSHA256 = "sha256:99f24c37b0935825a3b65dc9f01e563ebe573a7139ca4c34182f26d228577c3d"
)

func TestVerifyInstallerPasswordHashes(t *testing.T) {
	for name, hash := range map[string]string{
		"sha512crypt": testInstallerSHA512,
		"product":     testInstallerSHA256,
	} {
		if !verifyPasswordHash(hash, "admin", "installer-pass-123") {
			t.Fatalf("%s hash rejected the installer password", name)
		}
		if verifyPasswordHash(hash, "admin", "wrong-pass-456") {
			t.Fatalf("%s hash accepted the wrong password", name)
		}
	}
	if verifyPasswordHash("$1$broken", "admin", "installer-pass-123") {
		t.Fatal("unsupported hash format was accepted")
	}
}

func TestInstallerHashLoginDoesNotForcePasswordChangeAndKeepsD7(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-installer-hash-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	dir := t.TempDir()
	controlEnv := filepath.Join(dir, "control-api.env")
	passwordFile := filepath.Join(dir, "ly-route-initial-admin-password")
	issueFile := filepath.Join(dir, "issue")
	if err := os.WriteFile(controlEnv, []byte("LY_ROUTE_MANAGEMENT_INTERFACE=eth0\nLY_ROUTE_ADMIN_PASSWORD_HASH="+testInstallerSHA512+"\nLY_ROUTE_FORCE_PASSWORD_CHANGE=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_CONTROL_ENV_FILE", controlEnv)
	t.Setenv("LY_ROUTE_INITIAL_ADMIN_PASSWORD_FILE", passwordFile)
	t.Setenv("LY_ROUTE_ISSUE_FILE", issueFile)

	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPasswordHash: testInstallerSHA512, ForcePasswordChange: true}),
		WithClock(fixedClock()),
	)

	wrong := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"wrong-pass-456"}`)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong installer password status = %d, want 401: %s", wrong.Code, wrong.Body.String())
	}
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"installer-pass-123"}`)
	if login.Code != http.StatusOK || strings.Contains(login.Body.String(), `"password_change_required":true`) {
		t.Fatalf("installer password login = %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]

	changed := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/auth/change-password", `{"current_password":"installer-pass-123","new_password":"CorrectHorseBattery1"}`, cookie)
	if changed.Code != http.StatusOK || strings.Contains(changed.Body.String(), `"password_change_required":true`) {
		t.Fatalf("change password = %d %s", changed.Code, changed.Body.String())
	}
	oldLogin := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"installer-pass-123"}`)
	if oldLogin.Code != http.StatusUnauthorized {
		t.Fatalf("old installer password status = %d, want 401: %s", oldLogin.Code, oldLogin.Body.String())
	}
	newLogin := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"CorrectHorseBattery1"}`)
	if newLogin.Code != http.StatusOK || strings.Contains(newLogin.Body.String(), `"password_change_required":true`) {
		t.Fatalf("new password login = %d %s", newLogin.Code, newLogin.Body.String())
	}

	envBody, err := os.ReadFile(controlEnv)
	if err != nil {
		t.Fatal(err)
	}
	envText := string(envBody)
	if strings.Contains(envText, "LY_ROUTE_ADMIN_PASSWORD=") {
		t.Fatalf("rotated environment still contains a plaintext password: %q", envText)
	}
	if !strings.Contains(envText, "LY_ROUTE_ADMIN_PASSWORD_HASH=") {
		t.Fatalf("rotated environment lost the hash bootstrap credential: %q", envText)
	}
	rotated, err := os.ReadFile(passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated) == 0 || !strings.Contains(envText, "LY_ROUTE_ADMIN_PASSWORD_HASH="+hashPassword("admin", string(rotated))) {
		t.Fatalf("rotated root-only password does not match the environment hash")
	}
	if info, err := os.Stat(controlEnv); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control env mode = %v (%v)", info.Mode().Perm(), err)
	}
}
