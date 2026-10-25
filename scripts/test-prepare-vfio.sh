#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
script="$repo_root/packaging/rootfs-overlay/usr/lib/ly-route/prepare-vfio.sh"
tmp=$(mktemp -d)
if [ "${LY_ROUTE_KEEP_TEST_TMP:-0}" = 1 ]; then
  printf 'kept fixture: %s\n' "$tmp" >&2
else
  trap 'rm -rf "$tmp"' EXIT HUP INT TERM
fi

mkdir -p "$tmp/bin"
cat > "$tmp/bin/modprobe" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
cat > "$tmp/bin/ip" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
cat > "$tmp/bin/bind-helper" <<'EOF'
#!/usr/bin/env sh
set -eu
[ "$1" = bind ] || exit 1
case "${FAKE_BIND_RESULT:-ok}" in
  fail) exit 1 ;;
  leave) exit 0 ;;
esac
root=$LY_ROUTE_SYSFS_ROOT
device="$root/bus/pci/devices/$2"
driver="$root/bus/pci/drivers/$3"
[ -d "$device" ] && [ -d "$driver" ] || exit 1
rm -f "$device/driver"
ln -s "$driver" "$device/driver"
EOF
chmod 0755 "$tmp/bin/modprobe" "$tmp/bin/ip" "$tmp/bin/bind-helper"

setup_fixture() {
  case_name=$1
  with_iommu=$2
  data_pci=${3:-0000:03:00.0}
  management_pci=${4:-0000:01:00.0}
  fixture="$tmp/$case_name"
  sys="$fixture/sys"
  mkdir -p "$sys/bus/pci/devices/$data_pci" "$sys/bus/pci/drivers/igb" \
    "$sys/bus/pci/drivers/vfio-pci" "$sys/bus/pci/drivers/uio_pci_generic" \
    "$sys/module/vfio_pci" "$sys/module/uio_pci_generic" \
    "$sys/class/net/enp3s0" "$sys/bus/pci/devices/$data_pci/net/enp3s0" \
    "$sys/bus/pci/devices/$management_pci" "$sys/bus/pci/devices/$management_pci/net/enp1s0"
  ln -s "$sys/bus/pci/drivers/igb" "$sys/bus/pci/devices/$data_pci/driver"
  ln -s "$sys/bus/pci/devices/$data_pci" "$sys/class/net/enp3s0/device"
  printf '%s\n' '00:11:22:33:44:55' > "$sys/class/net/enp3s0/address"
  if [ "$with_iommu" = yes ]; then
    mkdir -p "$sys/kernel/iommu_groups/12/devices"
    ln -s "$sys/bus/pci/devices/$data_pci" "$sys/kernel/iommu_groups/12/devices/$data_pci"
    ln -s "$sys/kernel/iommu_groups/12" "$sys/bus/pci/devices/$data_pci/iommu_group"
  fi
  printf 'plugin dpdk_plugin.so { disable }\n' > "$fixture/startup.conf"
  cat > "$fixture/inventory.json" <<EOF
{"schema_version":1,"source":"test","management":{"linux_interface":"enp1s0","pci":"$management_pci"},"data_interfaces":[{"linux_interface":"enp3s0","pci":"$data_pci","mac":"00:11:22:33:44:55","kernel_driver":"igb","role":"data"}]}
EOF
  chmod 0600 "$fixture/inventory.json"
  printf '%s\n' "$fixture"
}

run_prepare() {
  fixture=$1
  PATH="$tmp/bin:$PATH" \
    LY_ROUTE_SYSFS_ROOT="$fixture/sys" \
    LY_ROUTE_VFIO_DEVICES="$fixture/vfio-devices" \
    LY_ROUTE_INSTALLER_NETWORK="$fixture/installed-network.json" \
    LY_ROUTE_DATAPLANE_INVENTORY="$fixture/inventory.json" \
    LY_ROUTE_VPP_STARTUP_CONFIG="$fixture/startup.conf" \
    LY_ROUTE_DPDK_OWNERSHIP_STATE="$fixture/ownership.json" \
    LY_ROUTE_PCI_BIND_HELPER="$tmp/bin/bind-helper" \
    sh "$script"
}

