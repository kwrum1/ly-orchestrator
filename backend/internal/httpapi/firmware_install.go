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

stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup=/var/lib/ly-route/firmware-update/backup-$stamp
mkdir -p "$backup"
if [ -e "$target_dir" ]; then cp -a "$target_dir" "$backup/target"; fi
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

restore_backup() {
  echo "upgrade failed; restoring $backup" >&2
  if [ -e "$backup/target" ]; then
    rm -rf "$target_dir"
    cp -a "$backup/target" "$target_dir"
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
  systemctl daemon-reload || true
  systemctl restart ly-route-control-api.service || true
  systemctl reload nginx.service || systemctl restart nginx.service || true
  mkdir -p /var/lib/ly-route/firmware-update
  printf '{"timestamp":"%s","backup":"%s","reason":"upgrade health check failed"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$backup" \
    >> /var/lib/ly-route/firmware-update/rollback-audit.jsonl
}

install -d -m 0755 "$target_dir"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-control" "$target_dir/ly-route-control"
install -m 0755 "$tmp/usr/lib/ly-route/vpp-apply" "$target_dir/vpp-apply"
install -D -m 0755 "$dataplane_plugin" "/$dataplane_plugin_rel"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-pppoe-plugin" "$target_dir/ly-route-pppoe-plugin"
if [ -f "$tmp/usr/lib/ly-route/recover-runtime.sh" ]; then
  install -m 0755 "$tmp/usr/lib/ly-route/recover-runtime.sh" "$target_dir/recover-runtime.sh"
fi
for runtime_script in runtime-check.sh firstboot.sh; do
  if [ -f "$tmp/usr/lib/ly-route/$runtime_script" ]; then
    install -m 0755 "$tmp/usr/lib/ly-route/$runtime_script" "$target_dir/$runtime_script"
  fi
done
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
systemctl daemon-reload || true
if ! timeout 30s systemctl restart ly-route-control-api.service; then
  restore_backup
  exit 1
fi
systemctl reload nginx.service || systemctl restart nginx.service || true

healthy=0
i=0
while [ "$i" -lt 60 ]; do
  if timeout 3s systemctl is-active --quiet ly-route-control-api.service &&
     timeout 3s curl -fsS --max-time 2 http://127.0.0.1:8080/api/v1/health >/dev/null 2>&1; then
    healthy=1
    break
  fi
  i=$((i + 1))
  sleep 1
done
if [ "$healthy" != 1 ]; then
  restore_backup
  echo "upgrade health check failed" >&2
  exit 1
fi

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
