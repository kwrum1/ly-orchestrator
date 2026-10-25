#!/bin/sh
# NAT44 outside-address selection across a WAN primary/backup failover.
#
# Real VPP, real packets: the source address observed by a dual-homed target
# must be the egress WAN's own outside address before failover, after failover,
# and after failback.  This guards the bug where every WAN used the primary
# outside address because the pool was installed with the interface-address
# command (fib_index ~0) instead of an explicit inside-VRF address.
set -eu

image=${LY_ROUTE_VPP_TEST_IMAGE:-ly-route/vpp-test:25.10}
suffix=${LY_ROUTE_TEST_SUFFIX:-$$}
container="ly-route-nat-wan-source-$suffix"
client_ns="lyroute-natws-client-$suffix"
target_ns="lyroute-natws-target-$suffix"
bridge1="nbw1-$suffix"
bridge2="nbw2-$suffix"
lan_bridge="nbwl-$suffix"
socket="/run/vpp/nat-wan-source-$suffix.sock"
nft_chain="lyws_$suffix"
work=""

for cmd in docker ip nft nsenter python3 curl; do
	command -v "$cmd" >/dev/null 2>&1 || { echo "SKIP: $cmd is required" >&2; exit 78; }
done

cleanup() {
	(
		set +e
		ip netns del "$client_ns" >/dev/null 2>&1
		ip netns del "$target_ns" >/dev/null 2>&1
		for link in "nwsl$suffix" "nwsh1$suffix" "nwsh2$suffix" "nwslh$suffix" "nwsw1$suffix" "nwsw2$suffix"; do
			ip link del "$link" >/dev/null 2>&1
		done
		handle=$(nft -a list chain ip filter FORWARD 2>/dev/null | awk -v c="$nft_chain" '$0 ~ ("jump " c "[[:space:]]") {print $NF}')
		[ -n "$handle" ] && nft delete rule ip filter FORWARD handle "$handle" >/dev/null 2>&1
		nft flush chain ip filter "$nft_chain" >/dev/null 2>&1
		nft delete chain ip filter "$nft_chain" >/dev/null 2>&1
		ip link del "$bridge1" >/dev/null 2>&1
		ip link del "$bridge2" >/dev/null 2>&1
		ip link del "$lan_bridge" >/dev/null 2>&1
		docker rm -f "$container" >/dev/null 2>&1
		rm -rf "$work"
	)
}
trap cleanup EXIT INT TERM
cleanup
work=$(mktemp -d "/tmp/ly-route-nat-wan-source.XXXXXX")

docker run -d --name "$container" --privileged --network none --shm-size 512m \
	"$image" /usr/bin/vpp unix "{ nodaemon cli-listen $socket }" \
	api-segment "{ prefix natwansource$suffix }" \
	plugins "{ plugin dpdk_plugin.so { disable } }" >/dev/null
vpp_pid=$(docker inspect -f '{{.State.Pid}}' "$container")
vppctl() { docker exec "$container" vppctl -s "$socket" "$@"; }
ready=false
for _ in $(seq 1 150); do
	if vppctl show version >/dev/null 2>&1; then ready=true; break; fi
	sleep .2
done
if [ "$ready" != true ]; then
	echo "VPP CLI did not become ready" >&2
	docker logs "$container" >&2 || true
	exit 1
fi

ip netns add "$client_ns"
ip netns add "$target_ns"
ip link add "$lan_bridge" type bridge
ip link add "$bridge1" type bridge
ip link add "$bridge2" type bridge
ip link set "$lan_bridge" up
ip link set "$bridge1" up
ip link set "$bridge2" up

# client -> LAN bridge -> VPP
ip link add "nwsc$suffix" type veth peer name "nwsl$suffix"
ip link set "nwsc$suffix" netns "$client_ns"
ip link set "nwsl$suffix" master "$lan_bridge"
ip link set "nwsl$suffix" up

# target: one veth on each WAN bridge
ip link add "nwst1$suffix" type veth peer name "nwsw1$suffix"
ip link set "nwst1$suffix" netns "$target_ns"
ip link set "nwsw1$suffix" master "$bridge1"
ip link set "nwsw1$suffix" up
ip link add "nwst2$suffix" type veth peer name "nwsw2$suffix"
ip link set "nwst2$suffix" netns "$target_ns"
ip link set "nwsw2$suffix" master "$bridge2"
ip link set "nwsw2$suffix" up

