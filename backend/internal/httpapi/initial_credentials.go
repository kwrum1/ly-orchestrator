package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"log"
	"os"
	"strings"
)

// clearInitialAdminCredentials retires the one-time factory credential after
// the administrator has chosen a new password. The running control API still
// has the environment value in memory, and the persisted database hash takes
// precedence for all subsequent logins.
//
// The credential is rotated, never deleted. Removing LY_ROUTE_ADMIN_PASSWORD
// from the environment file left the appliance with no bootstrap credential at
// all: rebuilding the database (factory reset, corrupted state, a failed
// upgrade) then produced 503 auth_not_configured with no way back in. A fresh
// random value keeps the retired password dead while a root-only bootstrap
// credential always exists.
func (server *Server) clearInitialAdminCredentials() {
	passwordFile := envOrDefault("LY_ROUTE_INITIAL_ADMIN_PASSWORD_FILE", "/root/ly-route-initial-admin-password")
	// The console banner must not leak a credential; only the root-only file
	// and the root-only environment file carry the rotated value.
	issueFile := envOrDefault("LY_ROUTE_ISSUE_FILE", "/etc/issue")
	if body, err := os.ReadFile(issueFile); err == nil {
		lines := strings.Split(string(body), "\n")
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "Initial admin password:") {
				continue
			}
			kept = append(kept, line)
		}
		_ = os.WriteFile(issueFile, []byte(strings.Join(kept, "\n")), 0o644)
	}

	password, err := randomBootstrapPassword()
	if err != nil || password == "" {
		// Keep the existing bootstrap credential rather than leaving the device
		// unbootstrappable. The password the operator just chose already
		// replaced this value in the database.
		log.Printf("keep the existing bootstrap admin password: %v", err)
		return
	}
	if err := os.WriteFile(passwordFile, []byte(password), 0o600); err != nil {
		log.Printf("write bootstrap admin password file: %v", err)
		return
	}
	_ = os.Chmod(passwordFile, 0o600)

	controlEnv := envOrDefault("LY_ROUTE_CONTROL_ENV_FILE", "/etc/ly-route/control-api.env")
	lines := []string{}
	if body, err := os.ReadFile(controlEnv); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "LY_ROUTE_ADMIN_PASSWORD=") ||
				strings.HasPrefix(line, "LY_ROUTE_ADMIN_PASSWORD_HASH=") || line == "" {
				continue
			}
			lines = append(lines, line)
		}
	}
	// Keep the bootstrap credential one-way when the installer provisioned a
	// hash. The rotated plaintext stays in the root-only file so a rebuilt
	// database can still be bootstrapped, but it never enters the environment.
	if strings.TrimSpace(server.auth.AdminPasswordHash) != "" {
		lines = append(lines, "LY_ROUTE_ADMIN_PASSWORD_HASH="+hashPassword(server.auth.AdminUsername, password))
	} else {
		lines = append(lines, "LY_ROUTE_ADMIN_PASSWORD="+password)
	}
	if err := os.WriteFile(controlEnv, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		log.Printf("write bootstrap admin password to environment file: %v", err)
		return
	}
	_ = os.Chmod(controlEnv, 0o600)
	log.Printf("rotated the bootstrap admin password; it is kept root-only in %s", passwordFile)
}

// randomBootstrapPassword returns 24 bytes of entropy encoded URL-safe without
// padding: long enough for a bootstrap credential, short enough to type on a
// serial console.
func randomBootstrapPassword() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
