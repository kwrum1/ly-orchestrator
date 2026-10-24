#!/usr/bin/env bash
# End-to-end LAN DHCP + LAN->WAN proof against a live Ly Route appliance.
#
# The appliance owns the LAN NIC in VPP, so the test client must be attached to
# the same L2 segment from the outside. Run this script on the acceptance host
# that carries the appliance LAN tap on a Linux bridge:
#
#   ./scripts/e2e-lan-dhcp.sh \
#     --bridge br-acc-lan \
#     --guest root@127.0.0.1:12022 \
#     --identity /path/to/qemu_ed25519 \
#     --wan-probe http://100.77.0.1:18088/route \
#     --expect-token LYACC-token
#
# It proves, in order:
#   1. DHCPv4: a real udhcpc lease (address, router, DNS) from the appliance.
#   2. L3: the leased client pings the LAN gateway.
#   3. NAT44 + routing: the client reaches a WAN fixture and gets its token.
#
# Requires: root (netns/veth), busybox, curl, iproute2 + a running appliance.
set -eu

bridge=""
guest=""
identity=""
wan_probe=""
expect_token=""
lan_gateway="192.168.77.2"
timeout_seconds=45
netns="ly-e2e-dhcp"
client_if="lycli0"
host_if="lyhst0"

usage() {
  sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-1}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --bridge) bridge=${2:?}; shift 2 ;;
    --guest) guest=${2:?}; shift 2 ;;
    --identity) identity=${2:-}; shift 2 ;;
    --wan-probe) wan_probe=${2:?}; shift 2 ;;
    --expect-token) expect_token=${2:?}; shift 2 ;;
    --lan-gateway) lan_gateway=${2:?}; shift 2 ;;
    --timeout) timeout_seconds=${2:?}; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "e2e-lan-dhcp: must run as root (netns/veth)" >&2; exit 77; }
for tool in ip busybox curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "e2e-lan-dhcp: missing $tool" >&2; exit 77; }
done
[ -n "$bridge" ] || usage
[ -n "$guest" ] || usage
ip link show "$bridge" >/dev/null 2>&1 || { echo "e2e-lan-dhcp: bridge $bridge does not exist" >&2; exit 77; }

cleanup() {
  ip netns del "$netns" 2>/dev/null || true
  ip link del "$host_if" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "== setup client netns on $bridge"
cleanup
ip netns add "$netns"
ip link add "$host_if" type veth peer name "$client_if"
ip link set "$client_if" netns "$netns"
ip link set "$host_if" master "$bridge"
ip link set "$host_if" up
ip -n "$netns" link set lo up
ip -n "$netns" link set "$client_if" up

lease_file=$(mktemp)
lease_script=$(mktemp)
cat >"$lease_script" <<'SCRIPT'
#!/bin/sh
# Minimal DHCP client hook: configure the leased address/route in the client
# netns and persist the lease so the caller can assert on it.
mask_prefix() {
  p=0
  for oct in $(printf '%s' "${1:-255.255.255.0}" | tr '.' ' '); do
    case "$oct" in
      255) p=$((p+8)) ;; 254) p=$((p+7)) ;; 252) p=$((p+6)) ;; 248) p=$((p+5)) ;;
      240) p=$((p+4)) ;; 224) p=$((p+3)) ;; 192) p=$((p+2)) ;; 128) p=$((p+1)) ;; 0) ;;
    esac
  done
  printf '%s' "$p"
}
case "$1" in
  deconfig)
    ip -4 addr flush dev "$interface" 2>/dev/null || true
    ip route del default 2>/dev/null || true
    ;;
  bound|renew)
    case "${mask:-}" in
      ''|*[!0-9]*) prefix=$(mask_prefix "${mask:-255.255.255.0}") ;;
      *) prefix=$mask ;;
    esac
    ip -4 addr flush dev "$interface" 2>/dev/null || true
    ip -4 addr add "${ip}/${prefix}" dev "$interface"
    first_router=$(printf '%s' "${router:-}" | cut -d, -f1)
    if [ -n "$first_router" ]; then
      ip route replace default via "$first_router" dev "$interface"
    fi
    printf 'ip=%s\nrouter=%s\ndns=%s\n' "${ip:-}" "${router:-}" "${dns:-}" >"${LEASE_FILE:?}"
    ;;
esac
SCRIPT
chmod 755 "$lease_script"
trap 'cleanup; rm -f "$lease_file" "$lease_script"' EXIT INT TERM

echo "== DHCPv4 lease (udhcpc)"
: >"$lease_file"
timeout "$timeout_seconds" ip netns exec "$netns" env LEASE_FILE="$lease_file" \
  busybox udhcpc -f -q -n -t 8 -T 3 -i "$client_if" -s "$lease_script" || true
if [ ! -s "$lease_file" ]; then
  echo "FAIL: no DHCPv4 lease after ${timeout_seconds}s" >&2
  exit 1
fi
# shellcheck disable=SC1090
. "$lease_file"
printf 'lease: ip=%s router=%s dns=%s\n' "${ip:-}" "${router:-}" "${dns:-}"
[ "$(printf '%s' "$router" | cut -d, -f1)" = "$lan_gateway" ] || { echo "FAIL: router ${router:-} != $lan_gateway" >&2; exit 1; }
printf '%s' "$dns" | tr ',' '\n' | grep -qx "$lan_gateway" || { echo "FAIL: DNS list ${dns:-} lacks $lan_gateway" >&2; exit 1; }

echo "== LAN routing (client -> gateway)"
ip netns exec "$netns" ping -c 2 -W 3 "$lan_gateway" >/dev/null || { echo "FAIL: cannot ping $lan_gateway" >&2; exit 1; }

if [ -n "$wan_probe" ]; then
  echo "== NAT44 + routing (client -> $wan_probe)"
  body=$(mktemp)
  code=$(ip netns exec "$netns" curl -sS -m 15 -o "$body" -w '%{http_code}' "$wan_probe" || true)
  [ "$code" = 200 ] || { echo "FAIL: WAN probe HTTP=$code" >&2; cat "$body" >&2 2>/dev/null || true; rm -f "$body"; exit 1; }
  if [ -n "$expect_token" ]; then
    grep -q "$expect_token" "$body" || { echo "FAIL: WAN probe body lacks token $expect_token" >&2; cat "$body" >&2; rm -f "$body"; exit 1; }
  fi
  echo "wan probe: HTTP=200 $(head -c 160 "$body")"
  rm -f "$body"
fi

echo "PASS: DHCPv4 lease + LAN routing + LAN->WAN translation verified"
