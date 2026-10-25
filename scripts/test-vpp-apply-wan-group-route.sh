#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ly-route-vpp-apply-group.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

mkdir -p "$tmp/bin" "$tmp/out"
cat >"$tmp/bin/vppctl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$LY_ROUTE_TEST_VPPCTL_LOG"
case "$*" in
  'show version') printf '%s\n' 'vpp v26.06' ;;
  'show interface address lyroute-enp0s4') printf '%s\n' 'lyroute-enp0s4 (up): L3 192.168.77.2/24' ;;
  'show tap') ;;
  *) ;;
esac
EOF
chmod 0755 "$tmp/bin/vppctl"

cat >"$tmp/operations.json" <<'EOF'
{"operations":[{"Name":"vpp.lan-control-lcp","Resource":"lan","Payload":{},"VPPCtlCommands":["lcp lcp-sync on","lcp create lyroute-enp0s4 host-if lylan-enp0s4"]},{"Name":"vpp.route-policy","Resource":"acc-b-wan-group-route","Payload":{"id":"acc-b-wan-group-route","priority":150,"action":"nat","egress":"acc-b-wan-group","match":{"sources":["192.168.77.0/24"],"destinations":["0.0.0.0/0"],"protocols":["any"]}},"VPPCtlCommands":["?ip table add 56340","?set ip flow-hash table 56340 src dst sport dport proto","?ip route add table 56340 0.0.0.0/0 via ip4-lookup-in-table 84675","set acl-plugin acl index 1 permit src 192.168.77.0/24 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535 tag ly-route-acc_b_wan_group_route","abf policy add id 15567 acl 1 via ip4-lookup-in-table 84675","abf attach ip4 policy 15567 priority 150 lyroute-enp0s4","show ip fib table 56340"]}]}
EOF

LY_ROUTE_RUNTIME_DEBS_DIR="$tmp/out" "$repo/scripts/build-runtime-debs.sh" vpp-apply >/dev/null
deb=$(find "$tmp/out" -maxdepth 1 -name 'ly-route-vpp-apply_*_*.deb' -print -quit)
test -n "$deb"
dpkg-deb -x "$deb" "$tmp/root"

: >"$tmp/vppctl.log"
PATH="$tmp/bin:$PATH" \
LY_ROUTE_TEST_VPPCTL_LOG="$tmp/vppctl.log" \
LY_ROUTE_VPP_COMMAND_MAP="$tmp/missing-command-map.json" \
LY_ROUTE_VPP_RECEIPT="$tmp/receipt.json" \
"$tmp/root/usr/lib/ly-route/vpp-apply" "$tmp/operations.json"

grep -q 'set ly-route pre-nat-route add id ' "$tmp/vppctl.log" || {
  echo 'WAN-group route did not install a native pre-NAT classifier' >&2
  cat "$tmp/vppctl.log" >&2
  exit 1
}
grep -q 'priority 150 source 192.168.77.0/24 destination 0.0.0.0/0 protocol any' "$tmp/vppctl.log" || {
  echo 'WAN-group pre-NAT rule does not carry the policy match' >&2
  cat "$tmp/vppctl.log" >&2
  exit 1
}
grep -q 'table 56340' "$tmp/vppctl.log" || {
  echo 'WAN-group pre-NAT rule does not select the route FIB' >&2
  exit 1
}
if grep -qE '^(set acl-plugin acl|abf policy add|abf attach ip4 policy) ' "$tmp/vppctl.log"; then
  echo 'native WAN-group policy replay must not create legacy ACL/ABF objects' >&2
  cat "$tmp/vppctl.log" >&2
  exit 1
fi
grep -q '"status": "applied"' "$tmp/receipt.json"
echo 'vpp-apply installs WAN-group policies through the native pre-NAT classifier'
