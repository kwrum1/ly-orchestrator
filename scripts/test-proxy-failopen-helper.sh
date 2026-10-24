#!/bin/sh
# Regression test for packaging/rootfs-overlay/usr/lib/ly-route/proxy-failopen
# and the packaged proxy-failure helper.
#
# It uses fake nft/vppctl/ss so it can run without VPP, nginx or root.
# Coverage:
#   - no proxy egress selected: legacy VPP bypass (direct is correct),
#   - proxy egress selected and Xray stopping: VPP steering stays on the
#     ingress TAP and the local failure page is installed (no direct bypass),
#   - healthy start: capture reinstalled and the failure page removed,
#   - TPROXY listener not ready: page/blackhole instead of direct,
#   - page listener not ready: blackhole guard instead of the page,
#   - helper missing: inline blackhole, still no direct bypass,
#   - idempotency of the failure reservation.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HELPER="$ROOT/packaging/rootfs-overlay/usr/lib/ly-route/proxy-failopen"
FAILURE="$ROOT/packaging/rootfs-overlay/usr/lib/ly-route/proxy-failure"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ly-proxy-failopen.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

[ -x "$HELPER" ] || { echo "FAIL: helper is not executable: $HELPER" >&2; exit 1; }
[ -x "$FAILURE" ] || { echo "FAIL: failure helper is not executable: $FAILURE" >&2; exit 1; }

cat >"$TMP/show.txt" <<'EOF'
enabled 1 interface lyroute-eth2 lan-prefix 192.168.88.0/24 rules 4 radix-nodes 57
rule id 15756 priority 10 source 192.168.88.0/24 destination 100.64.0.1/32 protocol 6 sport 0-65535 dport 0-65535 table 97664 fib-index 3 skip-nat 1 bypass 0
rule id 65000 priority 0 source 0.0.0.0/0 destination 0.0.0.0/0 protocol 17 sport 68-68 dport 67-67 table 0 fib-index 0 skip-nat 0 bypass 1
rule id 64999 priority 1 source 192.168.88.0/24 destination 192.168.88.0/24 protocol 0 sport 0-65535 dport 0-65535 table 0 fib-index 0 skip-nat 0 bypass 1
rule id 9001 priority 9 source 192.168.88.0/24 destination 100.64.0.1/32 protocol 6 sport 0-65535 dport 0-65535 table 0 fib-index 0 skip-nat 0 bypass 1
EOF

VPP_LOG="$TMP/vpp.log"
NFT_LOG="$TMP/nft.log"
export LY_ROUTE_PRE_NAT_SHOW="$TMP/show.txt"
export LY_ROUTE_VPP_LOG="$VPP_LOG"
export LY_ROUTE_NFT_LOG="$NFT_LOG"
export LY_ROUTE_NFT="$TMP/fake-nft"
export LY_ROUTE_VPPCTL="$TMP/fake-vppctl"
export LY_ROUTE_SS="$TMP/fake-ss"
export LY_ROUTE_NFT_CONF="$TMP/nftables.conf"
export LY_ROUTE_XRAY_CONFIG="$TMP/xray.json"
export LY_ROUTE_LISTENER_WAIT_ATTEMPTS=1
export LY_ROUTE_PAGE_WAIT_ATTEMPTS=1
export LY_ROUTE_PROXY_FAILURE_HELPER="$FAILURE"
export LY_ROUTE_PROXY_FAILURE_STATE="$TMP/failure.json"

cat >"$TMP/fake-vppctl" <<'EOF'
#!/bin/sh
[ "$1" = "show" ] && cat "$LY_ROUTE_PRE_NAT_SHOW" && exit 0
printf '%s\n' "$*" >>"$LY_ROUTE_VPP_LOG"
EOF
cat >"$TMP/fake-nft" <<'EOF'
#!/bin/sh
if [ "$1" = "list" ]; then
    exit 0
fi
{
    printf 'NFT %s\n' "$*"
    case "$*" in
        *"-f -"*) cat ;;
    esac
} >>"$LY_ROUTE_NFT_LOG"
EOF
cat >"$TMP/fake-ss" <<'EOF'
#!/bin/sh
[ "${FAKE_PAGE_UP:-0}" = 1 ] && printf 'LISTEN 0 511 0.0.0.0:8081 0.0.0.0:*\n'
exit 0
EOF
chmod +x "$TMP/fake-vppctl" "$TMP/fake-nft" "$TMP/fake-ss"

reset_logs() {
    : >"$VPP_LOG"
    : >"$NFT_LOG"
}

printf '#!/usr/sbin/nft -f\n' >"$LY_ROUTE_NFT_CONF"
rm -f "$LY_ROUTE_PROXY_FAILURE_STATE"

