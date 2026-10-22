#!/bin/sh
# Regression test for packaging/rootfs-overlay/usr/lib/ly-route/proxy-failopen.
# Uses a fake vppctl/nft so it can run without VPP or root.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HELPER="$ROOT/packaging/rootfs-overlay/usr/lib/ly-route/proxy-failopen"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ly-proxy-failopen.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

[ -x "$HELPER" ] || { echo "FAIL: helper is not executable: $HELPER" >&2; exit 1; }

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
export LY_ROUTE_NFT_CONF="$TMP/nftables.conf"
export LY_ROUTE_XRAY_CONFIG="$TMP/xray.json"
export LY_ROUTE_LISTENER_WAIT_ATTEMPTS=1

cat >"$TMP/fake-vppctl" <<'EOF'
#!/bin/sh
[ "$1" = "show" ] && cat "$LY_ROUTE_PRE_NAT_SHOW" && exit 0
printf '%s\n' "$*" >>"$LY_ROUTE_VPP_LOG"
EOF
cat >"$TMP/fake-nft" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LY_ROUTE_NFT_LOG"
EOF
chmod +x "$TMP/fake-vppctl" "$TMP/fake-nft"
: >"$VPP_LOG"
: >"$NFT_LOG"
printf '#!/usr/sbin/nft -f\n' >"$LY_ROUTE_NFT_CONF"

# Existing bypass 9001 must be removed before capture is installed.
"$HELPER" on
grep -q "set ly-route pre-nat-route del id 9001" "$VPP_LOG" ||
    { echo "FAIL: on did not remove the existing bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }
grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG" ||
    { echo "FAIL: on did not reinstall the persisted capture" >&2; cat "$NFT_LOG" >&2; exit 1; }
if grep -q "set ly-route pre-nat-route add " "$VPP_LOG"; then
    echo "FAIL: healthy on must not add a bypass" >&2
    exit 1
fi

# Stop: bypass must be added before the host capture chain is flushed.
: >"$VPP_LOG"
: >"$NFT_LOG"
"$HELPER" off
grep -q "set ly-route pre-nat-route add id 9001 priority 9 source 192.168.88.0/24 destination 100.64.0.1/32 protocol tcp sport 0-65535 dport 0-65535 bypass" "$VPP_LOG" ||
    { echo "FAIL: off did not add the matching VPP bypass" >&2; cat "$VPP_LOG" >&2; exit 1; }
grep -q "flush chain inet ly_route_proxy_capture proxy_prerouting" "$NFT_LOG" ||
    { echo "FAIL: off did not flush the capture chain" >&2; cat "$NFT_LOG" >&2; exit 1; }
vpp_line=$(grep -n "set ly-route pre-nat-route add " "$VPP_LOG" | head -1 | cut -d: -f1)
nft_line=$(grep -n "flush chain" "$NFT_LOG" | head -1 | cut -d: -f1)
[ -n "$vpp_line" ] && [ -n "$nft_line" ] ||
    { echo "FAIL: missing bypass/flush evidence" >&2; exit 1; }

# Idempotency: a second off replaces, not duplicates, the reserved ids.
"$HELPER" off
[ "$(grep -c "set ly-route pre-nat-route add id 9001" "$VPP_LOG")" -eq 2 ] ||
    { echo "FAIL: repeated off is not deterministic" >&2; cat "$VPP_LOG" >&2; exit 1; }

# Listener not ready: keep capture fail-open and install the bypass.
printf '{"inbounds":[{"protocol":"dokodemo-door","port":1,"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}\n' >"$LY_ROUTE_XRAY_CONFIG"
: >"$VPP_LOG"
: >"$NFT_LOG"
"$HELPER" on
if grep -q -- "-f $LY_ROUTE_NFT_CONF" "$NFT_LOG"; then
    echo "FAIL: on installed capture without a ready TPROXY listener" >&2
    exit 1
fi
grep -q "set ly-route pre-nat-route add id 9001" "$VPP_LOG" ||
    { echo "FAIL: on did not fail open when the listener stayed down" >&2; cat "$VPP_LOG" >&2; exit 1; }
grep -q "flush chain inet ly_route_proxy_capture proxy_prerouting" "$NFT_LOG" ||
    { echo "FAIL: on did not keep capture flushed while listener was down" >&2; cat "$NFT_LOG" >&2; exit 1; }

echo 'PASS: proxy fail-open helper adds VPP bypass, flushes capture, reinstalls on healthy start and is idempotent'
