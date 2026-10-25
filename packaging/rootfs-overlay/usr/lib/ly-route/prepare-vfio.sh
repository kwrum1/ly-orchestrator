#!/bin/sh
set -eu

# The ownership preflight is the only place that may move a physical data NIC
# from the kernel driver to a DPDK binding before VPP starts. It binds VFIO
# where a device has an isolated IOMMU group and uio_pci_generic where it does
# not, so a single appliance can mix both transports. The management PCI is
# never touched.
#
# Inputs (in precedence order):
#   1. /etc/ly-route/vfio-devices ......... explicit rows: name|mac|pci|role
#   2. /etc/ly-route/dataplane-inventory.json
#      Persisted by the control API from operator-configured interfaces. It is
#      the normal source when the product (rather than the installer) selected
#      the data ports.
#   3. /etc/ly-route/installed-network.json
#      Installer mapping written before first boot.
#
# Outputs:
#   - the selected PCI devices are bound to vfio-pci (preferred) or
#     uio_pci_generic (no IOMMU fallback) and the binding is verified;
#   - /etc/vpp/startup.conf receives the managed `dpdk { dev ... }` block;
#   - /var/lib/ly-route/dataplane/ownership.json records which device the
#     product bound, its original kernel driver and whether the product created
#     that ownership. Rollback uses this record to return the NIC to Linux.
#
# The script fails closed: an unverified binding or an ambiguous mix of IOMMU
# and non-IOMMU devices is a hard error, never an optimistic VPP start.

fail() {
  printf '%s\n' "VPP ownership locked: $*" >&2
  exit 1
}

log() {
  printf '%s\n' "$*"
}

sysfs=${LY_ROUTE_SYSFS_ROOT:-/sys}
rows_file=${LY_ROUTE_VFIO_DEVICES:-/etc/ly-route/vfio-devices}
inventory=${LY_ROUTE_DATAPLANE_INVENTORY:-/etc/ly-route/dataplane-inventory.json}
network=${LY_ROUTE_INSTALLER_NETWORK:-/etc/ly-route/installed-network.json}
startup=${LY_ROUTE_VPP_STARTUP_CONFIG:-/etc/vpp/startup.conf}
ownership=${LY_ROUTE_DPDK_OWNERSHIP_STATE:-/var/lib/ly-route/dataplane/ownership.json}
active_state=${LY_ROUTE_DATAPLANE_ACTIVE_STATE:-/var/lib/ly-route/dataplane/active.json}
verification_state=${LY_ROUTE_DPDK_VERIFICATION_STATE:-/var/lib/ly-route/dataplane/dpdk-verification.json}
bootstrap_mode=${LY_ROUTE_VFIO_BOOTSTRAP:-}
python_bin=${LY_ROUTE_PYTHON:-python3}
bind_helper=${LY_ROUTE_PCI_BIND_HELPER:-}

modprobe vfio >/dev/null 2>&1 || true
modprobe vfio-pci >/dev/null 2>&1 || true
modprobe uio_pci_generic >/dev/null 2>&1 || true

management_pci=
pci_rows=
inventory_source=
if [ -r "$rows_file" ]; then
  management_pci=$(awk -F'|' '$4 == "management" {print $3; exit}' "$rows_file")
  pci_rows=$(awk -F'|' '$4 == "data" && $3 != "" {print $3}' "$rows_file")
  inventory_source=vfio-devices
elif [ -r "$inventory" ]; then
  inventory_rows=$(python3 - "$inventory" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    document = json.load(source)
if not isinstance(document, dict):
    raise SystemExit(1)
management = document.get("management")
if not isinstance(management, dict):
    management = {}
print("management|" + str(management.get("linux_interface", "")).strip() + "|" +
      str(management.get("pci", "")).strip() + "|" + str(management.get("mac", "")).strip())
data_interfaces = document.get("data_interfaces")
if not isinstance(data_interfaces, list):
    data_interfaces = []
for item in data_interfaces:
    if not isinstance(item, dict):
        continue
    pci = str(item.get("pci", "")).strip()
    if not pci:
        continue
    print("data|" + str(item.get("linux_interface", "")).strip() + "|" + pci + "|" +
          str(item.get("kernel_driver", "")).strip() + "|" + str(item.get("mac", "")).strip())
PY
  ) || fail 'operator dataplane interface inventory is invalid'
  management_pci=$(printf '%s\n' "$inventory_rows" | awk -F'|' '$1 == "management" {print $3; exit}')
  pci_rows=$(printf '%s\n' "$inventory_rows" | awk -F'|' '$1 == "data" && $3 != "" {print $3}')
  inventory_source=control-api-inventory
