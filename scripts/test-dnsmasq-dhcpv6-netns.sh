#!/bin/sh
# End-to-end IPv6 RA + DHCPv6 smoke for the dnsmasq configuration Ly Route
# renders. Runs dnsmasq and a Linux/busybox client in a private netns.
#
# Skips (77) when dnsmasq, busybox, ip or go are unavailable. Set DNSMASQ to a
# specific binary (for packaged test hosts) or leave it unset for PATH lookup.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dnsmasq_bin=${DNSMASQ:-$(command -v dnsmasq || true)}
[ -n "$dnsmasq_bin" ] || { echo "dnsmasq not installed; skipping IPv6 netns e2e" >&2; exit 77; }
command -v busybox >/dev/null 2>&1 || { echo "busybox not installed; skipping IPv6 netns e2e" >&2; exit 77; }
command -v ip >/dev/null 2>&1 || { echo "iproute2 not installed; skipping IPv6 netns e2e" >&2; exit 77; }
command -v go >/dev/null 2>&1 || { echo "go not installed; skipping IPv6 netns e2e" >&2; exit 77; }
command -v python3 >/dev/null 2>&1 || { echo "python3 not installed; skipping IPv6 netns e2e" >&2; exit 77; }

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ly-route-dnsmasq-v6.XXXXXX")
helper_dir=$(mktemp -d "$repo_root/backend/.dnsmasq-v6-e2e.XXXXXX")
namespaces=""
dnsmasq_pid=

cleanup() {
	status=$?
	[ -z "$dnsmasq_pid" ] || kill "$dnsmasq_pid" 2>/dev/null || true
	[ -z "$dnsmasq_pid" ] || wait "$dnsmasq_pid" 2>/dev/null || true
	for namespace in $namespaces; do
		ip netns pids "$namespace" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
		ip netns del "$namespace" 2>/dev/null || true
	done
	rm -rf "$tmpdir" "$helper_dir"
	exit "$status"
}
trap cleanup EXIT HUP INT TERM

cat >"$helper_dir/main.go" <<'EOF'
package main

import (
	"os"

	"ly-route/backend/internal/runtime/service"
)

func main() {
	lifetime := 1800
	artifacts, err := service.RenderDnsmasqConfig([]service.DnsmasqPlan{{
		ID: "e2e", InterfaceID: os.Getenv("E2E_IFACE"),
		Subnet: "192.168.88.0/24", Pools: []string{"192.168.88.100-192.168.88.150"},
		Routers: []string{"192.168.88.1"},
		IPv6Prefix: os.Getenv("E2E_PREFIX"), IPv6Mode: os.Getenv("E2E_MODE"),
		RAInterval: 3, RARouterLifetime: &lifetime,
	}})
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[1], []byte(artifacts[0].Content), 0600); err != nil {
		panic(err)
	}
}
EOF

cat >"$tmpdir/udhcpc6.sh" <<'EOF'
#!/bin/sh
case "$1" in
  bound|renew|inform)
    {
      echo "ip=${ip:-}"
      echo "ipv6=${ipv6:-}"
      echo "ip6=${ip6:-}"
      echo "dns=${dns:-}"
      env | sort >"$LEASE_FILE.env"
    } >"$LEASE_FILE"
    ;;
esac
EOF
chmod +x "$tmpdir/udhcpc6.sh"

