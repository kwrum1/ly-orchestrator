#!/bin/bash
# Ly Route three-VM functional lab (QEMU VMnet equivalent).
# Functional acceptance only; VM throughput is not a production benchmark.
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=/dev/null
. "$here/topology.env"

say() { printf '[lab] %s\n' "$*"; }
die() { printf '[lab] ERROR: %s\n' "$*" >&2; exit 1; }

require_root() { [ "$(id -u)" -eq 0 ] || die "must run as root"; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

qemu_pid() { cat "$LAB_ROOT/$1.pid" 2>/dev/null || true; }

tap_ensure() {
  local tap=$1 bridge=$2
  ip link show "$tap" >/dev/null 2>&1 || ip tuntap add dev "$tap" mode tap
  ip link set "$tap" master "$bridge"
  ip link set "$tap" up
}

bridge_ensure() {
  local bridge=$1
  ip link show "$bridge" >/dev/null 2>&1 || ip link add "$bridge" type bridge
  ip link set "$bridge" up
}

make_seed() {
  local name=$1 user=$2 network=$3
  local dir="$LAB_ROOT/seed-$name"
  local iso="$LAB_ROOT/$name-seed.iso"
  rm -rf "$dir"
  mkdir -p "$dir"
  sed "s|__SSH_PUBKEY__|$(cat "$LAB_SSH_KEY.pub")|g" "$user" > "$dir/user-data"
  cp "$network" "$dir/network-config"
  cat > "$dir/meta-data" <<EOF
instance-id: ly-route-$name-$(date +%s)
local-hostname: ly-lab-$name
EOF
  xorriso -as mkisofs -quiet -output "$iso" -volid cidata -joliet -rock "$dir" >/dev/null
  say "seed ready: $iso"
}

prepare() {
  require_root
  require_cmd qemu-system-x86_64
  require_cmd qemu-img
  require_cmd xorriso
  mkdir -p "$LAB_ROOT"
  [ -f "$CLOUD_IMAGE" ] || die "missing cloud image: $CLOUD_IMAGE"
  [ -f "$LAB_SSH_KEY" ] || ssh-keygen -q -t ed25519 -N '' -f "$LAB_SSH_KEY"
  [ -f "$ROUTER_QCOW2" ] || "$here/prepare-router.sh"

  for vm in server client; do
    [ -f "$LAB_ROOT/$vm.qcow2" ] || qemu-img create -q -f qcow2 -F qcow2 -b "$CLOUD_IMAGE" "$LAB_ROOT/$vm.qcow2"
  done
  make_seed pppoe-server "$here/cloud-init/pppoe-server.yaml" "$here/cloud-init/pppoe-server-network.yaml"
  make_seed client "$here/cloud-init/client.yaml" "$here/cloud-init/client-network.yaml"
}

network_up() {
  require_root
  for bridge in br-wan br-lan; do bridge_ensure "$bridge"; done
  tap_ensure tap-pppoe-wan br-wan
  tap_ensure tap-router-wan br-wan
  tap_ensure tap-router-lan br-lan
  tap_ensure tap-client-lan br-lan
}

start_router() {
  local pid
  pid=$(qemu_pid router)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then say "router already running pid=$pid"; return; fi
  rm -f "$LAB_ROOT/router-serial.log"
  qemu-system-x86_64 \
    -name ly-lab-router \
    -m 3072 -smp 2 -machine q35 -accel tcg,thread=multi -cpu Nehalem \
    -kernel "$ROUTER_KERNEL" -initrd "$ROUTER_INITRD" \
    -append "root=UUID=$(cat "$ROUTER_UUID_FILE") rw net.ifnames=0 console=ttyS0,115200n8" \
    -drive file="$ROUTER_QCOW2",format=qcow2,if=ide \
    -netdev user,id=mgmt,hostfwd=tcp:127.0.0.1:"$ROUTER_MGMT_PORT"-:443,hostfwd=tcp:127.0.0.1:"$ROUTER_SSH_PORT"-:22 \
    -device e1000,netdev=mgmt,mac="$MGMT_MAC" \
    -netdev tap,id=wan,ifname=tap-router-wan,script=no,downscript=no \
    -device e1000,netdev=wan,mac="$WAN_MAC" \
    -netdev tap,id=lan,ifname=tap-router-lan,script=no,downscript=no \
    -device e1000,netdev=lan,mac="$LAN_MAC" \
    -serial file:"$LAB_ROOT/router-serial.log" \
    -display none -daemonize -pidfile "$LAB_ROOT/router.pid"
  say "router started pid=$(qemu_pid router)"
}

start_cloud_vm() {
  local name=$1
  local pid
  pid=$(qemu_pid "$name")
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then say "$name already running pid=$pid"; return; fi
  rm -f "$LAB_ROOT/$name-serial.log"
  if [ "$name" = pppoe-server ]; then
    qemu-system-x86_64 \
      -name ly-lab-pppoe \
      -m 1024 -smp 1 -machine q35 -accel tcg,thread=multi -cpu Nehalem \
      -drive file="$LAB_ROOT/server.qcow2",format=qcow2,if=virtio \
      -drive file="$LAB_ROOT/pppoe-server-seed.iso",format=raw,if=virtio,readonly=on \
      -netdev user,id=mgmt,hostfwd=tcp:127.0.0.1:"$SERVER_SSH_PORT"-:22,hostfwd=tcp:127.0.0.1:"$SERVER_HTTP_PORT"-:8080 \
      -device e1000,netdev=mgmt,mac="$SERVER_MGMT_MAC" \
      -netdev tap,id=wan,ifname=tap-pppoe-wan,script=no,downscript=no \
      -device e1000,netdev=wan,mac="$SERVER_WAN_MAC" \
      -serial file:"$LAB_ROOT/pppoe-server-serial.log" \
      -display none -daemonize -pidfile "$LAB_ROOT/pppoe-server.pid"
  else
    qemu-system-x86_64 \
      -name ly-lab-client \
      -m 1024 -smp 1 -machine q35 -accel tcg,thread=multi -cpu Nehalem \
      -drive file="$LAB_ROOT/client.qcow2",format=qcow2,if=virtio \
      -drive file="$LAB_ROOT/client-seed.iso",format=raw,if=virtio,readonly=on \
      -netdev user,id=mgmt,hostfwd=tcp:127.0.0.1:"$CLIENT_SSH_PORT"-:22 \
      -device e1000,netdev=mgmt,mac="$CLIENT_MGMT_MAC" \
      -netdev tap,id=lan,ifname=tap-client-lan,script=no,downscript=no \
      -device e1000,netdev=lan,mac="$CLIENT_LAN_MAC" \
      -serial file:"$LAB_ROOT/client-serial.log" \
      -display none -daemonize -pidfile "$LAB_ROOT/client.pid"
  fi
  say "$name started pid=$(qemu_pid "$name")"
}

cmd_up() {
  prepare
  network_up
  start_router
  start_cloud_vm pppoe-server
  start_cloud_vm client
  say "management: router https://127.0.0.1:$ROUTER_MGMT_PORT"
  say "ssh: router=$ROUTER_SSH_PORT server=$SERVER_SSH_PORT client=$CLIENT_SSH_PORT"
}

cmd_status() {
  for name in router pppoe-server client; do
    pid=$(qemu_pid "$name")
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      printf '%-14s running pid=%s\n' "$name" "$pid"
    else
      printf '%-14s stopped\n' "$name"
    fi
  done
  ip -br link show br-wan br-lan 2>/dev/null || true
}

cmd_down() {
  require_root
  for name in router pppoe-server client; do
    pid=$(qemu_pid "$name")
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
    rm -f "$LAB_ROOT/$name.pid"
  done
  sleep 1
  for tap in tap-pppoe-wan tap-router-wan tap-router-lan tap-client-lan; do
    ip link del "$tap" 2>/dev/null || true
  done
  for bridge in br-wan br-lan; do
    ip link del "$bridge" 2>/dev/null || true
  done
  say "lab stopped"
}

cmd_ssh() {
  local target=${1:?usage: lab.sh ssh router|server|client [command]}
  shift
  local port
  case "$target" in
    router) port=$ROUTER_SSH_PORT; target_user=root ;;
    server) port=$SERVER_SSH_PORT; target_user=lab ;;
    client) port=$CLIENT_SSH_PORT; target_user=lab ;;
    *) die "unknown ssh target: $target" ;;
  esac
  ssh -i "$LAB_SSH_KEY" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=10 "$target_user"@127.0.0.1 "$@"
}

cmd=${1:-up}
case "$cmd" in
  prepare) prepare ;;
  up) cmd_up ;;
  status) cmd_status ;;
  down) cmd_down ;;
  ssh) shift; cmd_ssh "$@" ;;
  *) die "usage: lab.sh prepare|up|status|down|ssh <router|server|client> [command]" ;;
esac
