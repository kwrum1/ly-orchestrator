#!/bin/sh
# End-to-end DHCPv4 smoke for the dnsmasq configuration Ly Route renders.
#
# Runs dnsmasq and a busybox udhcpc client in a private netns. Skips (77) when
# dnsmasq/busybox/ip are unavailable so the container acceptance matrix can run
# on hosts without the DHCP daemon; the image itself always ships dnsmasq.
set -eu

command -v dnsmasq >/dev/null 2>&1 || { echo "dnsmasq not installed; skipping DHCP netns e2e" >&2; exit 77; }
command -v busybox >/dev/null 2>&1 || { echo "busybox not installed; skipping DHCP netns e2e" >&2; exit 77; }
command -v ip >/dev/null 2>&1 || { echo "iproute2 not installed; skipping DHCP netns e2e" >&2; exit 77; }

ns=ly-route-dnsmasq-$$
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ly-route-dnsmasq.XXXXXX")
server=ds-s-$$
client=ds-c-$$
dnsmasq_pid=

cleanup() {
	[ -z "$dnsmasq_pid" ] || kill "$dnsmasq_pid" 2>/dev/null || true
	[ -z "$dnsmasq_pid" ] || wait "$dnsmasq_pid" 2>/dev/null || true
	ip netns del "$ns" 2>/dev/null || true
	rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

ip netns add "$ns"
ip link add "$server" type veth peer name "$client"
ip link set "$server" netns "$ns"
ip link set "$client" netns "$ns"
ip netns exec "$ns" ip link set "$server" up
ip netns exec "$ns" ip link set "$client" up
ip netns exec "$ns" ip addr add 192.168.88.1/24 dev "$server"

cat >"$tmpdir/dnsmasq.conf" <<EOF
port=0
dhcp-authoritative
log-dhcp
interface=$server
dhcp-range=set:lyroute-test,192.168.88.100,192.168.88.150,255.255.255.0,1h
dhcp-option=tag:lyroute-test,3,192.168.88.1
dhcp-option=tag:lyroute-test,6,192.168.88.1
dhcp-leasefile=$tmpdir/dnsmasq.leases
pid-file=$tmpdir/dnsmasq.pid
log-facility=$tmpdir/dnsmasq.log
EOF

cat >"$tmpdir/udhcpc.sh" <<'EOF'
#!/bin/sh
case "$1" in
  bound|renew)
    {
      echo "ip=$ip"
      echo "router=$router"
      echo "dns=$dns"
    } >"$LEASE_FILE"
    ;;
esac
EOF
chmod +x "$tmpdir/udhcpc.sh"

ip netns exec "$ns" dnsmasq --conf-file="$tmpdir/dnsmasq.conf" --no-daemon --keep-in-foreground \
  >"$tmpdir/dnsmasq.stdout" 2>&1 &
dnsmasq_pid=$!
sleep 1
kill -0 "$dnsmasq_pid" 2>/dev/null || { echo "dnsmasq exited during startup" >&2; cat "$tmpdir/dnsmasq.stdout" >&2; exit 1; }

lease="$tmpdir/lease.env"
: >"$lease"
timeout 20 ip netns exec "$ns" env LEASE_FILE="$lease" \
  busybox udhcpc -f -q -n -t 8 -T 2 -i "$client" -s "$tmpdir/udhcpc.sh" \
  >"$tmpdir/udhcpc.log" 2>&1 || true

[ -s "$lease" ] || { echo "client did not obtain a lease" >&2; cat "$tmpdir/udhcpc.log" >&2; cat "$tmpdir/dnsmasq.stdout" >&2; exit 1; }
# shellcheck disable=SC1090
. "$lease"

case "${ip:-}" in
  192.168.88.*) ;;
  *) echo "lease address '$ip' is outside the configured pool" >&2; exit 1 ;;
esac
last=${ip##*.}
[ "$last" -ge 100 ] && [ "$last" -le 150 ] || { echo "lease address '$ip' is outside 192.168.88.100-150" >&2; exit 1; }
[ "${router:-}" = "192.168.88.1" ] || { echo "router '${router:-}' != 192.168.88.1" >&2; exit 1; }
printf '%s\n' "${dns:-}" | tr ' ' '\n' | grep -qx "192.168.88.1" || { echo "dns '${dns:-}' does not include 192.168.88.1" >&2; exit 1; }

grep -Eq '^[0-9]+ [0-9a-f:]+ '"$ip"' ' "$tmpdir/dnsmasq.leases" || {
  echo "dnsmasq lease file does not contain $ip" >&2
  cat "$tmpdir/dnsmasq.leases" >&2
  exit 1
}

echo "dnsmasq DHCPv4 e2e passed: lease=$ip router=$router dns=$dns"
