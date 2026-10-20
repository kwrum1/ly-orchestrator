#!/bin/bash
# Prepare the Ly Route router disk for the VM functional lab.
# The router keeps physical-first takeover rules; this script only injects the
# explicit VMnet AF_PACKET fallback and the lab NIC mapping.
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=/dev/null
. "$here/topology.env"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 2; }
[ -f "$ROUTER_RAW" ] || { echo "missing router raw image: $ROUTER_RAW" >&2; exit 2; }
[ -f "$ROUTER_QCOW2" ] || true

mkdir -p "$LAB_ROOT"
if [ ! -s "$LAB_SSH_KEY" ]; then
  ssh-keygen -q -t ed25519 -N '' -f "$LAB_SSH_KEY"
fi
pubkey=$(cat "$LAB_SSH_KEY.pub")

qemu-nbd -f raw -c /dev/nbd0 "$ROUTER_RAW"
cleanup() {
  umount /mnt/ly-router-lab 2>/dev/null || true
  qemu-nbd -d /dev/nbd0 >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

sleep 2
partprobe /dev/nbd0 2>/dev/null || true
mkdir -p /mnt/ly-router-lab
mount /dev/nbd0p3 /mnt/ly-router-lab

kernel=$(find /mnt/ly-router-lab/boot -maxdepth 1 -type f -name 'vmlinuz-*' | sort -V | tail -1)
initrd=$(find /mnt/ly-router-lab/boot -maxdepth 1 -type f -name 'initrd.img-*' | sort -V | tail -1)
[ -n "$kernel" ] && [ -n "$initrd" ] || { echo "router kernel/initrd not found" >&2; exit 1; }
cp "$kernel" "$ROUTER_KERNEL"
cp "$initrd" "$ROUTER_INITRD"
uuid=$(awk '$2 == "/" && $3 == "ext4" {sub("UUID=", "", $1); print $1; exit}' /mnt/ly-router-lab/etc/fstab)
[ -n "$uuid" ] || { echo "router root UUID not found" >&2; exit 1; }
printf '%s\n' "$uuid" > "$ROUTER_UUID_FILE"

python3 - "$MGMT_MAC" "$WAN_MAC" "$LAN_MAC" /mnt/ly-router-lab/etc/ly-route/installed-network.json <<'PY'
import json
import os
import sys

mgmt, wan, lan, path = sys.argv[1:]
document = {
    "schema_version": 1,
    "source": "ly-route-vm-lab",
    "management": {
        "name": "eth0",
        "mac": mgmt,
        "pci": "",
        "cidr": "10.0.2.15/24",
        "gateway": "10.0.2.2",
        "vpp_owned": False,
    },
    "data_interfaces": [
        {"name": "eth1", "mac": wan, "pci": "", "link": "up", "driver": "e1000",
         "state": "ready", "reason": "vm lab WAN", "selected": None, "candidates": []},
        {"name": "eth2", "mac": lan, "pci": "", "link": "up", "driver": "e1000",
         "state": "ready", "reason": "vm lab LAN", "selected": None, "candidates": []},
    ],
    "dataplane": {
        "state": "preflight_ready",
        "management_excluded": True,
        "selection_order": ["vpp_native", "vpp_dpdk"],
        "runtime_verification_required": True,
    },
}
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path, "w", encoding="utf-8") as output:
    json.dump(document, output, ensure_ascii=False, indent=2)
    output.write("\n")
os.chmod(path, 0o600)
PY

runtime_env=/mnt/ly-router-lab/etc/ly-route/runtime.env
touch "$runtime_env"
grep -v -E '^(LY_ROUTE_MANAGEMENT_INTERFACE|LY_ROUTE_VPP_DATA_INTERFACES|LY_ROUTE_VMNET_AF_PACKET_ACCEPTANCE)=' "$runtime_env" > "$runtime_env.tmp" || true
cat >> "$runtime_env.tmp" <<EOF
LY_ROUTE_MANAGEMENT_INTERFACE=eth0
LY_ROUTE_VPP_DATA_INTERFACES=eth1,eth2
LY_ROUTE_VMNET_AF_PACKET_ACCEPTANCE=true
EOF
chmod 0644 "$runtime_env.tmp"
mv -f "$runtime_env.tmp" "$runtime_env"

install -d -m 0700 /mnt/ly-router-lab/root/.ssh
printf '%s\n' "$pubkey" > /mnt/ly-router-lab/root/.ssh/authorized_keys
chmod 0600 /mnt/ly-router-lab/root/.ssh/authorized_keys

rm -f /mnt/ly-router-lab/var/lib/ly-route/firstboot-complete \
      /mnt/ly-router-lab/var/lib/ly-route/firstboot-ran-at \
      /mnt/ly-router-lab/etc/systemd/network/05-ly-route-management.network

sync
umount /mnt/ly-router-lab
trap - EXIT INT TERM
qemu-nbd -d /dev/nbd0 >/dev/null

qemu-img convert -f raw -O qcow2 "$ROUTER_RAW" "$ROUTER_QCOW2"
ls -lh "$ROUTER_QCOW2" "$ROUTER_KERNEL" "$ROUTER_INITRD"
echo "ROUTER_UUID=$(cat "$ROUTER_UUID_FILE")"
