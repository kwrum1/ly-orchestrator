#!/bin/sh
# Independent netns regression for the DNS LCP guard scoping.
#
# The guard must drop only the duplicate DNS copy that Linux receives on the
# VPP LCP host interface (lylan-*). SmartDNS listens on the same LAN gateway
# address, so normal LAN and DNS-service-TAP input on udp/tcp 53 must stay
# deliverable.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BACKEND="$ROOT/backend"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ly-dns-guard.XXXXXX")
SUFFIX=$$
ROUTER="lydg-r$SUFFIX"
LANC="lydg-l$SUFFIX"
LCPC="lydg-c$SUFFIX"
DNS_PID=
KEEP=${KEEP:-0}

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
    set +e
    [ -n "$DNS_PID" ] && kill "$DNS_PID" 2>/dev/null
    if [ "$KEEP" = "1" ]; then
        log "KEEP=1; namespaces and $TMP left in place"
        return
    fi
    ip netns del "$ROUTER" 2>/dev/null
    ip netns del "$LANC" 2>/dev/null
    ip netns del "$LCPC" 2>/dev/null
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

[ "$(id -u)" -eq 0 ] || fail "must run as root"
for command in ip nft go python3; do
    command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

export GOMODCACHE=${GOMODCACHE:-/root/go/pkg/mod-alt}
export GOPROXY=${GOPROXY:-off}
cd "$BACKEND"

log "== render guard and build probe =="
go build -o "$TMP/probe" ./internal/runtime/service/testdata/probe
go run ./internal/runtime/service/testdata/render -mode guard \
    -guard-address 192.168.88.1 -guard-interface lylan-eth2 >"$TMP/guard.nft"
grep -q 'iifname "lylan-eth2" ip daddr 192.168.88.1 udp dport 53 counter drop' "$TMP/guard.nft" ||
    fail "rendered guard is not scoped to the LCP interface"
if grep -q '^    ip daddr 192.168.88.1' "$TMP/guard.nft"; then
    fail "rendered guard contains an unscoped DNS drop"
fi

log "== namespaces =="
ip netns add "$ROUTER"
ip netns add "$LANC"
ip netns add "$LCPC"
ip link add lan0 netns "$ROUTER" type veth peer name c0 netns "$LANC"
ip link add lylan-eth2 netns "$ROUTER" type veth peer name c1 netns "$LCPC"

ip -n "$ROUTER" link set lo up
ip -n "$ROUTER" link set lan0 up
ip -n "$ROUTER" addr add 192.168.88.1/24 dev lan0
ip -n "$ROUTER" link set lylan-eth2 up
ip -n "$ROUTER" addr add 10.99.0.1/24 dev lylan-eth2
ip netns exec "$ROUTER" sysctl -q -w net.ipv4.conf.all.rp_filter=0
ip netns exec "$ROUTER" sysctl -q -w net.ipv4.conf.lan0.rp_filter=0
ip netns exec "$ROUTER" sysctl -q -w net.ipv4.conf.lylan-eth2.rp_filter=0 2>/dev/null || true

ip -n "$LANC" link set lo up
ip -n "$LANC" link set c0 up
ip -n "$LANC" addr add 192.168.88.2/24 dev c0
ip -n "$LANC" route add default via 192.168.88.1

ip -n "$LCPC" link set lo up
ip -n "$LCPC" link set c1 up
ip -n "$LCPC" addr add 10.99.0.2/24 dev c1
ip -n "$LCPC" route add 192.168.88.1/32 via 10.99.0.1

log "== install generated guard =="
ip netns exec "$ROUTER" nft -f "$TMP/guard.nft"
ip netns exec "$ROUTER" "$TMP/probe" -mode dns -listen 0.0.0.0:53 >"$TMP/dns.log" 2>&1 &
DNS_PID=$!
sleep 1

dns_query() {
    ns=$1
    proto=$2
    ip netns exec "$ns" python3 - "$proto" <<'PY'
import socket, sys
proto = sys.argv[1]
try:
    if proto == "udp":
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.settimeout(2)
        s.sendto(b"ping", ("192.168.88.1", 53))
        data, _ = s.recvfrom(64)
    else:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(2)
        s.connect(("192.168.88.1", 53))
        s.sendall(b"ping")
        data = s.recv(64)
    print(data.decode(errors="replace"))
except Exception:
    print("TIMEOUT")
PY
}

check() {
    label=$1
    ns=$2
    proto=$3
    expected=$4
    output=$(dns_query "$ns" "$proto")
    log "$label -> $output"
    [ "$output" = "$expected" ] || fail "$label expected $expected, got $output"
}

log "== normal LAN DNS must reach SmartDNS =="
check lan-udp "$LANC" udp DNS-OK
check lan-tcp "$LANC" tcp DNS-OK

log "== duplicate LCP DNS copies must be dropped =="
check lcp-udp "$LCPC" udp TIMEOUT
check lcp-tcp "$LCPC" tcp TIMEOUT

log "PASS: LAN 53 udp/tcp delivered; LCP duplicate udp/tcp dropped by scoped guard"
