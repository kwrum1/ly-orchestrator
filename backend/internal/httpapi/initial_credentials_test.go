package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Retiring the factory credential must rotate it, not delete it: a rebuilt
// database bootstraps from LY_ROUTE_ADMIN_PASSWORD, and deleting the value left
// the appliance answering 503 auth_not_configured with no way back in.
func TestClearInitialAdminCredentialsRotatesBootstrapPassword(t *testing.T) {
	dir := t.TempDir()
	controlEnv := filepath.Join(dir, "control-api.env")
	passwordFile := filepath.Join(dir, "ly-route-initial-admin-password")
	issueFile := filepath.Join(dir, "issue")
	original := "old-factory-password"
	if err := os.WriteFile(controlEnv, []byte("LY_ROUTE_MANAGEMENT_INTERFACE=ens160\nLY_ROUTE_ADMIN_PASSWORD="+original+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(issueFile, []byte("Welcome\nInitial admin password: "+original+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_CONTROL_ENV_FILE", controlEnv)
	t.Setenv("LY_ROUTE_INITIAL_ADMIN_PASSWORD_FILE", passwordFile)
	t.Setenv("LY_ROUTE_ISSUE_FILE", issueFile)

	server := New(WithClock(fixedClock()))
	server.clearInitialAdminCredentials()

	envBody, err := os.ReadFile(controlEnv)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(envBody)), "\n")
	rotated := ""
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "LY_ROUTE_ADMIN_PASSWORD=") {
			count++
			rotated = strings.TrimPrefix(line, "LY_ROUTE_ADMIN_PASSWORD=")
		}
	}
	if count != 1 {
		t.Fatalf("LY_ROUTE_ADMIN_PASSWORD entries = %d in %q", count, envBody)
	}
	if rotated == "" || rotated == original {
		t.Fatalf("bootstrap password was not rotated: %q", rotated)
	}
	if !strings.Contains(string(envBody), "LY_ROUTE_MANAGEMENT_INTERFACE=ens160") {
		t.Fatalf("unrelated environment entries were dropped: %q", envBody)
	}
	fileBody, err := os.ReadFile(passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(fileBody) != rotated {
		t.Fatalf("password file %q != environment value %q", fileBody, rotated)
	}
	if info, err := os.Stat(controlEnv); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control env mode = %v (%v)", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(passwordFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode = %v (%v)", info.Mode().Perm(), err)
	}
	issueBody, err := os.ReadFile(issueFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(issueBody), original) || strings.Contains(string(issueBody), "Initial admin password:") {
		t.Fatalf("console banner still leaks a credential: %q", issueBody)
	}
	// A rebuilt database must be bootstrappable from the rotated value.
	if !(AuthConfig{AdminUsername: "admin", AdminPassword: rotated}).configured() {
		t.Fatal("rotated credential does not configure authentication")
	}
}
