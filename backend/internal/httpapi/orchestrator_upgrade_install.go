package httpapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type orchestratorUpgradeInvocation struct {
	Name string
	Args []string
	Unit string
}

const orchestratorUpgradeScript = `
set -eu
exec >>/var/log/ly-route-orchestrator-upgrade.log 2>&1
action=$1
package_path=$2
package_hash=$3
target_dir=$4
backup_dir=$5
result_path=$6
reboot_requested=${7:-false}
result_dir=$(dirname "$result_path")
mkdir -p "$result_dir"
plugin_path=/usr/lib/x86_64-linux-gnu/vpp_plugins/ly_route_orchestrator_plugin.so
plugin_payload=

write_result() {
  state=$1
  reason=${2:-}
  printf '{"state":"%s","reason":"%s"}\n' "$state" "$reason" >"$result_path"
}

fail() {
  reason=$1
  write_result failed "$reason"
  echo "$reason" >&2
  exit 1
}

capture_services() {
  mkdir -p "$backup_dir"
  : >"$backup_dir/services.state"
  for unit in ly-route-control-api.service ly-route-vpp-apply.service ly-route-runtime-check.service ly-route-recovery.service ly-route-firstboot.service vpp.service nginx.service; do
    state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
    [ -n "$state" ] || state=missing
    printf '%s|%s\n' "$unit" "$state" >>"$backup_dir/services.state"
  done
}

restore_services() {
  [ -f "$backup_dir/services.state" ] || return 0
  while IFS='|' read -r unit state; do
    [ -n "$unit" ] || continue
    current=$(systemctl is-enabled "$unit" 2>/dev/null || true)
    [ "$current" != "$state" ] || continue
    case "$state" in
      enabled) timeout 15 systemctl enable "$unit" >/dev/null 2>&1 || true ;;
      disabled) timeout 15 systemctl disable "$unit" >/dev/null 2>&1 || true ;;
      masked) timeout 15 systemctl mask "$unit" >/dev/null 2>&1 || true ;;
      *) : ;;
    esac
  done <"$backup_dir/services.state"
}

restore_files() {
  # Publish the failure before touching the live payload: even if a service
  # manager call blocks, the control task must not stay "installing".
  write_result failed "install failed; restoring backup"
  echo "restoring orchestrator payload from $backup_dir" >&2
  if [ -d "$backup_dir/target" ]; then
    rm -rf "$target_dir"
    cp -a "$backup_dir/target" "$target_dir"
  fi
  if [ -d "$backup_dir/admin" ]; then
    rm -rf /opt/ly-route/admin
    cp -a "$backup_dir/admin" /opt/ly-route/admin
  fi
  if [ -f "$backup_dir/ly-route-admin.conf" ]; then
    cp -a "$backup_dir/ly-route-admin.conf" /etc/nginx/conf.d/ly-route-admin.conf
  fi
  if [ -f "$backup_dir/ly-route-control-api.service" ]; then
    cp -a "$backup_dir/ly-route-control-api.service" /etc/systemd/system/ly-route-control-api.service
  fi
  if [ -f "$backup_dir/version.env" ]; then
    cp -a "$backup_dir/version.env" /etc/ly-route/version.env
  fi
  if [ -f "$backup_dir/ly-route-firstboot.service" ]; then
    cp -a "$backup_dir/ly-route-firstboot.service" /etc/systemd/system/ly-route-firstboot.service
  fi
  if [ -f "$backup_dir/ly_route_orchestrator_plugin.so" ]; then
    install -D -m 0644 "$backup_dir/ly_route_orchestrator_plugin.so" "$plugin_path"
  elif [ -f "$backup_dir/plugin.absent" ]; then
    rm -f "$plugin_path"
  fi
  restore_services
  if [ -f "$backup_dir/plugin.changed" ]; then
    timeout 60 systemctl restart vpp.service || true
  fi
  timeout 30 systemctl daemon-reload || true
  timeout 30 systemctl restart ly-route-control-api.service || true
  timeout 30 systemctl reload nginx.service || timeout 30 systemctl restart nginx.service || true
}

health_check() {
  healthy=0
  i=0
  while [ "$i" -lt 120 ]; do
    if systemctl is-active --quiet ly-route-control-api.service &&
       curl -fsS --max-time 2 http://127.0.0.1:8080/api/v1/health >/dev/null 2>&1; then
      healthy=1
      break
    fi
    i=$((i + 1))
    sleep 1
  done
  [ "$healthy" = 1 ]
}

if [ "$action" = rollback ]; then
  [ -d "$backup_dir" ] || fail "rollback backup is missing"
  restore_files
  if health_check; then
    write_result rolled_back "service enable state and control payload restored"
    exit 0
  fi
  write_result rollback_failed "health check failed after rollback"
  exit 1
fi

[ "$action" = install ] || fail "unsupported action $action"
actual=$(sha256sum -- "$package_path" | cut -d' ' -f1)
[ "$actual" = "$package_hash" ] || fail "package hash mismatch"
tmp=$(mktemp -d /var/lib/ly-route/orchestrator/upgrade-install.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
zstd -dc -- "$package_path" | tar -x -C "$tmp"
test -f "$tmp/manifest.json" || fail "manifest.json is required"
test -f "$tmp/checksums.sha256" || fail "checksums.sha256 is required"
grep -q '"package_type": "ly-route-upgrade"' "$tmp/manifest.json" || fail "not an upgrade package"
grep -q '"product": "orchestrator"' "$tmp/manifest.json" || fail "not an Orchestrator upgrade package"
(cd "$tmp" && sha256sum -c checksums.sha256) || fail "package checksum verification failed"
test -x "$tmp/usr/lib/ly-route/ly-route-control" || fail "control binary is missing"
test -x "$tmp/usr/lib/ly-route/vpp-apply" || fail "vpp-apply is missing"

# An Orchestrator upgrade must never silently downgrade the dataplane.  The
# base appliance owns the VPP orchestrator plugin; accept it either from the
# system or from a conversion payload explicitly carrying the plugin.
plugin_payload="$tmp/usr/lib/x86_64-linux-gnu/vpp_plugins/ly_route_orchestrator_plugin.so"
if [ ! -f "$plugin_path" ] && [ ! -f "$plugin_payload" ]; then
  fail "VPP orchestrator plugin is not installed and the payload does not include it"
fi

mkdir -p "$backup_dir"
capture_services
if [ -f "$plugin_path" ]; then
  cp -a "$plugin_path" "$backup_dir/ly_route_orchestrator_plugin.so"
  : >"$backup_dir/plugin.present"
else
  : >"$backup_dir/plugin.absent"
fi
if [ -e "$target_dir" ]; then cp -a "$target_dir" "$backup_dir/target"; fi
if [ -e /opt/ly-route/admin ]; then cp -a /opt/ly-route/admin "$backup_dir/admin"; fi
if [ -e /etc/nginx/conf.d/ly-route-admin.conf ]; then cp -a /etc/nginx/conf.d/ly-route-admin.conf "$backup_dir/ly-route-admin.conf"; fi
if [ -e /etc/systemd/system/ly-route-control-api.service ]; then cp -a /etc/systemd/system/ly-route-control-api.service "$backup_dir/ly-route-control-api.service"; fi
if [ -e /etc/ly-route/version.env ]; then cp -a /etc/ly-route/version.env "$backup_dir/version.env"; fi
if [ -e /etc/systemd/system/ly-route-firstboot.service ]; then cp -a /etc/systemd/system/ly-route-firstboot.service "$backup_dir/ly-route-firstboot.service"; fi

install -d -m 0755 "$target_dir"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-control" "$target_dir/ly-route-control"
install -m 0755 "$tmp/usr/lib/ly-route/vpp-apply" "$target_dir/vpp-apply"
if [ -f "$plugin_payload" ]; then
  if [ ! -f "$plugin_path" ] || ! cmp -s "$plugin_payload" "$plugin_path"; then
    install -D -m 0644 "$plugin_payload" "$plugin_path"
    : >"$backup_dir/plugin.changed"
  fi
fi
for runtime_script in runtime-check.sh firstboot.sh; do
  if [ -f "$tmp/usr/lib/ly-route/$runtime_script" ]; then
    install -m 0755 "$tmp/usr/lib/ly-route/$runtime_script" "$target_dir/$runtime_script"
  fi
done
if [ "$target_dir" = "/usr/lib/ly-route" ] && [ -f "$tmp/etc/systemd/system/ly-route-firstboot.service" ]; then
  install -D -m 0644 "$tmp/etc/systemd/system/ly-route-firstboot.service" /etc/systemd/system/ly-route-firstboot.service
fi
if [ "$target_dir" = "/usr/lib/ly-route" ] && [ -f "$tmp/etc/systemd/system/ly-route-control-api.service" ]; then
  install -D -m 0644 "$tmp/etc/systemd/system/ly-route-control-api.service" /etc/systemd/system/ly-route-control-api.service
fi
if [ -f "$tmp/etc/ly-route/version.env" ]; then
  install -D -m 0644 "$tmp/etc/ly-route/version.env" /etc/ly-route/version.env
fi
if [ -d "$tmp/opt/ly-route/admin" ]; then
  rm -rf /opt/ly-route/admin.new
  mkdir -p /opt/ly-route/admin.new
  cp -a "$tmp/opt/ly-route/admin/." /opt/ly-route/admin.new/
  rm -rf /opt/ly-route/admin
  mv /opt/ly-route/admin.new /opt/ly-route/admin
fi
if [ -f "$tmp/etc/nginx/conf.d/ly-route-admin.conf" ]; then
  install -D -m 0644 "$tmp/etc/nginx/conf.d/ly-route-admin.conf" /etc/nginx/conf.d/ly-route-admin.conf
fi
timeout 30 systemctl daemon-reload || true
if [ -f "$backup_dir/plugin.changed" ]; then
  if ! timeout 60 systemctl restart vpp.service; then
    restore_files
    fail "VPP restart failed"
  fi
fi
if ! timeout 30 systemctl restart ly-route-control-api.service; then
  restore_files
  fail "control API restart failed"
fi
timeout 30 systemctl reload nginx.service || timeout 30 systemctl restart nginx.service || true
if ! health_check; then
  restore_files
  fail "health check failed after install"
fi
write_result installed "orchestrator payload installed and health check passed"
if [ "$reboot_requested" = true ]; then
  systemctl reboot
fi
`

