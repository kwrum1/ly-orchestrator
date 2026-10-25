#!/bin/bash
# Restart persistence for the Orchestrator transparent generation:
#   commit -> VPP restart -> re-readback, then -> control-api restart -> re-readback.
#
# Required environment: ORCH_SSH, PEER_SSH, SSH_KEY, ORCH_API, ORCH_PASS_FILE,
# ORCH_OUT (same as datapath-test.sh). CLIENT_IP/SERVER_IP default to the
# datapath fixture addresses.
set -uo pipefail

ORCH_SSH=${ORCH_SSH:?}
PEER_SSH=${PEER_SSH:?}
SSH_KEY=${SSH_KEY:?}
ORCH_API=${ORCH_API:-https://${ORCH_SSH#*@}}
ORCH_PASS_FILE=${ORCH_PASS_FILE:?}
ORCH_OUT=${ORCH_OUT:-./evidence-$(date -u +%Y%m%dT%H%M%SZ)}
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
  curl -sk -b "$ORCH_OUT/cookies" "$ORCH_API/api/v1/runtime/status" | tee "$1" \
    | python3 -c 'import json,sys;d=json.load(sys.stdin);print("status",d["status"],"last_apply",d["last_apply"]["status"]);print([(c["name"],c["state"],c.get("available")) for c in d["components"]])'
}

login
step "before"
status "$ORCH_OUT/status-before.json"
orch 'vppctl show ly-route orchestrator | sed -n "1,5p"'

step "restart VPP"
orch 'systemctl restart vpp; sleep 3; vppctl show ly-route orchestrator | head -2'
for _ in $(seq 1 30); do
  [ "$(orch 'vppctl show ly-route orchestrator | grep -c "^state running"')" = "1" ] && break
  sleep 2
done
orch 'vppctl show interface; echo ---; vppctl show ly-route orchestrator | head -6' | tee "$ORCH_OUT/vpp-after-restart.txt"
peer "ip netns exec lan ping -c 3 -W 2 $SERVER_IP; echo rc=\$?" | tee "$ORCH_OUT/ping-after-vpp-restart.txt"
status "$ORCH_OUT/status-after-vpp.json"

step "restart control API"
orch 'systemctl restart ly-route-control-api; sleep 5; systemctl is-active ly-route-control-api'
login
orch 'vppctl show ly-route orchestrator | head -6' | tee "$ORCH_OUT/vpp-after-api-restart.txt"
status "$ORCH_OUT/status-after-api.json"
peer "ip netns exec lan ping -c 3 -W 2 $SERVER_IP; echo rc=\$?" | tee "$ORCH_OUT/ping-after-api-restart.txt"
