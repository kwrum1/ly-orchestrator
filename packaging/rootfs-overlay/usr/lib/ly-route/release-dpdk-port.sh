#!/bin/sh
set -eu

# Return one DPDK-owned data port to its original kernel driver and remove its
# startup entry. Used when a destructive takeover candidate fails verification:
# the port must be usable by the next candidate (AF_XDP/AF_PACKET) instead of
# being left in a half-bound dead state.

fail() {
  printf '%s\n' "DPDK release failed: $*" >&2
  exit 1
}

pci=${1:-}
case "$pci" in
  [0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f]:[0-9A-Fa-f][0-9A-Fa-f]:[0-9A-Fa-f][0-9A-Fa-f].[0-7]) ;;
  *) fail "invalid PCI address '$pci'" ;;
esac

sysfs=${LY_ROUTE_SYSFS_ROOT:-/sys}
ownership=${LY_ROUTE_DPDK_OWNERSHIP_STATE:-/var/lib/ly-route/dataplane/ownership.json}
startup=${LY_ROUTE_VPP_STARTUP_CONFIG:-/etc/vpp/startup.conf}
bind_helper=${LY_ROUTE_PCI_BIND_HELPER:-}

driver_name() {
  basename "$(readlink -f "$sysfs/bus/pci/devices/$1/driver" 2>/dev/null || true)"
}

original_driver=$(python3 - "$ownership" "$pci" <<'PY'
import json
import sys

path, pci = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, ValueError):
    raise SystemExit(0)
for device in document.get("devices", []) or []:
    if not isinstance(device, dict):
        continue
    if str(device.get("pci_address", "")).strip() == pci:
        print(str(device.get("kernel_driver", "")).strip())
        break
PY
)
[ -n "$original_driver" ] || {
  inventory=${LY_ROUTE_DATAPLANE_INVENTORY:-/etc/ly-route/dataplane-inventory.json}
  original_driver=$(python3 - "$inventory" "$pci" <<'PY'
import json
import sys

path, pci = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, ValueError):
    raise SystemExit(0)
for interface in document.get("data_interfaces") or []:
    if isinstance(interface, dict) and str(interface.get("pci", "")).strip() == pci:
        print(str(interface.get("kernel_driver", "")).strip())
        break
PY
  )
}
[ -n "$original_driver" ] || fail "$pci has no recorded original kernel driver"

current_driver=$(driver_name "$pci")
if [ "$current_driver" = vfio-pci ] || [ "$current_driver" = uio_pci_generic ]; then
  if [ -n "$bind_helper" ]; then
    "$bind_helper" unbind "$pci" "$current_driver" || fail "$pci cannot unbind $current_driver"
  else
    printf '%s\n' "$pci" > "$sysfs/bus/pci/drivers/$current_driver/unbind" || \
      fail "$pci cannot unbind $current_driver"
  fi
fi
if [ -n "$bind_helper" ]; then
  "$bind_helper" bind "$pci" "$original_driver" || fail "$pci cannot restore $original_driver"
else
  [ -d "$sysfs/bus/pci/drivers/$original_driver" ] || fail "kernel driver $original_driver is unavailable"
  printf '%s\n' "$original_driver" > "$sysfs/bus/pci/devices/$pci/driver_override" || \
    fail "$pci cannot set $original_driver driver override"
  printf '%s\n' "$pci" > "$sysfs/bus/pci/drivers/$original_driver/bind" || \
    fail "$pci cannot bind $original_driver"
fi
[ "$(driver_name "$pci")" = "$original_driver" ] || \
  fail "$pci did not return to $original_driver"

if [ -f "$startup" ]; then
  sed -i "\|^  dev $pci\$|d" "$startup"
  if ! sed -n '/^# BEGIN LY ROUTE DPDK$/,/^# END LY ROUTE DPDK$/p' "$startup" | grep -q '^  dev '; then
    sed -i '/^# BEGIN LY ROUTE DPDK$/,/^# END LY ROUTE DPDK$/d' "$startup"
    if grep -q 'plugin dpdk_plugin.so { enable }' "$startup"; then
      sed -i 's/plugin dpdk_plugin\.so { enable }/plugin dpdk_plugin.so { disable }/' "$startup"
    fi
  fi
fi

python3 - "$ownership" "$pci" <<'PY'
import json
import os
import sys
import tempfile

path, pci = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, ValueError):
    raise SystemExit(0)
devices = [item for item in document.get("devices", []) or [] if not isinstance(item, dict) or str(item.get("pci_address", "")) != pci]
document["devices"] = devices
if not devices:
    try:
        os.remove(path)
    except OSError:
        pass
    raise SystemExit(0)
directory = os.path.dirname(path) or "."
descriptor, temporary = tempfile.mkstemp(prefix=".ownership-", dir=directory)
with os.fdopen(descriptor, "w", encoding="utf-8") as target:
    json.dump(document, target, indent=2)
    target.write("\n")
os.chmod(temporary, 0o600)
os.replace(temporary, path)
PY

verification=${LY_ROUTE_DPDK_VERIFICATION_STATE:-/var/lib/ly-route/dataplane/dpdk-verification.json}
python3 - "$verification" "$pci" <<'PY'
import json
import os
import sys

path, pci = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, ValueError):
    raise SystemExit(0)
document["interfaces"] = [item for item in document.get("interfaces", []) or [] if not isinstance(item, dict) or str(item.get("pci_address", "")) != pci]
if not document["interfaces"]:
    try:
        os.remove(path)
    except OSError:
        pass
else:
    with open(path, "w", encoding="utf-8") as target:
        json.dump(document, target, indent=2)
        target.write("\n")
PY

printf '%s\n' "DPDK released: $pci $current_driver -> $original_driver"
