package gateway

import "testing"

func TestGatewayAuthDefaultsRequireFactoryPasswordChange(t *testing.T) {
	for _, key := range []string{
		"LY_ROUTE_ADMIN_USERNAME",
		"LY_ROUTE_ADMIN_PASSWORD",
		"LY_ROUTE_ADMIN_PASSWORD_HASH",
		"LY_ROUTE_FORCE_PASSWORD_CHANGE",
	} {
		t.Setenv(key, "")
	}

	config := gatewayAuthConfig()
	if config.AdminUsername != "admin" {
		t.Fatalf("default admin username = %q, want admin", config.AdminUsername)
	}
	if config.AdminPassword != "" {
		t.Fatalf("factory admin password = %q, want first-boot generated secret", config.AdminPassword)
	}
	if !config.ForcePasswordChange {
		t.Fatal("factory admin account must require a password change")
	}
}

func TestGatewayAuthReadsInstallerPasswordHash(t *testing.T) {
	t.Setenv("LY_ROUTE_ADMIN_USERNAME", "")
	t.Setenv("LY_ROUTE_ADMIN_PASSWORD", "")
	t.Setenv("LY_ROUTE_ADMIN_PASSWORD_HASH", "$6$abcdefghijklmnop$hash")
	t.Setenv("LY_ROUTE_FORCE_PASSWORD_CHANGE", "false")

	config := gatewayAuthConfig()
	if config.AdminPasswordHash != "$6$abcdefghijklmnop$hash" {
		t.Fatalf("installer password hash = %q", config.AdminPasswordHash)
	}
	if config.ForcePasswordChange {
		t.Fatal("installer-provisioned password must not force a change")
	}
}
