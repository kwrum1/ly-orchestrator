#!/bin/bash
# Fault-injection acceptance for AC restart without restarting VPP.
#
# Run on the router.  LY_ROUTE_AC_RESTART_COMMAND must restart the external AC
# (for example: pkill -x pppoe-server; sleep 2; /root/work/lab-host-pppoe.sh).
set -eu

: "${LY_ROUTE_API_URL:=http://127.0.0.1:8080}"
: "${LY_ROUTE_PPPOE_PEER:=wan-eth1-ipv4}"
: "${LY_ROUTE_PPPOE_INTERFACE:=lyppp-8a258199}"
: "${LY_ROUTE_AC_RESTART_COMMAND:?set LY_ROUTE_AC_RESTART_COMMAND to the AC restart command}"
: "${LY_ROUTE_RECOVERY_LIMIT_SECONDS:=60}"

status_json=/tmp/ly-route-pppoe-status.json
vpp_started_before=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
curl -fsS --max-time 10 "$LY_ROUTE_API_URL/api/v1/gateway/pppoe/status" -o "$status_json"
before_session=$(python3 - "$status_json" <<'PY'
import json,sys
body=json.load(open(sys.argv[1]))
for item in body.get("items", []):
    if item.get("peer_id") == "wan-eth1-ipv4":
        print(item.get("assigned_ipv4", ""))
PY
)

echo "before: session $before_session; restarting AC"
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
  address=""
  if curl -fsS --max-time 5 "$LY_ROUTE_API_URL/api/v1/gateway/pppoe/status" -o "$status_json" 2>/dev/null; then
    read -r state address < <(python3 - "$status_json" <<'PY'
import json,sys
try:
    body=json.load(open(sys.argv[1]))
except Exception:
    print("invalid ")
    raise SystemExit
for item in body.get("items", []):
    if item.get("peer_id") == "wan-eth1-ipv4":
        print(item.get("state", ""), item.get("assigned_ipv4", ""))
        break
else:
    print("invalid ")
PY
)
  fi
  if [ "$state" = "connected" ] && vppctl ping "$address" source "$LY_ROUTE_PPPOE_INTERFACE" repeat 3 2>/dev/null | grep -q "0% packet loss"; then
    vpp_started_after=$(systemctl show vpp.service -p ActiveEnterTimestampMonotonic --value)
    if [ "$vpp_started_before" != "$vpp_started_after" ]; then
      echo "FAIL: WAN recovered only after VPP restart" >&2
      exit 1
    fi
    echo "PASS: recovered in ${elapsed}s session $address (VPP not restarted)"
    exit 0
  fi
  sleep 1
done