run_case() {
	mode=$1
	prefix=$2
	ns="ly-route-dnsmasq-v6-${mode}-$$"
	server="v6s-$$"
	client="v6c-$$"
	namespaces="$namespaces $ns"
	ip netns add "$ns"
	ip link add "$server" type veth peer name "$client"
	ip link set "$server" netns "$ns"
	ip link set "$client" netns "$ns"
	ip netns exec "$ns" ip link set lo up
	ip netns exec "$ns" ip link set "$server" up
	ip netns exec "$ns" ip link set "$client" up
	ip netns exec "$ns" sysctl -qw net.ipv6.conf.all.disable_ipv6=0 \
		net.ipv6.conf."$server".disable_ipv6=0 net.ipv6.conf."$client".disable_ipv6=0
	ip netns exec "$ns" sysctl -qw net.ipv6.conf.all.forwarding=0 \
		net.ipv6.conf."$server".forwarding=0 net.ipv6.conf."$client".forwarding=0 \
		net.ipv6.conf."$client".accept_ra=2 net.ipv6.conf."$client".autoconf=1
	# The LCP mirror owns the delegated LAN address in the product. Here the
	# server veth plays that role so dnsmasq has a prefix address to advertise.
	ip netns exec "$ns" ip addr add "$prefix::1/64" dev "$server" nodad

	(cd "$repo_root/backend" && E2E_IFACE="$server" E2E_PREFIX="$prefix::/64" E2E_MODE="$mode" \
		go run "$helper_dir" "$tmpdir/rendered-$mode.conf")
	sed -i "s#^dhcp-leasefile=.*#dhcp-leasefile=$tmpdir/lease-$mode#" "$tmpdir/rendered-$mode.conf"
	cat >>"$tmpdir/rendered-$mode.conf" <<CONF
pid-file=$tmpdir/pid-$mode
log-facility=$tmpdir/log-$mode
CONF

	ip netns exec "$ns" "$dnsmasq_bin" --conf-file="$tmpdir/rendered-$mode.conf" \
		--no-daemon --keep-in-foreground >"$tmpdir/stdout-$mode" 2>&1 &
	dnsmasq_pid=$!
	sleep 1
	kill -0 "$dnsmasq_pid" 2>/dev/null || {
		echo "dnsmasq exited during $mode startup" >&2
		cat "$tmpdir/stdout-$mode" >&2
		exit 1
	}

	lease="$tmpdir/lease.env-$mode"
	: >"$lease"
	if [ "$mode" = stateless ]; then
		# Wait for RA/SLAAC, then ask for stateless DHCPv6 information (DNS).
		attempt=0
		while [ "$attempt" -lt 10 ]; do
			if ip netns exec "$ns" ip -6 addr show dev "$client" | grep -F "inet6 $prefix:" >/dev/null; then
				break
			fi
			attempt=$((attempt + 1))
			sleep 1
		done
		ip netns exec "$ns" ip -6 addr show dev "$client" | grep -F "inet6 $prefix:" >/dev/null || {
			echo "client did not form a SLAAC address from $prefix" >&2
			cat "$tmpdir/stdout-$mode" >&2
			exit 1
		}
		ip netns exec "$ns" ip -6 route show default | grep -F 'via fe80:' >/dev/null || {
			echo "client did not install an IPv6 default route" >&2
			exit 1
		}
		timeout 15 ip netns exec "$ns" env LEASE_FILE="$lease" \
			busybox udhcpc6 -l -f -q -n -t 5 -T 1 -i "$client" -s "$tmpdir/udhcpc6.sh" \
			>"$tmpdir/udhcpc6-$mode.log" 2>&1 || true
	else
		timeout 20 ip netns exec "$ns" env LEASE_FILE="$lease" \
			busybox udhcpc6 -f -q -n -t 8 -T 1 -i "$client" -s "$tmpdir/udhcpc6.sh" \
			>"$tmpdir/udhcpc6-$mode.log" 2>&1 || true
	fi

	[ -s "$lease" ] || {
		echo "client did not complete DHCPv6 ($mode)" >&2
		cat "$tmpdir/udhcpc6-$mode.log" >&2
		cat "$tmpdir/stdout-$mode" >&2
		exit 1
	}
	# shellcheck disable=SC1090
	. "$lease"
	normalize6() {
		python3 -c 'import ipaddress,sys; print(ipaddress.IPv6Address(sys.argv[1].strip()))' "$1"
	}
	want_dns=$(normalize6 "$prefix::1")
	found_dns=0
	for server in ${dns:-}; do
		if [ "$(normalize6 "$server")" = "$want_dns" ]; then
			found_dns=1
		fi
	done
	[ "$found_dns" -eq 1 ] || {
		echo "DHCPv6 DNS '${dns:-}' does not contain $prefix::1 ($mode)" >&2
		exit 1
	}
	if [ "$mode" = stateful ]; then
		assigned=${ip:-${ipv6:-${ip6:-}}}
		[ -n "$assigned" ] || {
			echo "stateful DHCPv6 returned no IPv6 address" >&2
			cat "$LEASE_FILE.env" >&2 2>/dev/null || true
			cat "$tmpdir/udhcpc6-$mode.log" >&2
			cat "$tmpdir/stdout-$mode" >&2
			exit 1
		}
		python3 -c 'import ipaddress,sys; sys.exit(0 if ipaddress.ip_address(sys.argv[1]) in ipaddress.ip_network(sys.argv[2], strict=False) else 1)' "$assigned" "$prefix::/64" || {
			echo "stateful DHCPv6 address '$assigned' is outside $prefix" >&2
			exit 1
		}
	fi

	kill "$dnsmasq_pid" 2>/dev/null || true
	wait "$dnsmasq_pid" 2>/dev/null || true
	dnsmasq_pid=
	ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
	ip netns del "$ns"
	namespaces=""
	echo "dnsmasq IPv6 $mode e2e passed: prefix=$prefix dns=$dns"
}

run_case stateless 2001:db8:100
run_case stateful 2001:db8:200
