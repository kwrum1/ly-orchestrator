#!/usr/bin/env bash
# Clean-state regression for the 209 apply-order fix.
#
# Run on a pure 209 appliance (or against a lab instance reachable over HTTPS):
#   LY_ROUTE_GATEWAY_PASSWORD=... \
#   LY_ROUTE_DESIRED_PACKAGE=/root/work/lab-desired-209.json \
#   bash scripts/test-gateway-apply-order-209.sh
#
# Optional lab credentials that a desired package intentionally excludes:
#   LY_ROUTE_LAB_PPPOE_PASSWORD   PATCHed on the first PPPoE WAN link
#   LY_ROUTE_LAB_PROXY_SECRET     PATCHed on the first proxy node
set -euo pipefail

base=${LY_ROUTE_GATEWAY_URL:-https://127.0.0.1}
user=${LY_ROUTE_GATEWAY_USER:-admin}
password=${LY_ROUTE_GATEWAY_PASSWORD:?LY_ROUTE_GATEWAY_PASSWORD is required}
new_password=${LY_ROUTE_GATEWAY_NEW_PASSWORD:-ApplyFix2026!}
package=${LY_ROUTE_DESIRED_PACKAGE:-/root/work/lab-desired-209.json}
[ -r "$package" ] || { echo "desired package $package is not readable" >&2; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
jar=$tmp/cookies
curl_json() {
  local method=$1 path=$2 body=${3:-}
  local args=(-k -sS -f -b "$jar" -c "$jar" -X "$method" -H 'Accept: application/json')
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$body")
  fi
  curl "${args[@]}" "$base$path"
}

login() {
  curl_json POST /api/v1/auth/login "$(python3 - "$user" "$password" <<'PY'
import json,sys
print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))
PY
)"
}

login >"$tmp/login.json"
if python3 -c 'import json,sys; raise SystemExit(0 if json.load(open(sys.argv[1])).get("password_change_required") else 1)' "$tmp/login.json"; then
  curl_json POST /api/v1/auth/change-password "$(python3 - "$password" "$new_password" <<'PY'
import json,sys
print(json.dumps({"current_password":sys.argv[1],"new_password":sys.argv[2]}))
PY
)" >/dev/null
  password=$new_password
  login >/dev/null
fi

python3 - "$package" "$tmp/import.json" <<'PY'
import json,sys
package=json.load(open(sys.argv[1],encoding="utf-8"))
json.dump({"confirm":False,"dry_run":True,"package_manifest":package["package_manifest"],"payload":package["payload"]},open(sys.argv[2],"w",encoding="utf-8"))
PY
dry=$(curl_json POST /api/v1/config/import "@$tmp/import.json")
python3 -c 'import json,sys; d=json.loads(sys.argv[1]); raise SystemExit(0 if d.get("status")=="dry_run" and d.get("safe_to_apply") else 1)' "$dry" \
  || { echo "config import dry-run rejected: $dry" >&2; exit 1; }

python3 - "$package" "$dry" "$tmp/confirm.json" <<'PY'
import json,sys
package=json.load(open(sys.argv[1],encoding="utf-8"))
dry=json.loads(sys.argv[2])
json.dump({"confirm":True,"dry_run":False,"confirmation_token":dry["confirmation_token"],"confirmation_actor":dry.get("confirmation_actor",""),"confirmation_expires_at":dry.get("confirmation_expires_at",""),"package_hash":dry["package_hash"],"diff_hash":dry["diff_hash"],"package_manifest":package["package_manifest"],"payload":package["payload"]},open(sys.argv[3],"w",encoding="utf-8"))
PY
curl_json POST /api/v1/config/import "@$tmp/confirm.json" >/dev/null

seed_credential() {
  local link=$1 value=$2 path
  path=/api/v1/gateway/wan-links/$link
  curl_json PATCH "$path" "$(python3 - "$value" <<'PY'
import json,sys
print(json.dumps({"pppoe_password":sys.argv[1]}))
PY
)" >/dev/null
}
if [ -n "${LY_ROUTE_LAB_PPPOE_PASSWORD:-}" ]; then
  link=$(curl_json GET /api/v1/gateway/wan-links | python3 -c 'import json,sys; items=json.load(sys.stdin).get("items",[]); print(next((i["id"] for i in items if (i.get("type") or i.get("wan_type"))=="pppoe"),""))')
  [ -z "$link" ] || seed_credential "$link" "$LY_ROUTE_LAB_PPPOE_PASSWORD"
fi
if [ -n "${LY_ROUTE_LAB_PROXY_SECRET:-}" ]; then
  node=$(curl_json GET /api/v1/proxy/nodes | python3 -c 'import json,sys; items=json.load(sys.stdin).get("items",[]); print(items[0]["id"] if items else "")')
  [ -z "$node" ] || curl_json PATCH "/api/v1/proxy/nodes/$node" "$(python3 - "$LY_ROUTE_LAB_PROXY_SECRET" <<'PY'
import json,sys
print(json.dumps({"secret":sys.argv[1]}))
PY
)" >/dev/null
fi

echo "== apply =="
apply=$(curl_json POST /api/v1/runtime/apply '{}')
status=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d.get("status",""))' "$apply")
echo "apply status: $status"
python3 - "$apply" <<'PY'
import json,sys
d=json.loads(sys.argv[1])
if d.get("status") not in ("committed","already_applied","degraded"):
    raise SystemExit("apply did not commit: %s" % d.get("reason",d.get("status")))
if d.get("runtime_state") == "degraded" and d.get("status") == "apply_failed":
    raise SystemExit("apply failed: %s" % d.get("reason"))
PY

python3 - "$(curl_json GET /api/v1/gateway/pppoe/status)" <<'PY'
import json,sys
d=json.loads(sys.argv[1])
items=d.get("items") or []
if not items:
    raise SystemExit("no PPPoE peers reported")
for item in items:
    print("pppoe", item.get("peer_id"), item.get("state"), item.get("assigned_ipv4"), item.get("vpp_route_handoff"))
    if item.get("state") != "connected":
        raise SystemExit("PPPoE peer %s is %s" % (item.get("peer_id"), item.get("state")))
PY

vppctl=${LY_ROUTE_VPPCTL:-vppctl}
peer=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d["items"][0].get("assigned_ipv4",""))' "$(curl_json GET /api/v1/gateway/pppoe/status)")
if [ -n "$peer" ]; then
  echo "== ping $peer =="
  "$vppctl" ping "$peer" repeat 3 interval 0.2 | tee "$tmp/ping.txt"
  grep -q "0% packet loss" "$tmp/ping.txt" || { echo "PPPoE peer is not reachable" >&2; exit 1; }
fi

echo "== repeat apply (idempotency) =="
repeat=$(curl_json POST /api/v1/runtime/apply '{}')
python3 - "$repeat" <<'PY'
import json,sys
d=json.loads(sys.argv[1])
print("repeat status:", d.get("status"), d.get("runtime_state"))
if d.get("status") not in ("committed","already_applied"):
    raise SystemExit("repeat apply was not idempotent: %s" % d.get("reason",d.get("status")))
PY
echo "PASS: clean import + apply committed, PPPoE connected/reachable, repeat apply idempotent"
