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
# dataplane.  Require the native VPP PPPoE plugin (or an explicit manifest
# claim that the payload includes the dataplane) before touching the system.
if [ ! -f "$tmp/usr/lib/x86_64-linux-gnu/vpp_plugins/pppoeclient_plugin.so" ] &&
   ! grep -q '"includes_dataplane"[[:space:]]*:[[:space:]]*true' "$tmp/manifest.json"; then
  echo "refusing upgrade: payload does not include the VPP/PPPoE dataplane" >&2
  exit 1
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup=/var/lib/ly-route/firmware-update/backup-$stamp
mkdir -p "$backup"
if [ -e "$target_dir" ]; then cp -a "$target_dir" "$backup/target"; fi
if [ -e /opt/ly-route/admin ]; then cp -a /opt/ly-route/admin "$backup/admin"; fi
if [ -e /etc/nginx/conf.d/ly-route-admin.conf ]; then cp -a /etc/nginx/conf.d/ly-route-admin.conf "$backup/ly-route-admin.conf"; fi
if [ -e /etc/systemd/system/ly-route-firstboot.service ]; then cp -a /etc/systemd/system/ly-route-firstboot.service "$backup/ly-route-firstboot.service"; fi

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
  systemctl daemon-reload || true
  systemctl restart ly-route-control-api.service || true
  systemctl reload nginx.service || systemctl restart nginx.service || true
}

install -d -m 0755 "$target_dir"
install -m 0755 "$tmp/usr/lib/ly-route/ly-route-control" "$target_dir/ly-route-control"
install -m 0755 "$tmp/usr/lib/ly-route/vpp-apply" "$target_dir/vpp-apply"
for runtime_script in runtime-check.sh firstboot.sh; do
  if [ -f "$tmp/usr/lib/ly-route/$runtime_script" ]; then
    install -m 0755 "$tmp/usr/lib/ly-route/$runtime_script" "$target_dir/$runtime_script"
  fi
done
if [ "$target_dir" = "/usr/lib/ly-route" ] && [ -f "$tmp/etc/systemd/system/ly-route-firstboot.service" ]; then
  install -D -m 0644 "$tmp/etc/systemd/system/ly-route-firstboot.service" /etc/systemd/system/ly-route-firstboot.service
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
if ! systemctl restart ly-route-control-api.service; then
  restore_backup
  exit 1
fi
systemctl reload nginx.service || systemctl restart nginx.service || true

healthy=0
i=0
while [ "$i" -lt 30 ]; do
  if systemctl is-active --quiet ly-route-control-api.service &&
     curl -fsS --max-time 2 http://127.0.0.1:8080/api/v1/health >/dev/null 2>&1; then
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