# VPP-side veths
ip link add "nwsv1$suffix" type veth peer name "nwsh1$suffix"
ip link set "nwsv1$suffix" netns "$vpp_pid"
ip link set "nwsh1$suffix" master "$bridge1"
ip link set "nwsh1$suffix" up
ip link add "nwsv2$suffix" type veth peer name "nwsh2$suffix"
ip link set "nwsv2$suffix" netns "$vpp_pid"
ip link set "nwsh2$suffix" master "$bridge2"
ip link set "nwsh2$suffix" up
ip link add "nwslv$suffix" type veth peer name "nwslh$suffix"
ip link set "nwslv$suffix" netns "$vpp_pid"
ip link set "nwslh$suffix" master "$lan_bridge"
ip link set "nwslh$suffix" up

ip addr add 100.77.0.1/24 dev "$bridge1"
ip addr add 198.51.100.1/24 dev "$bridge1"
ip addr add 100.78.0.1/24 dev "$bridge2"
ip addr add 198.51.101.1/24 dev "$bridge2"
sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0

# Docker may set the host FORWARD policy to drop. Scope an allowance to exactly
# these two WAN fixtures instead of weakening the global policy.
nft add chain ip filter "$nft_chain" >/dev/null
nft add rule ip filter "$nft_chain" ip saddr 100.77.0.0/24 ip daddr 198.51.100.0/24 counter accept
nft add rule ip filter "$nft_chain" ip saddr 100.78.0.0/24 ip daddr 198.51.100.0/24 counter accept
nft add rule ip filter "$nft_chain" ip saddr 100.78.0.0/24 ip daddr 198.51.101.0/24 counter accept
nft add rule ip filter "$nft_chain" ip saddr 198.51.100.0/24 ip daddr 100.77.0.0/24 counter accept
nft add rule ip filter "$nft_chain" ip saddr 198.51.100.0/24 ip daddr 100.78.0.0/24 counter accept
nft add rule ip filter "$nft_chain" ip saddr 198.51.101.0/24 ip daddr 100.78.0.0/24 counter accept
nft insert rule ip filter FORWARD jump "$nft_chain"

ip -n "$client_ns" link set lo up
ip -n "$client_ns" link set "nwsc$suffix" up
ip -n "$client_ns" addr add 192.168.77.203/24 dev "nwsc$suffix"
ip -n "$client_ns" route add default via 192.168.77.1
ip netns exec "$client_ns" sysctl -qw net.ipv4.conf.all.rp_filter=0 "net.ipv4.conf.nwsc$suffix.rp_filter=0"

ip -n "$target_ns" link set lo up
ip -n "$target_ns" link set "nwst1$suffix" up
ip -n "$target_ns" link set "nwst2$suffix" up
# Two targets: one per WAN, so both WANs can carry their own egress at the
# same time.  The WAN1 target stays reachable through WAN2 after failover via
# the fixture gateways (the host routes between the two WAN bridges).
ip -n "$target_ns" addr add 198.51.100.10/24 dev "nwst1$suffix"
ip -n "$target_ns" addr add 198.51.101.10/24 dev "nwst2$suffix"
ip -n "$target_ns" route add default via 198.51.100.1 dev "nwst1$suffix"
ip -n "$target_ns" route add 100.77.0.1/32 dev "nwst1$suffix"
ip -n "$target_ns" route add 100.78.0.1/32 dev "nwst2$suffix"
ip -n "$target_ns" route add 100.77.0.0/24 via 100.77.0.1 dev "nwst1$suffix"
ip -n "$target_ns" route add 100.78.0.0/24 via 100.78.0.1 dev "nwst2$suffix"
ip netns exec "$target_ns" sysctl -qw net.ipv4.conf.all.rp_filter=0 \
	"net.ipv4.conf.nwst1$suffix.rp_filter=0" "net.ipv4.conf.nwst2$suffix.rp_filter=0"

