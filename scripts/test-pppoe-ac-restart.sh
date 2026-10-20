#!/bin/bash
# Fault-injection acceptance for AC restart without restarting VPP.
#
# Run on the router.  LY_ROUTE_AC_RESTART_COMMAND must restart the external AC
# (for example on the lab host: systemctl restart lab-pppoe-host).
set -eu

: "${LY_ROUTE_API_URL:=http://127.0.0.1:8080}"
: "${LY_ROUTE_PPPOE_PEER:=wan-eth1-ipv4}"
: "${LY_ROUTE_PPPOE_INTERFACE:=lyppp-8a258199}"
: "${LY_ROUTE_PPPOE_STATUS_DIR:=/run/ly-route/pppoe}"
: "${LY_ROUTE_AC_RESTART_COMMAND:?set LY_ROUTE_AC_RESTART_COMMAND to the AC restart command}"
: "${LY_ROUTE_RECOVERY_LIMIT_SECONDS:=60}"

status_json=/tmp/ly-route-pppoe-status.json
peer_status=$LY_ROUTE_PPPOE_STATUS_DIR/$LY_ROUTE_PPPOE_PEER.json

read_peer() {
  python3 - "$peer_status" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(" ")
    raise SystemExit
session = body.get("session") or {}
print(session.get("peer_address") or " ")
PY
}

read_api_state() {
  python3 - "$status_json" "$LY_ROUTE_PPPOE_PEER" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print("invalid  ")
    raise SystemExit
for item in body.get("items", []):
    if item.get("peer_id") == sys.argv[2]:
        print(item.get("state", ""), item.get("assigned_ipv4", ""))
        break
else:
    print("invalid  ")
PY
}

peer=$(read_peer)
vpp_started_before=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
echo "before: peer=$peer; restarting AC"
bash -c "$LY_ROUTE_AC_RESTART_COMMAND"
start=$(date +%s)
while :; do
  now=$(date +%s)
  elapsed=$((now - start))
  if [ "$elapsed" -gt "$LY_ROUTE_RECOVERY_LIMIT_SECONDS" ]; then
    echo "FAIL: WAN did not recover within ${LY_ROUTE_RECOVERY_LIMIT_SECONDS}s" >&2
    exit 1
  fi
  state=""
  local_address=""
  if curl -fsS --max-time 5 "$LY_ROUTE_API_URL/api/v1/gateway/pppoe/status" -o "$status_json" 2>/dev/null; then
    read -r state local_address < <(read_api_state)
  fi
  peer=$(read_peer)
  session_id=$(python3 - "$peer_status" <<'PY'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit
print((body.get("session") or {}).get("session_id", ""))
PY
)
  if [ "$state" = "connected" ] && [ "$peer" != " " ] && [ -n "$peer" ] &&
     vppctl ping "$peer" source "$LY_ROUTE_PPPOE_INTERFACE" repeat 3 2>/dev/null | grep -q "0% packet loss"; then
    vpp_started_after=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
    if [ "$vpp_started_before" != "$vpp_started_after" ]; then
      echo "FAIL: WAN recovered only after VPP restart" >&2
      exit 1
    fi
    echo "PASS: recovered in ${elapsed}s peer=$peer local=$local_address session=$session_id (VPP not restarted)"
    exit 0
  fi
  sleep 1
done