elif [ -r "$network" ]; then
  network_rows=$(python3 - "$network" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    document = json.load(source)
management = document.get("management", {})
print("management|" + str(management.get("pci", "")).strip())
data_interfaces = document.get("data_interfaces")
if not isinstance(data_interfaces, list):
    data_interfaces = []
for interface in data_interfaces:
    selected = interface.get("selected")
    if not isinstance(selected, dict):
        continue
    if selected.get("tier") == "vpp_dpdk" and selected.get("hook") == "dpdk":
        print("data|" + str(interface.get("pci", "")).strip())
PY
  ) || fail 'installer NIC mapping is invalid'
  management_pci=$(printf '%s\n' "$network_rows" | awk -F'|' '$1 == "management" {print $2; exit}')
  pci_rows=$(printf '%s\n' "$network_rows" | awk -F'|' '$1 == "data" && $2 != "" {print $2}')
  inventory_source=installer
else
  log 'VPP ownership preflight skipped: no operator interface inventory or installer NIC mapping'
  exit 0
fi

# Boot-mode ownership is evidence-based: only ports that were already
# verified/committed as DPDK are re-bound. The control API's apply bootstrap
# sets LY_ROUTE_VFIO_BOOTSTRAP=apply to test new candidates transactionally.
if [ "$bootstrap_mode" != apply ]; then
  verified_pcis=
  if [ -r "$active_state" ]; then
    verified_pcis=$(python3 - "$active_state" "$pci_rows" <<'PY'
import json
import re
import sys

path, rows = sys.argv[1], sys.argv[2].split()
pattern = re.compile(r"^[0-9A-Fa-f]{4}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-7]$")
try:
    with open(path, encoding="utf-8") as source:
        state = json.load(source)
    attachments = state.get("path", {}).get("attachments") or []
except (OSError, ValueError, AttributeError):
    attachments = []
for item in attachments:
    if not isinstance(item, dict):
        continue
    if item.get("tier") != "vpp_dpdk":
        continue
    if item.get("mode") not in {"vfio_pci", "uio_pci_generic"}:
        continue
    pci = str(item.get("pci_address", "")).strip()
    if pattern.fullmatch(pci) and pci in rows:
        print(pci)
PY
    ) || verified_pcis=
  fi
  pci_rows=$(printf '%s\n' "$verified_pcis" | awk 'NF')
fi

if [ -z "$pci_rows" ]; then
  # VMnet/AF_PACKET and native paths do not hand PCI devices to VFIO/UIO. A
  # MAC-only installer mapping has no management PCI, and that must not block
  # VPP startup when there is no verified DPDK data interface to take over.
  # Always remove a stale managed block so a failed takeover cannot survive a
  # reboot.
  if [ -f "$startup" ]; then
    sed -i '/^# BEGIN LY ROUTE DPDK$/,/^# END LY ROUTE DPDK$/d' "$startup"
    if grep -q 'plugin dpdk_plugin.so { enable }' "$startup"; then
      sed -i 's/plugin dpdk_plugin\.so { enable }/plugin dpdk_plugin.so { disable }/' "$startup"
    fi
  fi
  rm -f "$verification_state"
  if [ "$bootstrap_mode" = apply ]; then
    log 'VPP ownership preflight: no DPDK data interface; Linux retains NIC drivers'
  else
    log 'VPP ownership preflight: no verified DPDK owner; Linux retains NIC drivers'
  fi
  exit 0
fi
[ -n "$management_pci" ] || fail 'management PCI identity is missing'

