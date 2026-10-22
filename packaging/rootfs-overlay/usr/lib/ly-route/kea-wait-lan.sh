#!/bin/sh
# Wait until Kea's configured DHCP interface is ready.
#
# Kea fails hard when its interface does not exist ("interface 'lylan-eth2'
# doesn't exist in the system").  On appliance boot the VPP LAN tap can appear
# after Kea's unit starts, so a plain After= dependency on vpp-apply is not
# enough.  This helper is invoked from ExecStartPre and waits for:
#   1. the exact interface named in /etc/kea/kea-dhcp4.conf (not any lylan-*),
#   2. the interface to be administratively up, and
#   3. a non-link-local IPv4 address to be installed (the LAN gateway/DNS
#      address clients receive).
#
# The wait is strictly bounded.  When it times out the unit fails and the
# drop-in restart policy (plus the udev add rule) retries, instead of blocking
# boot forever.
set -eu

config=/etc/kea/kea-dhcp4.conf
timeout=45
interval=1
interface=
require_address=${LY_ROUTE_KEA_WAIT_ADDRESS:-yes}

usage() {
	cat >&2 <<'EOF'
usage: kea-wait-lan.sh [--config PATH] [--interface NAME] [--timeout SECONDS]
                       [--interval SECONDS] [--no-address]
EOF
	exit 2
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--config)
		[ "$#" -ge 2 ] || usage
		config=$2
		shift 2
		;;
	--interface)
		[ "$#" -ge 2 ] || usage
		interface=$2
		shift 2
		;;
	--timeout)
		[ "$#" -ge 2 ] || usage
		timeout=$2
		shift 2
		;;
	--interval)
		[ "$#" -ge 2 ] || usage
		interval=$2
		shift 2
		;;
	--no-address)
		require_address=no
		shift
		;;
	-h | --help)
		usage
		;;
	*)
		echo "kea-wait-lan: unknown argument: $1" >&2
		usage
		;;
	esac
done

case "$timeout" in
'' | *[!0-9]*)
	echo "kea-wait-lan: timeout must be a positive integer" >&2
	exit 2
	;;
esac
case "$interval" in
'' | *[!0-9]*)
	echo "kea-wait-lan: interval must be a positive integer" >&2
	exit 2
	;;
esac
[ "$timeout" -gt 0 ] || { echo "kea-wait-lan: timeout must be greater than zero" >&2; exit 2; }
[ "$interval" -gt 0 ] || { echo "kea-wait-lan: interval must be greater than zero" >&2; exit 2; }

if [ -z "$interface" ]; then
	if [ ! -r "$config" ]; then
		echo "kea-wait-lan: cannot read Kea configuration: $config" >&2
		exit 2
	fi
	if ! interface=$(python3 - "$config" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        document = json.load(source)
    interfaces = document["Dhcp4"]["interfaces-config"]["interfaces"]
    if not isinstance(interfaces, list) or len(interfaces) != 1:
        raise ValueError("exactly one DHCP interface is required")
    name = str(interfaces[0]).strip()
    if not name:
        raise ValueError("DHCP interface is empty")
    print(name)
except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
    print(f"kea-wait-lan: invalid Kea interface configuration: {error}", file=sys.stderr)
    raise SystemExit(2)
PY
	); then
		exit 2
	fi
fi

case "$interface" in
'' | *[!A-Za-z0-9_.:-]*)
	echo "kea-wait-lan: invalid interface name: $interface" >&2
	exit 2
	;;
esac

# Reject anything that is not an administratively up interface.
interface_up() {
	ip -o link show dev "$interface" 2>/dev/null | grep -Eq '<[^>]*\bUP\b'
}

# Require a routable IPv4 address.  Link-local 169.254/16 addresses appear
# before the VPP LAN address and must not count as LAN readiness.
interface_addressed() {
	ip -4 -o addr show dev "$interface" 2>/dev/null |
		awk '$4 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\// && $4 !~ /^169\.254\./ { found = 1 } END { exit !found }'
}

# Use a wall-clock deadline instead of a loop counter: on slow virtualized
# hardware a single `ip` invocation can take seconds, and 45 counted attempts
# would otherwise exceed the unit's TimeoutStartSec.
deadline=$(($(date +%s) + timeout))
while :; do
	if interface_up; then
		if [ "$require_address" = no ] || interface_addressed; then
			exit 0
		fi
	fi
	now=$(date +%s)
	if [ "$now" -ge "$deadline" ]; then
		break
	fi
	pause=$interval
	remaining=$((deadline - now))
	if [ "$pause" -gt "$remaining" ]; then
		pause=$remaining
	fi
	sleep "$pause"
done

echo "kea-wait-lan: timed out after ${timeout}s waiting for interface $interface (address required: $require_address)" >&2
exit 1
