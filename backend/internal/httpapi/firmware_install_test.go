package httpapi

import (
	"strings"
	"testing"
)

func TestFirmwareUpgradeInstallerHasBackupRollbackAndDataplaneCheck(t *testing.T) {
	script := firmwareUpgradeInstallScript
	for _, required := range []string{
		"backup-$stamp",
		"restore_backup",
		"includes_dataplane",
		"includes_dataplane",
		"ly-route-conversion",
		"\"preserve\"",
		"verify_packages",
		"version.env",
		"pppoeclient_plugin.so",
		"curl -fsS --max-time 10 http://127.0.0.1:8080/api/v1/health",
		"systemctl reboot",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer script is missing %q", required)
		}
	}
	if strings.Contains(script, "reboot -f") {
		t.Fatal("installer still forces an unconditional reboot")
	}
}

// An upgrade must deliver the proxy fail-closed assets, not just the control
// binary. A payload that ships the helper without installing it would leave
// the device fail-open after flashing an otherwise valid upgrade package.
func TestFirmwareUpgradeInstallsProxyFailClosedAssets(t *testing.T) {
	script := firmwareUpgradeInstallScript
	for _, required := range []string{
		"usr/lib/ly-route/proxy-failopen",
		"usr/lib/ly-route/proxy-failure",
		"etc/nginx/conf.d/ly-route-proxy-failure.conf",
		"etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf",
		"usr/share/ly-route/proxy-failure",
		"proxy-failopen proxy-failure",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer script does not install %q", required)
		}
	}
	for _, restored := range []string{
		`"$backup/proxy-$(printf '%s' "$proxy_path" | tr '/' '_')"`,
		"$backup/proxy-page",
	} {
		if !strings.Contains(script, restored) {
			t.Fatalf("installer rollback does not restore %q", restored)
		}
	}
}
