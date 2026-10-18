package httpapi

import (
	"os"
	"strings"
)

// clearInitialAdminCredentials removes the one-time factory credential after
// the administrator has chosen a new password. The running control API still
// has the environment value in memory, but the persisted database hash takes
// precedence for all subsequent logins.
func (server *Server) clearInitialAdminCredentials() {
	passwordFile := envOrDefault("LY_ROUTE_INITIAL_ADMIN_PASSWORD_FILE", "/root/ly-route-initial-admin-password")
	_ = os.Remove(passwordFile)

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

	controlEnv := envOrDefault("LY_ROUTE_CONTROL_ENV_FILE", "/etc/ly-route/control-api.env")
	if body, err := os.ReadFile(controlEnv); err == nil {
		lines := strings.Split(string(body), "\n")
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "LY_ROUTE_ADMIN_PASSWORD=") {
				continue
			}
			kept = append(kept, line)
		}
		_ = os.WriteFile(controlEnv, []byte(strings.Join(kept, "\n")), 0o600)
	}
}
