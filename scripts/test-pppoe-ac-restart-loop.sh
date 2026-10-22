#!/bin/bash
# Five-cycle AC-side LCP termination regression.
#
# Run on the router.  LY_ROUTE_AC_RESTART_COMMAND must terminate/restart the
# external AC session (for example a remote pppd kill is wrapped by the lab
# runner).  Each cycle asserts:
#   - VPP did not restart (monotonic start timestamp + NRestarts)
#   - WAN recovered to connected and peer ping has 0% loss within 60s
#   - the WAN-self port map follows the new PPPoE address
set -eu
: "${LY_ROUTE_API_URL:=http://127.0.0.1:8080}"
: "${LY_ROUTE_PPPOE_PEER:=wan-eth1-ipv4}"
: "${LY_ROUTE_PPPOE_INTERFACE:=lyppp-8a258199}"
: "${LY_ROUTE_PPPOE_STATUS_DIR:=/run/ly-route/pppoe}"
: "${LY_ROUTE_AC_RESTART_COMMAND:?set LY_ROUTE_AC_RESTART_COMMAND}"
: "${LY_ROUTE_AC_RESTART_ITERATIONS:=5}"
: "${LY_ROUTE_RECOVERY_LIMIT_SECONDS:=60}"
: "${LY_ROUTE_PORT_MAP_CHECK:=1}"
: "${LY_ROUTE_PORT_MAP_EXTERNAL_PORT:=18080}"

peer_file=$LY_ROUTE_PPPOE_STATUS_DIR/$LY_ROUTE_PPPOE_PEER.json
status_json=$(mktemp)
trap 'rm -f "$status_json"' EXIT

read_state_local() {
  python3 - "$status_json" "$LY_ROUTE_PPPOE_PEER" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print("invalid "); raise SystemExit
for item in body.get("items", []):
    if item.get("peer_id") == sys.argv[2]:
        print(item.get("state", ""), item.get("assigned_ipv4", ""))
        break
else:
    print("invalid ")
PY
}

read_peer() {
  python3 - "$peer_file" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(" "); raise SystemExit
print((body.get("session") or {}).get("peer_address") or " ")
PY
}

before_started=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
before_restarts=$(systemctl show vpp.service -p NRestarts --value)
for iteration in $(seq 1 "$LY_ROUTE_AC_RESTART_ITERATIONS"); do
  echo "=== AC termination cycle $iteration/$LY_ROUTE_AC_RESTART_ITERATIONS ==="
  bash -c "$LY_ROUTE_AC_RESTART_COMMAND"
  start=$(date +%s)
  while :; do
    now=$(date +%s)
    elapsed=$((now - start))
    if [ "$elapsed" -gt "$LY_ROUTE_RECOVERY_LIMIT_SECONDS" ]; then
      echo "FAIL: WAN did not recover within ${LY_ROUTE_RECOVERY_LIMIT_SECONDS}s in cycle $iteration" >&2
      exit 1
    fi
    state=""; local=""
    if curl -fsS --max-time 5 "$LY_ROUTE_API_URL/api/v1/gateway/pppoe/status" -o "$status_json" 2>/dev/null; then
      read -r state local < <(read_state_local)
    fi
    peer=$(read_peer)
    if [ "$state" = "connected" ] && [ "$peer" != " " ] && [ -n "$peer" ]; then
      if vppctl ping "$peer" source "$LY_ROUTE_PPPOE_INTERFACE" repeat 3 2>/dev/null | grep -q '0% packet loss'; then
        after_started=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
        after_restarts=$(systemctl show vpp.service -p NRestarts --value)
        [ "$before_started" = "$after_started" ] || { echo "FAIL: VPP restarted in cycle $iteration" >&2; exit 1; }
        [ "$before_restarts" = "$after_restarts" ] || { echo "FAIL: VPP NRestarts changed in cycle $iteration" >&2; exit 1; }
        if [ "$LY_ROUTE_PORT_MAP_CHECK" = "1" ]; then
          line=$(vppctl show nat44 static mappings 2>/dev/null | grep "external ${local}:${LY_ROUTE_PORT_MAP_EXTERNAL_PORT}" || true)
          [ -n "$line" ] || { echo "FAIL: port map did not follow WAN ${local} in cycle $iteration" >&2; exit 1; }
          echo "portmap: $line"
        fi
        echo "PASS cycle $iteration: recovered in ${elapsed}s state=$state local=$local peer=$peer"
        break
      fi
    fi
    sleep 1
  done
done
echo "PASS: ${LY_ROUTE_AC_RESTART_ITERATIONS}/${LY_ROUTE_AC_RESTART_ITERATIONS} AC LCP termination cycles; VPP never restarted"
