#!/bin/bash
# Fail-closed semantics for the Orchestrator transparent runtime.
#
# Breaks the dataplane by stopping VPP, then proves:
#   * POST /api/v1/runtime/apply reports apply_failed / runtime_state degraded
#     instead of a handler-level dataplane_locked,
#   * /api/v1/runtime/status is degraded and does not claim a committed
#     generation while the dataplane is down,
#   * no transparent transaction journal remains (no generation committed),
#   * traffic does not leak through the appliance while VPP is down,
#   * /api/v1/config/apply keeps the non-fatal capability probe (no 423
#     dataplane_locked handler lock),
#   * after restoring VPP the same generation returns and apply commits again.
#
# Required environment: ORCH_SSH, SSH_KEY, ORCH_PASS_FILE, ORCH_OUT.
# Optional: ORCH_API (default https://<host>), PEER_SSH, CLIENT_IP, SERVER_IP.
set -uo pipefail

ORCH_SSH=${ORCH_SSH:?}
SSH_KEY=${SSH_KEY:?}
ORCH_PASS_FILE=${ORCH_PASS_FILE:?}
ORCH_OUT=${ORCH_OUT:-./evidence-$(date -u +%Y%m%dT%H%M%SZ)}
ORCH_API=${ORCH_API:-https://${ORCH_SSH#*@}}
PEER_SSH=${PEER_SSH:-}
CLIENT_IP=${CLIENT_IP:-10.99.0.3}
SERVER_IP=${SERVER_IP:-10.99.0.2}
mkdir -p "$ORCH_OUT"

SSHO=(-i "$SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8)
orch() { ssh "${SSHO[@]}" "$ORCH_SSH" "$@"; }
peer() { ssh "${SSHO[@]}" "$PEER_SSH" "$@"; }
step() { echo; echo "##### $* #####"; }

login() {
  local tmp; tmp=$(mktemp)
  printf '{"username":"admin","password":"%s"}' "$(cat "$ORCH_PASS_FILE")" > "$tmp"
  curl -sk -c "$ORCH_OUT/cookies" -H 'Content-Type: application/json' -d @"$tmp" "$ORCH_API/api/v1/auth/login" >/dev/null
  rm -f "$tmp"
}
status() {
  curl -sk -b "$ORCH_OUT/cookies" -m 60 "$ORCH_API/api/v1/runtime/status" | tee "$1"
}
status_field() {
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("status",""), (d.get("last_apply") or {}).get("status",""))'
}
generation() {
  orch 'vppctl show ly-route orchestrator 2>/dev/null | sed -n "s/^generation //p"' | tr -d '\r' | head -1
}
wait_status_running() {
  local file=$1 out st
  for _ in $(seq 1 45); do
    out=$(curl -sk -b "$ORCH_OUT/cookies" -m 30 "$ORCH_API/api/v1/runtime/status" 2>/dev/null)
    st=$(printf '%s' "$out" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")' 2>/dev/null)
    if [ "$st" = "running" ]; then printf '%s' "$out" > "$file"; echo "running"; return 0; fi
    sleep 2
  done
  printf '%s' "$out" > "$file"; echo "${st:-timeout}"; return 1
}

login
step "pre-failure generation and status"
status "$ORCH_OUT/fc-status-before.json" | status_field
GEN_BEFORE=$(generation)
echo "generation_before=$GEN_BEFORE" | tee "$ORCH_OUT/fc-generation-before.txt"
orch 'vppctl show ly-route orchestrator' | tee "$ORCH_OUT/fc-vpp-before.txt"

step "break dataplane: stop VPP"
orch 'systemctl stop vpp; sleep 2; systemctl is-active vpp; vppctl show version 2>&1 | head -2' | tee "$ORCH_OUT/fc-vpp-stopped.txt"
sleep 3

step "POST /api/v1/runtime/apply with the dataplane down"
curl -sk -b "$ORCH_OUT/cookies" -m 120 -o "$ORCH_OUT/fc-apply-broken.json" -w 'http_code=%{http_code}\n' \
  -X POST -H 'Content-Type: application/json' -d '{}' "$ORCH_API/api/v1/runtime/apply" | tee "$ORCH_OUT/fc-apply-broken.http"
cat "$ORCH_OUT/fc-apply-broken.json" | tee "$ORCH_OUT/fc-apply-broken.pretty.json"
python3 - "$ORCH_OUT/fc-apply-broken.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("apply_status=%s runtime_state=%s" % (d.get("status"), d.get("runtime_state")))
print("reason=%s" % d.get("reason",""))
PY

step "runtime status while broken"
status "$ORCH_OUT/fc-status-broken.json" | tee "$ORCH_OUT/fc-status-broken.pretty.json"
python3 - "$ORCH_OUT/fc-status-broken.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
last=d.get("last_apply") or {}
print("status=%s last_apply_status=%s last_apply_state=%s" % (d.get("status"), last.get("status"), last.get("runtime_state")))
for c in d.get("components") or []:
    print("component %s state=%s available=%s reason=%s" % (c.get("name"), c.get("state"), c.get("available"), c.get("reason","")))
PY

step "no generation committed: journal and VPP readback"
orch 'set +e; ls -l /var/lib/ly-route/orchestrator/transparent-transaction.json; echo journal_ls_rc=$?; vppctl show ly-route orchestrator 2>&1 | head -3; echo vpp_readback_rc=$?' | tee "$ORCH_OUT/fc-no-generation.txt"

step "no traffic leaking direct while VPP is down"
if [ -n "$PEER_SSH" ]; then
  peer "ip netns exec lan ping -c 3 -W 2 $SERVER_IP; echo ping_rc=\$?" | tee "$ORCH_OUT/fc-ping-broken.txt"
  peer "ip netns exec lan curl -sS -m 5 -o /dev/null -w 'http_code=%{http_code} time=%{time_total}\n' http://$SERVER_IP:8080/index.html; echo curl_rc=\$?" | tee "$ORCH_OUT/fc-tcp-broken.txt"
else
  echo "PEER_SSH not set: leak check skipped" | tee "$ORCH_OUT/fc-ping-broken.txt"
fi
orch 'ip -br link; echo ---; ip -4 route; echo ---; sysctl net.ipv4.ip_forward; echo ---; bridge link 2>/dev/null || true' | tee "$ORCH_OUT/fc-host-broken.txt"

step "/api/v1/config/apply capability probe is non-fatal while broken"
curl -sk -b "$ORCH_OUT/cookies" -m 120 -o "$ORCH_OUT/fc-config-apply-broken.json" -w 'http_code=%{http_code}\n' \
  -X POST -H 'Content-Type: application/json' -d '{}' "$ORCH_API/api/v1/config/apply" | tee "$ORCH_OUT/fc-config-apply-broken.http"
cat "$ORCH_OUT/fc-config-apply-broken.json" | tee "$ORCH_OUT/fc-config-apply-broken.pretty.json"

step "restore VPP"
orch 'systemctl start vpp' | tee "$ORCH_OUT/fc-vpp-start.txt"
for _ in $(seq 1 40); do
  [ "$(orch 'systemctl is-active vpp')" = "active" ] && break
  sleep 2
done
login
STATE=$(wait_status_running "$ORCH_OUT/fc-status-restored.json")
echo "status_after_restore=$STATE" | tee "$ORCH_OUT/fc-restored-state.txt"
GEN_AFTER=$(generation)
echo "generation_after=$GEN_AFTER" | tee "$ORCH_OUT/fc-generation-after.txt"
if [ -n "$GEN_BEFORE" ] && [ "$GEN_BEFORE" = "$GEN_AFTER" ]; then
  echo "generation_unchanged=true" | tee "$ORCH_OUT/fc-generation-verdict.txt"
else
  echo "generation_unchanged=false before=$GEN_BEFORE after=$GEN_AFTER" | tee "$ORCH_OUT/fc-generation-verdict.txt"
fi
orch 'vppctl show ly-route orchestrator | head -8; echo ---; vppctl show interface' | tee "$ORCH_OUT/fc-vpp-restored.txt"

step "apply commits again after restore"
curl -sk -b "$ORCH_OUT/cookies" -m 180 -o "$ORCH_OUT/fc-apply-restored.json" -w 'http_code=%{http_code}\n' \
  -X POST -H 'Content-Type: application/json' -d '{}' "$ORCH_API/api/v1/runtime/apply" | tee "$ORCH_OUT/fc-apply-restored.http"
cat "$ORCH_OUT/fc-apply-restored.json" | tee "$ORCH_OUT/fc-apply-restored.pretty.json"

step "forwarding restored"
if [ -n "$PEER_SSH" ]; then
  peer "ip netns exec lan ping -c 3 -W 2 $SERVER_IP; echo ping_rc=\$?" | tee "$ORCH_OUT/fc-ping-restored.txt"
  peer "ip netns exec lan curl -sS -m 20 -o /dev/null -w 'http_code=%{http_code} bytes=%{size_download}\n' http://$SERVER_IP:8080/index.html" | tee "$ORCH_OUT/fc-tcp-restored.txt"
fi

step "final status"
status "$ORCH_OUT/fc-status-final.json" | status_field

echo; echo "evidence: $ORCH_OUT"