is_selected() {
  candidate=$1
  case " $pci_rows " in
    *" $candidate "*) return 0 ;;
    *) return 1 ;;
  esac
}

driver_name() {
  device=$1
  basename "$(readlink -f "$sysfs/bus/pci/devices/$device/driver" 2>/dev/null || true)"
}

interface_name() {
  device=$1
  for net in "$sysfs/bus/pci/devices/$device/net/"*; do
    [ -e "$net" ] || continue
    basename "$net"
    return 0
  done
  return 1
}

interface_mac() {
  device=$1
  iface=$(interface_name "$device" || true)
  if [ -n "$iface" ] && [ -r "$sysfs/class/net/$iface/address" ]; then
    tr 'A-F' 'a-f' < "$sysfs/class/net/$iface/address"
    return 0
  fi
  return 1
}

inventory_value() {
  # inventory_value PCI FIELD -> prints the configured value, if present.
  pci=$1
  field=$2
  case "$inventory_source" in
    vfio-devices)
      awk -F'|' -v pci="$pci" -v field="$field" '$3 == pci && $4 == "data" {
        if (field == "linux_interface") print $1;
        else if (field == "mac") print $2;
        else if (field == "kernel_driver") print $5;
        exit
      }' "$rows_file"
      ;;
    control-api-inventory)
      printf '%s\n' "$inventory_rows" | awk -F'|' -v pci="$pci" -v field="$field" '$1 == "data" && $3 == pci {
        if (field == "linux_interface") print $2;
        else if (field == "kernel_driver") print $4;
        else if (field == "mac") print $5;
        exit
      }'
      ;;
    installer)
      python3 - "$network" "$pci" "$field" <<'PY'
import json
import sys

path, pci, field = sys.argv[1:]
with open(path, encoding="utf-8") as source:
    document = json.load(source)
data_interfaces = document.get("data_interfaces")
if not isinstance(data_interfaces, list):
    data_interfaces = []
for interface in data_interfaces:
    if str(interface.get("pci", "")).strip() != pci:
        continue
    if field == "linux_interface":
        print(str(interface.get("name", "")).strip())
    elif field == "mac":
        print(str(interface.get("mac", "")).strip().lower())
    elif field == "kernel_driver":
        print(str(interface.get("driver", "")).strip())
    break
PY
      ;;
  esac
}

ownership_value() {
  # ownership_value PCI FIELD -> value from the previous product ownership record.
  pci=$1
  field=$2
  [ -r "$ownership" ] || return 0
  python3 - "$ownership" "$pci" "$field" <<'PY'
import json
import sys

path, pci, field = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, ValueError):
    raise SystemExit(0)
for device in document.get("devices", []):
    if not isinstance(device, dict) or str(device.get("pci_address", "")).strip() != pci:
        continue
    if field == "kernel_driver":
        print(str(device.get("kernel_driver", "")).strip())
    elif field == "created_by_product":
        print("true" if device.get("created_by_product") else "false")
    break
PY
}

