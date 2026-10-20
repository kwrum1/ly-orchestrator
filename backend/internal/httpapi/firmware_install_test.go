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
		"pppoeclient_plugin.so",
		"curl -fsS --max-time 2 http://127.0.0.1:8080/api/v1/health",
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