# IOMMU present -> VFIO preferred.
fixture=$(setup_fixture iommu yes)
run_prepare "$fixture" > "$fixture/log" 2>&1
grep -q 'VPP ownership prepared: vfio enp3s0 0000:03:00.0' "$fixture/log"
[ "$(basename "$(readlink -f "$fixture/sys/bus/pci/devices/0000:03:00.0/driver")")" = vfio-pci ]
grep -q 'uio-driver vfio-pci' "$fixture/startup.conf"
grep -q 'dev 0000:03:00.0' "$fixture/startup.conf"
python3 - "$fixture/ownership.json" <<'PY'
import json, sys
document = json.load(open(sys.argv[1], encoding="utf-8"))
device = document["devices"][0]
assert device["pci_address"] == "0000:03:00.0"
assert device["kernel_driver"] == "igb"
assert device["transport"] == "vfio"
assert device["created_by_product"] is True
PY

# No IOMMU -> UIO fallback, and the management PCI is untouched.
fixture=$(setup_fixture uio no)
run_prepare "$fixture" > "$fixture/log" 2>&1
grep -q 'VPP ownership prepared: uio enp3s0 0000:03:00.0' "$fixture/log"
[ "$(basename "$(readlink -f "$fixture/sys/bus/pci/devices/0000:03:00.0/driver")")" = uio_pci_generic ]
grep -q 'uio-driver uio_pci_generic' "$fixture/startup.conf"
[ ! -e "$fixture/sys/bus/pci/devices/0000:01:00.0/driver" ]
python3 - "$fixture/ownership.json" <<'PY'
import json, sys
device = json.load(open(sys.argv[1], encoding="utf-8"))["devices"][0]
assert device["transport"] == "uio"
assert device["created_by_product"] is True
PY

# Mixed devices: one isolated, one not -> per-device transports and auto driver.
fixture=$(setup_fixture mixed yes)
mkdir -p "$fixture/sys/bus/pci/devices/0000:04:00.0/net/enp4s0" "$fixture/sys/class/net/enp4s0"
ln -s "$fixture/sys/bus/pci/drivers/igb" "$fixture/sys/bus/pci/devices/0000:04:00.0/driver"
ln -s "$fixture/sys/bus/pci/devices/0000:04:00.0" "$fixture/sys/class/net/enp4s0/device"
printf '%s\n' '00:11:22:33:44:66' > "$fixture/sys/class/net/enp4s0/address"
python3 - "$fixture/inventory.json" <<'PY'
import json, sys
path = sys.argv[1]
document = json.load(open(path, encoding="utf-8"))
document["data_interfaces"].append({"linux_interface": "enp4s0", "pci": "0000:04:00.0", "mac": "00:11:22:33:44:66", "kernel_driver": "igb", "role": "data"})
json.dump(document, open(path, "w", encoding="utf-8"))
PY
run_prepare "$fixture" > "$fixture/log" 2>&1
grep -q 'VPP ownership prepared: vfio enp3s0 0000:03:00.0' "$fixture/log"
grep -q 'VPP ownership prepared: uio enp4s0 0000:04:00.0' "$fixture/log"
grep -q 'uio-driver auto' "$fixture/startup.conf"

# Bind failure -> fail closed with an explicit reason.
fixture=$(setup_fixture bind-fail no)
if FAKE_BIND_RESULT=fail run_prepare "$fixture" > "$fixture/log" 2>&1; then
  echo 'bind failure was accepted' >&2
  exit 1
fi
grep -q 'VPP ownership locked: 0000:03:00.0 cannot be bound to uio_pci_generic' "$fixture/log"
[ "$(basename "$(readlink -f "$fixture/sys/bus/pci/devices/0000:03:00.0/driver")")" = igb ]

# Binding did not take effect (helper lies) -> verification failure.
fixture=$(setup_fixture verify-fail no)
if FAKE_BIND_RESULT=leave run_prepare "$fixture" > "$fixture/log" 2>&1; then
  echo 'unverified binding was accepted' >&2
  exit 1
fi
grep -q 'VPP ownership locked: 0000:03:00.0 cannot be bound to uio_pci_generic' "$fixture/log"

# Data mapping includes the management PCI -> fail closed.
fixture=$(setup_fixture management no 0000:01:00.0 0000:01:00.0)
if run_prepare "$fixture" > "$fixture/log" 2>&1; then
  echo 'management PCI was accepted as a data device' >&2
  exit 1
fi
grep -q 'data mapping includes management PCI 0000:01:00.0' "$fixture/log"

# No installer mapping and no API inventory -> explicit skip, not a failure.
fixture=$(setup_fixture none no)
rm -f "$fixture/inventory.json"
run_prepare "$fixture" > "$fixture/log" 2>&1
grep -q 'VPP ownership preflight skipped: no operator interface inventory or installer NIC mapping' "$fixture/log"

printf 'ownership preflight scenarios passed\n'