func orchestratorUpgradeScriptInvocation(action, packagePath, packageHash, targetDir, backupDir, resultPath string, reboot bool) orchestratorUpgradeInvocation {
	return orchestratorUpgradeInvocation{
		Name: "bash",
		Args: []string{"-c", orchestratorUpgradeScript, "ly-route-orchestrator-upgrade", action, packagePath, packageHash, targetDir, backupDir, resultPath, strconv.FormatBool(reboot)},
	}
}

func orchestratorUpgradeUnitName(taskID string) string {
	name := strings.ToLower(strings.TrimSpace(taskID))
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unknown"
	}
	return "ly-route-orch-upgrade-" + name
}

// startOrchestratorUpgradeUnit runs the installer outside the control-api
// service cgroup.  The installer restarts ly-route-control-api.service, and a
// plain child process would be terminated by systemd before it can write the
// result file or restore the backup.
func startOrchestratorUpgradeUnit(invocation orchestratorUpgradeInvocation) error {
	if strings.TrimSpace(invocation.Unit) == "" {
		return fmt.Errorf("orchestrator upgrade unit name is required")
	}
	unitArgs, err := orchestratorUpgradeSystemdRunArgs(invocation)
	if err != nil {
		return err
	}
	output, err := exec.Command("systemd-run", unitArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start detached orchestrator upgrade unit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func orchestratorUpgradeSystemdRunArgs(invocation orchestratorUpgradeInvocation) ([]string, error) {
	argv, err := orchestratorUpgradeUnitArgv(invocation)
	if err != nil {
		return nil, err
	}
	unitArgs := []string{
		"--collect",
		"--no-block",
		"--unit", invocation.Unit,
		"--property=Type=oneshot",
		"--property=TimeoutStartSec=300",
		"--property=StandardOutput=journal",
		"--property=StandardError=journal",
		"--",
	}
	return append(unitArgs, argv...), nil
}

func orchestratorUpgradeUnitArgv(invocation orchestratorUpgradeInvocation) ([]string, error) {
	name := invocation.Name
	args := append([]string(nil), invocation.Args...)
	// systemd-run writes its command line into a transient unit file.  A
	// multi-line `bash -c` payload would be parsed as unit directives, so
	// materialize the installer script and pass only its path to systemd.
	if name == "bash" && len(args) >= 3 && args[0] == "-c" {
		scriptPath := filepath.Join(os.TempDir(), invocation.Unit+".sh")
		if err := os.WriteFile(scriptPath, []byte(args[1]), 0o700); err != nil {
			return nil, fmt.Errorf("write detached orchestrator upgrade script: %w", err)
		}
		name = "bash"
		args = append([]string{scriptPath}, args[3:]...)
	}
	return append([]string{name}, args...), nil
}