# ---------------------------------------------------------------------------
# 1. No proxy egress selected: the legacy direct bypass remains correct.
# ---------------------------------------------------------------------------
reset_logs
"$HELPER" on
grep -q "set ly-route pre-nat-route del id 9001" "$VPP_LOG" ||
    { echo "FAIL: healthy on did not remove the existing bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }
grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG" ||
    { echo "FAIL: healthy on did not reinstall the persisted capture" >&2; cat "$NFT_LOG" >&2; exit 1; }
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: healthy on must not add a bypass" >&2
    exit 1
fi

reset_logs
"$HELPER" off
grep -q "set ly-route pre-nat-route add id 9001 priority 9 source 192.168.88.0/24 destination 100.64.0.1/32 protocol tcp sport 0-65535 dport 0-65535 bypass" "$VPP_LOG" ||
    { echo "FAIL: off without a proxy egress did not add the VPP bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }
grep -q "flush chain inet ly_route_proxy_capture proxy_prerouting" "$NFT_LOG" ||
    { echo "FAIL: off did not flush the capture chain" >&2; cat "$NFT_LOG" >&2; exit 1; }

# Listener not ready without a proxy egress also stays direct.
printf '{"inbounds":[{"protocol":"dokodemo-door","port":1,"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}\n' >"$LY_ROUTE_XRAY_CONFIG"
reset_logs
"$HELPER" on
if grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG"; then
    echo "FAIL: on installed capture without a ready TPROXY listener" >&2
    exit 1
fi
grep -q "set ly-route pre-nat-route add id 9001" "$VPP_LOG" ||
    { echo "FAIL: on did not fail open when the listener stayed down and no proxy egress exists" >&2; cat "$VPP_LOG" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 2. Proxy egress selected: stop/crash must publish the local failure page.
# ---------------------------------------------------------------------------
cat >"$LY_ROUTE_PROXY_FAILURE_STATE" <<'EOF'
{
  "version": 1,
  "enabled": true,
  "egress_id": "proxy-test",
  "ingress_interface": "lypxhintest",
  "page_port": 8081,
  "bypass_destinations": ["198.18.0.0/30", "192.168.88.0/24"]
}
EOF

reset_logs
FAKE_PAGE_UP=1 "$HELPER" off
grep -q "set ly-route pre-nat-route del id 9001" "$VPP_LOG" ||
    { echo "FAIL: failure mode did not remove the legacy direct bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: failure mode fell back to a direct VPP bypass" >&2
    exit 1
fi
grep -q '^add table inet ly_route_proxy_failure$' "$NFT_LOG" ||
    { echo "FAIL: failure mode did not install the failure table" >&2; cat "$NFT_LOG" >&2; exit 1; }
grep -q "redirect to :8081" "$NFT_LOG" ||
    { echo "FAIL: failure mode did not redirect TCP/80 to the page" >&2; cat "$NFT_LOG" >&2; exit 1; }
grep -q '^add table inet ly_route_proxy_failure_guard$' "$NFT_LOG" ||
    { echo "FAIL: failure mode did not install the blocking guard" >&2; cat "$NFT_LOG" >&2; exit 1; }
grep -q "flush chain inet ly_route_proxy_capture proxy_prerouting" "$NFT_LOG" ||
    { echo "FAIL: failure mode did not flush the normal capture chain" >&2; cat "$NFT_LOG" >&2; exit 1; }

# Idempotent: a second stop must not create a direct bypass.
FAKE_PAGE_UP=1 "$HELPER" off
[ "$(grep -c "set ly-route pre-nat-route add " "$VPP_LOG" || true)" -eq 0 ] ||
    { echo "FAIL: repeated stop introduced a direct bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 3. Healthy start clears the page after reinstalling capture.
# ---------------------------------------------------------------------------
python3 - "$TMP/listen.ready" <<'PY' &
import socket, sys, time
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1], file=open(sys.argv[1], "w"))
sock.listen(1)
time.sleep(8)
PY
LISTENER_PID=$!
sleep 0.6
READY_PORT=$(cat "$TMP/listen.ready")
printf '{"inbounds":[{"protocol":"dokodemo-door","port":%s,"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}\n' "$READY_PORT" >"$LY_ROUTE_XRAY_CONFIG"
reset_logs
FAKE_PAGE_UP=1 "$HELPER" on
grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG" ||
    { echo "FAIL: healthy on did not reinstall capture" >&2; cat "$NFT_LOG" >&2; exit 1; }
grep -q "delete table inet ly_route_proxy_failure" "$NFT_LOG" ||
    { echo "FAIL: healthy on did not remove the failure page" >&2; cat "$NFT_LOG" >&2; exit 1; }
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: healthy on added a direct bypass" >&2
    exit 1
fi
kill "$LISTENER_PID" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 4. TPROXY listener never ready: keep steering and publish the page.
# ---------------------------------------------------------------------------
printf '{"inbounds":[{"protocol":"dokodemo-door","port":1,"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}\n' >"$LY_ROUTE_XRAY_CONFIG"
reset_logs
FAKE_PAGE_UP=1 "$HELPER" on
if grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG"; then
    echo "FAIL: on installed capture without a ready TPROXY listener" >&2
    exit 1
fi
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: on fell back to direct when a proxy egress is selected" >&2
    exit 1
fi
grep -q "redirect to :8081" "$NFT_LOG" ||
    { echo "FAIL: on did not publish the failure page" >&2; cat "$NFT_LOG" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 5. Page server down: blackhole guard, still no direct bypass.
# ---------------------------------------------------------------------------
reset_logs
FAKE_PAGE_UP=0 "$HELPER" off
grep -q '^add table inet ly_route_proxy_failure_guard$' "$NFT_LOG" ||
    { echo "FAIL: page-down fallback did not install the guard" >&2; cat "$NFT_LOG" >&2; exit 1; }
if grep -q "redirect to :8081" "$NFT_LOG"; then
    echo "FAIL: page-down fallback still redirected to a dead page server" >&2
    exit 1
fi
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: page-down fallback fell through to direct" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# 6. Helper missing: inline blackhole is still fail closed.
# ---------------------------------------------------------------------------
reset_logs
FAKE_PAGE_UP=1 LY_ROUTE_PROXY_FAILURE_HELPER="$TMP/does-not-exist" "$HELPER" off
grep -q "table inet ly_route_proxy_failure_guard" "$NFT_LOG" ||
    { echo "FAIL: missing helper did not install the inline blackhole" >&2; cat "$NFT_LOG" >&2; exit 1; }
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: missing helper fell through to direct" >&2
    exit 1
fi

echo 'PASS: proxy fail-open fast path keeps steering and publishes the local failure page/blackhole; direct bypass only without a proxy egress'