nsenter -t "$vpp_pid" -n ip link set "nwslv$suffix" up
nsenter -t "$vpp_pid" -n ip link set "nwsv1$suffix" up
nsenter -t "$vpp_pid" -n ip link set "nwsv2$suffix" up
vppctl create host-interface name "nwslv$suffix" >/dev/null
vppctl create host-interface name "nwsv1$suffix" >/dev/null
vppctl create host-interface name "nwsv2$suffix" >/dev/null
lan="host-nwslv$suffix"
wan1="host-nwsv1$suffix"
wan2="host-nwsv2$suffix"
vppctl set interface state "$lan" up
vppctl set interface state "$wan1" up
vppctl set interface state "$wan2" up
vppctl set interface ip address "$lan" 192.168.77.1/24
vppctl set interface ip address "$wan1" 100.77.0.2/24
vppctl set interface ip address "$wan2" 100.78.0.2/24
vppctl ip route add 198.51.100.10/32 via 100.77.0.1 "$wan1"
vppctl ip route add 198.51.101.10/32 via 100.78.0.1 "$wan2"

# Product command contract: an explicit inside-VRF pool address, not the
# interface-address form that stores fib_index ~0 and ignores the egress WAN.
vppctl nat44 plugin enable >/dev/null
vppctl set interface nat44 in "$lan"
vppctl set interface nat44 out "$wan1" output-feature
vppctl set interface nat44 out "$wan2" output-feature
vppctl nat44 add address 100.77.0.2 tenant-vrf 0
vppctl nat44 add address 100.78.0.2 tenant-vrf 0
vppctl show nat44 addresses >"$work/nat-addresses.txt"

cat >"$work/target.py" <<'PY'
import http.server, socketserver, sys, threading

log = sys.argv[1]
lock = threading.Lock()

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with lock:
            with open(log, "a") as handle:
                handle.write("%s %s\n" % (self.server.server_address[0], self.client_address[0]))
        body = b"ok"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_args):
        pass

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

servers = [Server((address, 8080), Handler) for address in ("198.51.100.10", "198.51.101.10")]
for server in servers:
    threading.Thread(target=server.serve_forever, daemon=True).start()
threading.Event().wait()
PY
ip netns exec "$target_ns" python3 "$work/target.py" "$work/sources.log" >"$work/target.out" 2>&1 &
target_pid=$!
sleep 1

probe() { # ip label expected
	ip=$1
	label=$2
	expected=$3
	last=none
	code=000
	attempt=1
	while [ "$attempt" -le 15 ]; do
		code=$(ip netns exec "$client_ns" curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "http://$ip:8080/" 2>/dev/null || true)
		last=$(awk -v ip="$ip" '$1 == ip {src=$2} END {if (src == "") print "none"; else print src}' "$work/sources.log" 2>/dev/null || echo none)
		if [ "$code" = 200 ] && [ "$last" = "$expected" ]; then
			echo "$label: http=$code source=$last"
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "$label: http=${code:-000} source=$last (want $expected)" >&2
	vppctl show ip fib "$ip" >&2 || true
	vppctl show ip neighbors >&2 || true
	vppctl show nat44 sessions >&2 || true
	ip -n "$target_ns" route show >&2 || true
	ip -n "$target_ns" neigh show >&2 || true
	cat "$work/target.out" >&2 || true
	return 1
}

# Both WANs are simultaneously usable for their own egress.
probe 198.51.100.10 "baseline wan1" 100.77.0.2
probe 198.51.101.10 "simultaneous wan2" 100.78.0.2

# Fail the primary dataplane link and move the same service to the backup.
vppctl set interface state "$wan1" down
vppctl ip route del 198.51.100.10/32 via 100.77.0.1 "$wan1" >/dev/null 2>&1 || true
vppctl ip route add 198.51.100.10/32 via 100.78.0.1 "$wan2"
probe 198.51.100.10 "failover wan2" 100.78.0.2

# Fail back and prove the primary outside address is selected again.
vppctl set interface state "$wan1" up
vppctl ip route del 198.51.100.10/32 via 100.78.0.1 "$wan2" >/dev/null 2>&1 || true
vppctl ip route add 198.51.100.10/32 via 100.77.0.1 "$wan1"
probe 198.51.100.10 "failback wan1" 100.77.0.2

vppctl show nat44 addresses
kill "$target_pid" >/dev/null 2>&1 || true
wait "$target_pid" 2>/dev/null || true
echo "NAT44 per-egress WAN outside-address selection passed"