group_state() {
  # group_state PCI -> isolated | unisolated | absent
  pci=$1
  device=$sysfs/bus/pci/devices/$pci
  group_link=$device/iommu_group
  if [ ! -e "$group_link" ]; then
    printf '%s\n' absent
    return 0
  fi
  group=$(basename "$(readlink -f "$group_link" 2>/dev/null || true)")
  case "$group" in ''|*[!0-9]*) printf '%s\n' unisolated; return 0 ;; esac
  group_dir=$sysfs/kernel/iommu_groups/$group/devices
  if [ ! -d "$group_dir" ]; then
    printf '%s\n' unisolated
    return 0
  fi
  for member in "$group_dir"/*; do
    [ -e "$member" ] || continue
    member_pci=$(basename "$member")
    [ "$member_pci" = "$pci" ] && continue
    member_driver=$(driver_name "$member_pci")
    if ! is_selected "$member_pci" && [ -n "$member_driver" ]; then
      printf '%s\n' unisolated
      return 0
    fi
  done
  printf '%s\n' isolated
}

unbind_one() {
  pci=$1
  driver=$2
  [ -n "$driver" ] || return 0
  printf '%s\n' "$pci" > "$sysfs/bus/pci/drivers/$driver/unbind" || return 1
}

bind_one() {
  pci=$1
  target=$2
  current=$(driver_name "$pci")
  if [ "$current" = "$target" ]; then
    return 0
  fi
  if [ -n "$bind_helper" ]; then
    "$bind_helper" bind "$pci" "$target" || return 1
  else
    [ -d "$sysfs/bus/pci/drivers/$target" ] || return 1
    unbind_one "$pci" "$current" || return 1
    printf '%s\n' "$target" > "$sysfs/bus/pci/devices/$pci/driver_override" || return 1
    printf '%s\n' "$pci" > "$sysfs/bus/pci/drivers/$target/bind" || return 1
  fi
  [ "$(driver_name "$pci")" = "$target" ] || return 1
}

# Validate every selected device before mutating any of them. A partially
# applied ownership decision is exactly the kind of state that leaves a router
# without a management path. VFIO is used where the device has an isolated
# IOMMU group; UIO is used where it does not. Mixed transports are allowed
# because the fallback is per device, not per appliance.
any_vfio=false
any_uio=false
seen_devices=
for pci in $pci_rows; do
  [ "$pci" = "$management_pci" ] && fail "data mapping includes management PCI $pci"
  [ -e "$sysfs/bus/pci/devices/$pci" ] || fail "configured data PCI $pci is absent"
  case " $seen_devices " in
    *" $pci "*) fail "configured data PCI $pci is duplicated" ;;
  esac
  seen_devices="$seen_devices $pci"
  state=$(group_state "$pci")
  case "$state" in
    isolated) any_vfio=true ;;
    absent) any_uio=true ;;
    unisolated) any_uio=true ;;
    *) fail "$pci has an unreadable IOMMU group state" ;;
  esac
done

if [ "$any_vfio" = true ] && [ ! -d "$sysfs/bus/pci/drivers/vfio-pci" ]; then
  fail 'vfio-pci is unavailable for an IOMMU-isolated data device'
fi
if [ "$any_uio" = true ] && [ ! -d "$sysfs/bus/pci/drivers/uio_pci_generic" ]; then
  fail 'uio_pci_generic is unavailable for a data device without a usable IOMMU group'
fi

target_for_pci() {
  case "$(group_state "$1")" in
    isolated) printf '%s\n' vfio-pci ;;
    *) printf '%s\n' uio_pci_generic ;;
  esac
}

transport_for_driver() {
  case "$1" in
    vfio-pci) printf '%s\n' vfio ;;
    uio_pci_generic) printf '%s\n' uio ;;
    *) printf '%s\n' unknown ;;
  esac
}

ownership_args=
for pci in $pci_rows; do
  target_driver=$(target_for_pci "$pci")
  transport=$(transport_for_driver "$target_driver")
  prepared_iface=$(inventory_value "$pci" linux_interface || true)
  [ -n "$prepared_iface" ] || prepared_iface=$(interface_name "$pci" || true)
  prepared_mac=$(inventory_value "$pci" mac || true)
  [ -n "$prepared_mac" ] || prepared_mac=$(interface_mac "$pci" || true)
  prepared_driver=$(inventory_value "$pci" kernel_driver || true)
  current_driver=$(driver_name "$pci")
  if [ -z "$prepared_driver" ] || [ "$prepared_driver" = vfio-pci ] || [ "$prepared_driver" = uio_pci_generic ]; then
    prepared_driver=$(ownership_value "$pci" kernel_driver || true)
  fi
  if [ -z "$prepared_driver" ] || [ "$prepared_driver" = vfio-pci ] || [ "$prepared_driver" = uio_pci_generic ]; then
    if [ "$current_driver" != vfio-pci ] && [ "$current_driver" != uio_pci_generic ] && [ -n "$current_driver" ]; then
      prepared_driver=$current_driver
    fi
  fi
  [ -n "$prepared_driver" ] || fail "original kernel driver for $pci is unknown"
  if [ "$current_driver" != "$target_driver" ]; then
    [ -n "$prepared_iface" ] || fail "data interface identity for $pci is unknown"
  fi
  created_by_product=$(ownership_value "$pci" created_by_product || true)
  if [ "$created_by_product" != true ] && [ "$current_driver" != "$target_driver" ]; then
    created_by_product=true
  fi
  ownership_args="$ownership_args $pci|$prepared_iface|$prepared_mac|$prepared_driver|${created_by_product:-false}|$transport"
done

for pci in $pci_rows; do
  target_driver=$(target_for_pci "$pci")
  transport=$(transport_for_driver "$target_driver")
  iface=$(interface_name "$pci" || true)
  [ -z "$iface" ] || ip link set dev "$iface" down 2>/dev/null || true
  bind_one "$pci" "$target_driver" || fail "$pci cannot be bound to $target_driver"
  [ "$(driver_name "$pci")" = "$target_driver" ] || fail "$pci ownership is not $target_driver"
  log "VPP ownership prepared: $transport ${iface:-pci} $pci"
done

mkdir -p "$(dirname "$ownership")"
# shellcheck disable=SC2086
python3 - "$ownership" "$inventory_source" $ownership_args <<'PY'
import json
import sys
from datetime import datetime, timezone

path, source, *rows = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as existing:
        previous = json.load(existing)
except (OSError, ValueError):
    previous = {}
previous_devices = {
    str(item.get("pci_address", "")).strip(): item
    for item in previous.get("devices", [])
    if isinstance(item, dict)
}
devices = []
for row in rows:
    pci, interface, mac, driver, created, transport = (row.split("|") + [""] * 6)[:6]
    prior = previous_devices.get(pci, {})
    # If the device was already on the target driver at the start of this run,
    # only a previous product record proves that the product created it.
    created_by_product = str(created).lower() == "true" or bool(prior.get("created_by_product"))
    devices.append({
        "pci_address": pci,
        "linux_interface": interface,
        "mac_address": mac.lower(),
        "kernel_driver": driver,
        "transport": transport,
        "created_by_product": created_by_product,
    })
transports = sorted({item["transport"] for item in devices})
document = {
    "schema_version": 1,
    "source": source,
    "transport": transports[0] if len(transports) == 1 else "mixed",
    "updated_at": datetime.now(timezone.utc).isoformat(),
    "devices": devices,
}
with open(path + ".tmp", "w", encoding="utf-8") as target:
    json.dump(document, target, indent=2)
    target.write("\n")
import os
os.chmod(path + ".tmp", 0o600)
os.replace(path + ".tmp", path)
PY

[ -f "$startup" ] || fail 'VPP startup configuration is missing'
sed -i '/^# BEGIN LY ROUTE DPDK$/,/^# END LY ROUTE DPDK$/d' "$startup"
if grep -q 'plugin dpdk_plugin.so { disable }' "$startup"; then
  sed -i 's/plugin dpdk_plugin\.so { disable }/plugin dpdk_plugin.so { enable }/' "$startup"
elif ! grep -q 'plugin dpdk_plugin.so { enable }' "$startup"; then
  fail 'VPP startup configuration does not declare the DPDK plugin'
fi
{
  printf '\n# BEGIN LY ROUTE DPDK\n'
  printf 'dpdk {\n'
  if [ "$any_vfio" = true ] && [ "$any_uio" = true ]; then
    # VPP has one global uio-driver setting. Auto-detection lets a device-wide
    # path mix IOMMU-protected VFIO ports with UIO ports on the same appliance.
    printf '  uio-driver auto\n'
  elif [ "$any_vfio" = true ]; then
    printf '  uio-driver vfio-pci\n'
  else
    printf '  uio-driver uio_pci_generic\n'
  fi
  for pci in $pci_rows; do
    case "$pci" in
      [0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f]:[0-9A-Fa-f][0-9A-Fa-f]:[0-9A-Fa-f][0-9A-Fa-f].[0-7]) ;;
      *) fail "invalid selected DPDK PCI address: $pci" ;;
    esac
    printf '  dev %s\n' "$pci"
  done
  printf '}\n# END LY ROUTE DPDK\n'
} >> "$startup"
