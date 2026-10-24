package httpapi

import "strconv"

type firmwareInstallInvocation struct {
	Name string
	Args []string
}

const firmwareUpgradeInstallScript = `
set -eu
exec >>/var/log/ly-route-firmware-install.log 2>&1
package_path=$1
package_hash=$2
target_dir=$3
reboot_requested=$4
install_started=$(date +%s)
# Bound the complete install+health+restore decision. The normal health
# window stays 60s below; this deadline prevents a crashing control binary
# from extending a failed install into an eight-minute crash loop.
install_deadline=$((install_started + 180))
upgrade_dropin=/run/systemd/system/ly-route-control-api.service.d/90-upgrade.conf
remove_upgrade_dropin() {
  rm -f "$upgrade_dropin"
  rmdir /run/systemd/system/ly-route-control-api.service.d 2>/dev/null || true
  timeout 5s systemctl daemon-reload || true
}
trap remove_upgrade_dropin EXIT
actual=$(sha256sum -- "$package_path" | cut -d' ' -f1)
test "$actual" = "$package_hash"
tmp=$(mktemp -d /var/lib/ly-route/firmware-update/install.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
zstd -dc -- "$package_path" | tar -x -C "$tmp"
test -f "$tmp/manifest.json"
test -f "$tmp/checksums.sha256"
grep -q '"package_type": "ly-route-upgrade"' "$tmp/manifest.json"
grep -q '"version"' "$tmp/manifest.json"
(cd "$tmp" && sha256sum -c checksums.sha256)
test -x "$tmp/usr/lib/ly-route/ly-route-control"
test -x "$tmp/usr/lib/ly-route/vpp-apply"

# A control-plane-only payload would silently roll the device back to the old
# dataplane.  Require both the native VPP PPPoE plugin and the explicit
# manifest claim; a manifest claim without the file is not installable.
case "$(uname -m)" in
  aarch64|arm64) dataplane_plugin_rel="usr/lib/aarch64-linux-gnu/vpp_plugins/pppoeclient_plugin.so" ;;
  *) dataplane_plugin_rel="usr/lib/x86_64-linux-gnu/vpp_plugins/pppoeclient_plugin.so" ;;
esac
dataplane_plugin="$tmp/$dataplane_plugin_rel"
if [ ! -f "$dataplane_plugin" ] ||
   ! grep -q '"includes_dataplane"[[:space:]]*:[[:space:]]*true' "$tmp/manifest.json"; then
  echo "refusing upgrade: payload does not include the VPP/PPPoE dataplane" >&2
  exit 1
fi
test -f "$tmp/usr/lib/ly-route/ly-route-pppoe-plugin"

record_firmware_audit() {
  status=$1
  reason=$2
  backup_path=$3
  python3 - "$status" "$reason" "$backup_path" <<'AUDIT_PY' || true
import sqlite3, sys, uuid
from datetime import datetime, timezone
status, reason, backup = sys.argv[1:4]
now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
try:
    db = sqlite3.connect("/var/lib/ly-route/gateway/ly-route.db", timeout=30)
    db.execute(
        "insert or replace into audit_events "
        "(id,timestamp,actor,role,resource,action,status,error,transaction_id) "
        "values (?,?,?,?,?,?,?,?,?)",
        ("firmware-install-" + str(uuid.uuid4()), now, "system", "system",
         "/api/v1/firmware/update/install", "firmware_install", status,
         reason + (" backup=" + backup if backup else ""), None),
    )
    db.commit()
    db.close()
except Exception as exc:
    print("firmware audit insert failed: " + str(exc), file=sys.stderr)
AUDIT_PY
}

stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup=/var/lib/ly-route/firmware-update/backup-$stamp
mkdir -p "$backup"
if [ -e "$target_dir" ]; then
  cp -al "$target_dir" "$backup/target" 2>/dev/null || cp -a "$target_dir" "$backup/target"
fi
if [ -e /opt/ly-route/admin ]; then cp -a /opt/ly-route/admin "$backup/admin"; fi
if [ -e /etc/nginx/conf.d/ly-route-admin.conf ]; then cp -a /etc/nginx/conf.d/ly-route-admin.conf "$backup/ly-route-admin.conf"; fi
if [ -e /etc/systemd/system/ly-route-firstboot.service ]; then cp -a /etc/systemd/system/ly-route-firstboot.service "$backup/ly-route-firstboot.service"; fi
if [ -e "/$dataplane_plugin_rel" ]; then
  cp -a "/$dataplane_plugin_rel" "$backup/plugin"
fi
if [ -e /usr/lib/ly-route/ly-route-pppoe-plugin ]; then
  cp -a /usr/lib/ly-route/ly-route-pppoe-plugin "$backup/helper"
fi
if [ -e /usr/lib/ly-route/recover-runtime.sh ]; then
  cp -a /usr/lib/ly-route/recover-runtime.sh "$backup/recover"
fi
if [ -e /usr/share/ly-route/artifact-manifest.json ]; then
  cp -a /usr/share/ly-route/artifact-manifest.json "$backup/artifact-manifest.json"
fi
# Proxy fail-closed assets: helper, failure-page actuator, page, vhost and the
# xray drop-in that wires them. They are backed up individually so a failed
# upgrade restores the exact previous fail-closed posture.
for proxy_path in \
  usr/lib/ly-route/proxy-failopen \
  usr/lib/ly-route/proxy-failure \
  etc/nginx/conf.d/ly-route-proxy-failure.conf \
  etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf; do
  if [ -e "/$proxy_path" ]; then
    cp -a "/$proxy_path" "$backup/proxy-$(printf '%s' "$proxy_path" | tr '/' '_')"
  fi
done
if [ -e /usr/share/ly-route/proxy-failure ]; then
  cp -a /usr/share/ly-route/proxy-failure "$backup/proxy-page"
fi

restore_backup() {
  echo "upgrade failed; restoring $backup" >&2
  if [ -e "$backup/target" ]; then
    # Swap the restored tree into place by rename. Removing and copying a
    # 32MB /usr/lib/ly-route tree takes minutes on a loaded TCG lab; the
    # backup is already hardlinked, so this keeps rollback fast.
    restore_tmp="${target_dir}.restore-$$"
    failed_tmp="${target_dir}.failed-$$"
    rm -rf "$restore_tmp"
    if cp -al "$backup/target" "$restore_tmp" 2>/dev/null || cp -a "$backup/target" "$restore_tmp"; then
      if mv "$target_dir" "$failed_tmp" 2>/dev/null; then
        mv "$restore_tmp" "$target_dir"
        (rm -rf "$failed_tmp" >/dev/null 2>&1 &)
      else
        rm -rf "$target_dir"
        mv "$restore_tmp" "$target_dir"
      fi
    fi
  fi
  if [ -e "$backup/admin" ]; then
    rm -rf /opt/ly-route/admin
    cp -a "$backup/admin" /opt/ly-route/admin
  fi
  if [ -e "$backup/ly-route-admin.conf" ]; then
    cp -a "$backup/ly-route-admin.conf" /etc/nginx/conf.d/ly-route-admin.conf
  fi
  if [ -e "$backup/ly-route-firstboot.service" ]; then
    cp -a "$backup/ly-route-firstboot.service" /etc/systemd/system/ly-route-firstboot.service
  fi
  if [ -e "$backup/plugin" ]; then
    install -D -m 0755 "$backup/plugin" "/$dataplane_plugin_rel"
  fi
  if [ -e "$backup/helper" ]; then
    install -m 0755 "$backup/helper" /usr/lib/ly-route/ly-route-pppoe-plugin
  fi
  if [ -e "$backup/recover" ]; then
    install -m 0755 "$backup/recover" /usr/lib/ly-route/recover-runtime.sh
  fi
  if [ -e "$backup/artifact-manifest.json" ]; then
    install -D -m 0644 "$backup/artifact-manifest.json" /usr/share/ly-route/artifact-manifest.json
  fi
  for proxy_path in \
    usr/lib/ly-route/proxy-failopen \
    usr/lib/ly-route/proxy-failure \
    etc/nginx/conf.d/ly-route-proxy-failure.conf \
    etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf; do
    proxy_backup="$backup/proxy-$(printf '%s' "$proxy_path" | tr '/' '_')"
    if [ -e "$proxy_backup" ]; then
      install -D -m 0755 "$proxy_backup" "/$proxy_path"
    else
      rm -f "/$proxy_path"
    fi
  done
  if [ -e "$backup/proxy-page" ]; then
    rm -rf /usr/share/ly-route/proxy-failure
    cp -a "$backup/proxy-page" /usr/share/ly-route/proxy-failure
  else
    rm -rf /usr/share/ly-route/proxy-failure
  fi
  # Stop the candidate before restoring files. If it is crash-looping, a
  # plain restart can hold the install open for minutes and keep spawning
  # failures. Bound the stop/start as well so restore itself stays fast.
  timeout 5s systemctl stop --no-block ly-route-control-api.service || true
  timeout 2s systemctl kill -s KILL ly-route-control-api.service || true
  remove_upgrade_dropin
  timeout 5s systemctl reset-failed ly-route-control-api.service || true
  # The control process is stopped here, so the audit row cannot race the
  # service's database writers. Then start the restored control.
  mkdir -p /var/lib/ly-route/firmware-update
  printf '{"timestamp":"%s","backup":"%s","reason":"upgrade health check failed"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$backup" \
    >> /var/lib/ly-route/firmware-update/rollback-audit.jsonl
  record_firmware_audit failure "upgrade health check failed" "$backup"
  timeout 8s systemctl start --no-block ly-route-control-api.service || true
  timeout 5s systemctl reload nginx.service || timeout 5s systemctl restart nginx.service || true
}

install -d -m 0755 "$target_dir"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-control" "$target_dir/ly-route-control"
install -m 0755 "$tmp/usr/lib/ly-route/vpp-apply" "$target_dir/vpp-apply"
install -D -m 0755 "$dataplane_plugin" "/$dataplane_plugin_rel"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-pppoe-plugin" "$target_dir/ly-route-pppoe-plugin"
if [ -f "$tmp/usr/lib/ly-route/recover-runtime.sh" ]; then
  install -m 0755 "$tmp/usr/lib/ly-route/recover-runtime.sh" "$target_dir/recover-runtime.sh"
fi
for runtime_script in runtime-check.sh firstboot.sh proxy-failopen proxy-failure; do
  if [ -f "$tmp/usr/lib/ly-route/$runtime_script" ]; then
    install -m 0755 "$tmp/usr/lib/ly-route/$runtime_script" "$target_dir/$runtime_script"
  fi
done
if [ -f "$tmp/etc/nginx/conf.d/ly-route-proxy-failure.conf" ]; then
  install -D -m 0644 "$tmp/etc/nginx/conf.d/ly-route-proxy-failure.conf" \
    /etc/nginx/conf.d/ly-route-proxy-failure.conf
fi
if [ -d "$tmp/usr/share/ly-route/proxy-failure" ]; then
  rm -rf /usr/share/ly-route/proxy-failure.new
  mkdir -p /usr/share/ly-route/proxy-failure.new
  cp -a "$tmp/usr/share/ly-route/proxy-failure/." /usr/share/ly-route/proxy-failure.new/
  rm -rf /usr/share/ly-route/proxy-failure
  mv /usr/share/ly-route/proxy-failure.new /usr/share/ly-route/proxy-failure
fi
if [ -f "$tmp/etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf" ]; then
  install -D -m 0644 "$tmp/etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf" \
    /etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf
fi
if [ "$target_dir" = "/usr/lib/ly-route" ] && [ -f "$tmp/etc/systemd/system/ly-route-firstboot.service" ]; then
  install -D -m 0644 "$tmp/etc/systemd/system/ly-route-firstboot.service" /etc/systemd/system/ly-route-firstboot.service
fi
if [ -f "$tmp/usr/share/ly-route/artifact-manifest.json" ]; then
  install -D -m 0644 "$tmp/usr/share/ly-route/artifact-manifest.json" /usr/share/ly-route/artifact-manifest.json
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
timeout 5s systemctl daemon-reload || true
restart_base=$(timeout 3s systemctl show ly-route-control-api.service -p NRestarts --value 2>/dev/null || echo 0)
case "$restart_base" in ''|*[!0-9]*) restart_base=0;; esac
# During candidate validation do not let systemd respawn a broken control
# indefinitely. This runtime-only drop-in is removed on success or rollback,
# restoring the normal Restart=on-failure policy.
mkdir -p "$(dirname "$upgrade_dropin")"
printf '[Service]\nRestart=no\n' > "$upgrade_dropin"
timeout 5s systemctl daemon-reload || true
restart_timeout=$((install_deadline - $(date +%s)))
if [ "$restart_timeout" -lt 15 ]; then
  restore_backup
  echo "upgrade deadline reached before candidate restart" >&2
  exit 1
fi
if [ "$restart_timeout" -gt 60 ]; then restart_timeout=60; fi
# A graceful restart is normally enough, but a stuck control stop job must not
# consume the whole deadline. Stop, force-kill if necessary, then start the
# candidate with a bounded job.
timeout 5s systemctl stop --no-block ly-route-control-api.service || true
pkill -9 -f "^/usr/lib/ly-route/ly-route-control$" 2>/dev/null || true
timeout 5s systemctl reset-failed ly-route-control-api.service || true
# systemd may queue the start behind other jobs; --no-block plus a short
# command timeout lets the health deadline, not a stuck D-Bus call, decide.
timeout 8s systemctl start --no-block ly-route-control-api.service || true
timeout 5s systemctl reload nginx.service || timeout 5s systemctl restart nginx.service || true

healthy=0
inactive_probes=0
health_started=$(date +%s)
health_deadline=$((health_started + 60))
while :; do
  now=$(date +%s)
  if [ "$now" -ge "$install_deadline" ] || [ "$now" -ge "$health_deadline" ]; then
    break
  fi
  if timeout 3s systemctl is-active --quiet ly-route-control-api.service &&
     timeout 12s curl -fsS --max-time 10 http://127.0.0.1:8080/api/v1/health >/dev/null 2>&1; then
    healthy=1
    break
  fi
  restarts=$(timeout 3s systemctl show ly-route-control-api.service -p NRestarts --value 2>/dev/null || echo 0)
  case "$restarts" in ''|*[!0-9]*) restarts=0;; esac
  if [ "$restarts" -ge $((restart_base + 3)) ]; then
    echo "candidate control crash-loop detected: NRestarts $restart_base->$restarts" >&2
    break
  fi
  active_state=$(timeout 3s systemctl show ly-route-control-api.service -p ActiveState --value 2>/dev/null || true)
  if [ "$active_state" = "failed" ] || [ "$active_state" = "inactive" ]; then
    inactive_probes=$((inactive_probes + 1))
    if [ "$inactive_probes" -ge 3 ]; then
      echo "candidate control stayed inactive for 3 probes" >&2
      break
    fi
  else
    inactive_probes=0
  fi
  sleep 1
done
if [ "$healthy" != 1 ]; then
  restore_backup
  if [ "$(date +%s)" -ge "$install_deadline" ]; then
    echo "upgrade health check failed (total deadline)" >&2
  else
    echo "upgrade health check failed" >&2
  fi
  exit 1
fi
remove_upgrade_dropin
record_firmware_audit success "upgrade installed" "$backup"

if [ "$reboot_requested" = true ]; then
  # Request a graceful reboot; never force it while the new control API has
  # just been verified.
  systemctl reboot
fi
`

func firmwareUpgradeInstallInvocation(packagePath, packageHash, targetDir string, reboot bool) firmwareInstallInvocation {
	return firmwareInstallInvocation{
		Name: "bash",
		Args: []string{"-c", firmwareUpgradeInstallScript, "ly-route-upgrade-installer", packagePath, packageHash, targetDir, strconv.FormatBool(reboot)},
	}
}
