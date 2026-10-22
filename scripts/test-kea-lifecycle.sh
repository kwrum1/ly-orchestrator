#!/bin/sh
# Contract and behaviour tests for the Kea LAN readiness lifecycle.
#
# Static checks prove the factory scope stays on the LAN and the systemd/udev
# wiring remains bounded.  The netns checks exercise the real wait helper:
# a late lylan-* tap must be detected, a wrong interface must not satisfy the
# wait, and a missing configuration must fail fast.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
wait_script=$repo_root/packaging/rootfs-overlay/usr/lib/ly-route/kea-wait-lan.sh
dropin=$repo_root/packaging/rootfs-overlay/etc/systemd/system/kea-dhcp4-server.service.d/20-ly-route-lan-retry.conf
kea_config=$repo_root/packaging/rootfs-overlay/etc/kea/kea-dhcp4.conf
udev_rule=$repo_root/packaging/rootfs-overlay/etc/udev/rules.d/90-ly-route-kea-lan.rules

command -v ip >/dev/null
command -v python3 >/dev/null

sh -n "$wait_script"
sh -n "$repo_root/scripts/test-kea-lifecycle.sh"

python3 - "$kea_config" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    document = json.load(source)
dhcp = document["Dhcp4"]
interfaces = dhcp["interfaces-config"]["interfaces"]
assert interfaces, "factory Kea config has no interface"
assert all(name.startswith("lylan-") for name in interfaces), interfaces
subnets = dhcp["subnet4"]
assert [subnet["subnet"] for subnet in subnets] == ["192.168.88.0/24"], subnets
options = {item["name"]: item["data"] for item in subnets[0]["option-data"]}
assert options["routers"] == "192.168.88.1", options
assert options["domain-name-servers"] == "192.168.88.1", options
pools = [item["pool"] for item in subnets[0]["pools"]]
assert pools == ["192.168.88.100 - 192.168.88.199"], pools
raw = open(sys.argv[1], encoding="utf-8").read()
assert "10.0.2." not in raw, "factory Kea config must never use the management network"
PY

grep -q '^ExecStartPre=/usr/lib/ly-route/kea-wait-lan.sh$' "$dropin"
grep -q '^TimeoutStartSec=120$' "$dropin"
grep -q '^Restart=on-failure$' "$dropin"
grep -q '^RestartSec=3$' "$dropin"
grep -q '^StartLimitIntervalSec=600$' "$dropin"
grep -q '^StartLimitBurst=10$' "$dropin"
grep -q '^BindsTo=vpp.service$' "$dropin"
grep -q 'KERNEL=="lylan-\*"' "$udev_rule"
grep -q 'ENV{SYSTEMD_WANTS}+="kea-dhcp4-server.service"' "$udev_rule"

ns=ly-route-kea-wait-$$
tmpdir=$(mktemp -d)
cleanup() {
	ip netns del "$ns" 2>/dev/null || true
	rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM
ip netns add "$ns"

cat >"$tmpdir/late.json" <<'EOF'
{"Dhcp4":{"interfaces-config":{"interfaces":["lylan-test0"]},"subnet4":[{"subnet":"192.168.88.0/24","option-data":[{"name":"routers","data":"192.168.88.1"}]}]}}
EOF
cat >"$tmpdir/wrong.json" <<'EOF'
{"Dhcp4":{"interfaces-config":{"interfaces":["lylan-target1"]},"subnet4":[{"subnet":"192.168.88.0/24"}]}}
EOF
cat >"$tmpdir/unaddressed.json" <<'EOF'
{"Dhcp4":{"interfaces-config":{"interfaces":["lylan-test2"]},"subnet4":[{"subnet":"192.168.88.0/24"}]}}
EOF

# A tap created after ExecStartPre began must be picked up.
ip netns exec "$ns" "$wait_script" --config "$tmpdir/late.json" --timeout 8 --interval 1 &
waiter=$!
sleep 1
ip netns exec "$ns" ip link add lylan-test0 type dummy
ip netns exec "$ns" ip link set lylan-test0 up
ip netns exec "$ns" ip addr add 192.168.88.1/24 dev lylan-test0
if ! wait "$waiter"; then
	echo "wait helper did not detect a late LAN tap" >&2
	exit 1
fi

# A different lylan-* interface must not satisfy the wait for the configured
# interface, and the timeout must stay bounded.
ip netns exec "$ns" ip link add lylan-other0 type dummy
ip netns exec "$ns" ip link set lylan-other0 up
ip netns exec "$ns" ip addr add 192.168.99.1/24 dev lylan-other0
started=$(date +%s)
if ip netns exec "$ns" "$wait_script" --config "$tmpdir/wrong.json" --timeout 3 --interval 1; then
	echo "wait helper accepted the wrong LAN interface" >&2
	exit 1
fi
elapsed=$(( $(date +%s) - started ))
[ "$elapsed" -le 8 ] || { echo "wrong-interface timeout was not bounded: ${elapsed}s" >&2; exit 1; }

# The interface exists but has no routable IPv4 address yet.
ip netns exec "$ns" ip link add lylan-test2 type dummy
ip netns exec "$ns" ip link set lylan-test2 up
if ip netns exec "$ns" "$wait_script" --config "$tmpdir/unaddressed.json" --timeout 2 --interval 1; then
	echo "wait helper accepted an unaddressed LAN tap" >&2
	exit 1
fi
# Address not required: link readiness alone is enough for callers that opt out.
ip netns exec "$ns" "$wait_script" --config "$tmpdir/unaddressed.json" --timeout 2 --interval 1 --no-address

# Missing or invalid configuration is a fast configuration error (exit 2),
# not a long wait.
started=$(date +%s)
set +e
ip netns exec "$ns" "$wait_script" --config "$tmpdir/missing.json" --timeout 30 --interval 1
status=$?
set -e
elapsed=$(( $(date +%s) - started ))
[ "$status" -eq 2 ] || { echo "missing config exit status = $status, want 2" >&2; exit 1; }
[ "$elapsed" -le 5 ] || { echo "missing config was not rejected fast: ${elapsed}s" >&2; exit 1; }

printf 'Kea LAN readiness contract and netns behaviour passed\n'
