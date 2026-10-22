#!/bin/sh
# Independent netns acceptance for the generated proxy capture plan.
#
# It renders the real NftablesCapturePlan from backend/internal/runtime/proxy,
# applies that output inside four throwaway network namespaces, and verifies:
#   1. selected traffic is TPROXY diverted to the local proxy listener,
#   2. LAN / DNS destinations bypass capture and stay direct,
#   3. flushing the managed table is fail-open (traffic is forwarded, not
#      black-holed), and re-applying the same file is idempotent.
#
# Requires root, nft, ip, go. It never touches the host firewall or the lab.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BACKEND="$ROOT/backend"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ly-proxy-netns.XXXXXX")
SUFFIX=$$
CLIENT="lypc$SUFFIX"
PROXY="lypp$SUFFIX"
LAN="lypl$SUFFIX"
UPSTREAM="lypu$SUFFIX"
TPROXY_PID=
LAN_PID=
UP_PID=
DNS_PID=
KEEP=${KEEP:-0}

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
    set +e
    for pid in "$TPROXY_PID" "$LAN_PID" "$UP_PID" "$DNS_PID"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null
    done
    if [ "$KEEP" = "1" ]; then
        log "KEEP=1; namespaces $CLIENT $PROXY $LAN $UPSTREAM and $TMP left in place"
        return
    fi
    ip netns del "$CLIENT" 2>/dev/null
    ip netns del "$PROXY" 2>/dev/null
    ip netns del "$LAN" 2>/dev/null
    ip netns del "$UPSTREAM" 2>/dev/null
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

log "== render generated capture plan =="
go build -o "$TMP/probe" ./internal/runtime/service/testdata/probe
go run ./internal/runtime/service/testdata/render -mode json -lan 10.200.1.0/24 >"$TMP/plan.json"
go run ./internal/runtime/service/testdata/render -mode nft -lan 10.200.1.0/24 >"$TMP/capture.nft"
python3 - "$TMP/plan.json" >"$TMP/fields" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1], encoding="utf-8"))
network = plan["network"]
capture = plan["capture"]
routing = plan["routing"]
print(network["ingress_host_interface"])
print(network["listener_port"])
print(capture["mark"])
print(routing["table_id"])
print(routing["rule_priority"])
PY
INGRESS=$(sed -n 1p "$TMP/fields")
PORT=$(sed -n 2p "$TMP/fields")
MARK=$(sed -n 3p "$TMP/fields")
TABLE=$(sed -n 4p "$TMP/fields")
PRIORITY=$(sed -n 5p "$TMP/fields")
log "ingress=$INGRESS tproxy-port=$PORT mark=$MARK table=$TABLE rule-priority=$PRIORITY"
grep -q "tproxy ip to :$PORT" "$TMP/capture.nft" || fail "rendered nft file has no TPROXY rule for port $PORT"
grep -q "ip daddr 10.200.1.0/24" "$TMP/capture.nft" || fail "rendered nft file has no LAN bypass"

log "== create isolated namespaces =="
ip netns add "$CLIENT"
ip netns add "$PROXY"
ip netns add "$LAN"
ip netns add "$UPSTREAM"
ip link add "$INGRESS" netns "$PROXY" type veth peer name c0 netns "$CLIENT"
ip link add p_lan netns "$PROXY" type veth peer name l0 netns "$LAN"
ip link add p_up netns "$PROXY" type veth peer name u0 netns "$UPSTREAM"

ip -n "$CLIENT" link set lo up
ip -n "$CLIENT" link set c0 up
ip -n "$CLIENT" addr add 10.200.0.2/24 dev c0
ip -n "$CLIENT" route add default via 10.200.0.1

ip -n "$PROXY" link set lo up
ip -n "$PROXY" link set "$INGRESS" up
ip -n "$PROXY" addr add 10.200.0.1/24 dev "$INGRESS"
ip -n "$PROXY" link set p_lan up
ip -n "$PROXY" addr add 10.200.1.1/24 dev p_lan
ip -n "$PROXY" link set p_up up
ip -n "$PROXY" addr add 203.0.113.1/24 dev p_up

ip -n "$LAN" link set lo up
ip -n "$LAN" link set l0 up
ip -n "$LAN" addr add 10.200.1.2/24 dev l0
ip -n "$LAN" route add default via 10.200.1.1

ip -n "$UPSTREAM" link set lo up
ip -n "$UPSTREAM" link set u0 up
ip -n "$UPSTREAM" addr add 203.0.113.10/24 dev u0
ip -n "$UPSTREAM" route add default via 203.0.113.1

ip netns exec "$PROXY" sysctl -q -w net.ipv4.ip_forward=1
ip netns exec "$PROXY" sysctl -q -w net.ipv4.conf.all.rp_filter=0
ip netns exec "$PROXY" sysctl -q -w "net.ipv4.conf.$INGRESS.rp_filter=0"
ip netns exec "$PROXY" sysctl -q -w net.ipv4.ip_nonlocal_bind=1

log "== install generated capture and policy route =="
ip netns exec "$PROXY" nft -f "$TMP/capture.nft"
ip netns exec "$PROXY" ip rule add fwmark "$MARK/0xffffffff" table "$TABLE" priority "$PRIORITY"
ip netns exec "$PROXY" ip route add local 0.0.0.0/0 dev lo table "$TABLE"

ip netns exec "$LAN" "$TMP/probe" -mode echo -listen 10.200.1.2:80 -reply DIRECT-LAN >"$TMP/lan.log" 2>&1 &
LAN_PID=$!
ip netns exec "$UPSTREAM" "$TMP/probe" -mode echo -listen 203.0.113.10:80 -reply DIRECT-UPSTREAM >"$TMP/up.log" 2>&1 &
UP_PID=$!
ip netns exec "$UPSTREAM" "$TMP/probe" -mode echo -listen 203.0.113.10:53 -reply DIRECT-DNS >"$TMP/dns.log" 2>&1 &
DNS_PID=$!
ip netns exec "$PROXY" "$TMP/probe" -mode tproxy -listen "0.0.0.0:$PORT" -reply PROXIED >"$TMP/tproxy.log" 2>&1 &
TPROXY_PID=$!
sleep 1

check() {
    label=$1
    destination=$2
    expected=$3
    output=$(ip netns exec "$CLIENT" "$TMP/probe" -mode client -addr "$destination" -attempts 5 2>&1) || true
    log "$label -> $output"
    printf '%s\n' "$output" | grep -q "$expected" || fail "$label expected $expected, got: $output"
}

log "== path 1: selected traffic enters TPROXY =="
check selected 203.0.113.10:80 PROXIED

log "== path 2: LAN and DNS stay direct =="
check lan 10.200.1.2:80 DIRECT-LAN
check dns 203.0.113.10:53 DIRECT-DNS

log "== idempotent re-apply keeps the same behaviour =="
ip netns exec "$PROXY" nft -f "$TMP/capture.nft"
check selected-after-reapply 203.0.113.10:80 PROXIED
check lan-after-reapply 10.200.1.2:80 DIRECT-LAN

log "== path 3: flushed/unavailable capture is fail-open =="
ip netns exec "$PROXY" nft flush table inet ly_route_proxy_capture
if ip netns exec "$PROXY" nft list table inet ly_route_proxy_capture | grep -q tproxy; then
    fail "flushed capture table still contains TPROXY rules"
fi
check fail-open 203.0.113.10:80 DIRECT-UPSTREAM
check lan-after-flush 10.200.1.2:80 DIRECT-LAN

log "PASS: selected=TPROXY, lan/dns=direct, flush=fail-open, reapply=idempotent"
