#!/bin/bash
# Orchestrator transparent datapath functional test (AF_PACKET acceptance mode).
#
# Fixture: one peer machine with two network namespaces, one per isolated data
# leg, and an Orchestrator configured with the transparent topology:
#   WAN boundary  = the peer's "wan" leg (server side)
#   LAN boundary  = the peer's "lan" leg (client side)
# Policy rules are canonicalised client->server, where LAN ingress is the
# request direction.
#
# Required environment:
#   ORCH_SSH        ssh target of the appliance (e.g. root@10.1.18.203)
#   PEER_SSH        ssh target of the peer (e.g. root@10.1.18.204)
#   SSH_KEY         private key accepted by both
#   PEER_WAN_IF / PEER_LAN_IF   data NIC names in the peer
#   CLIENT_IP / SERVER_IP       addresses (same subnet; default 10.99.0.3/.2)
#   ORCH_API        API base (default https://<ORCH_SSH host>)
#   ORCH_PASS_FILE  admin password file
#   ORCH_OUT        evidence directory (default ./evidence-<utc>)
set -uo pipefail

ORCH_SSH=${ORCH_SSH:?}
PEER_SSH=${PEER_SSH:?}
SSH_KEY=${SSH_KEY:?}
PEER_WAN_IF=${PEER_WAN_IF:-ens224}
PEER_LAN_IF=${PEER_LAN_IF:-ens256}
CLIENT_IP=${CLIENT_IP:-10.99.0.3}
SERVER_IP=${SERVER_IP:-10.99.0.2}
SUBNET=${SUBNET:-10.99.0.0/24}
ORCH_API=${ORCH_API:-https://${ORCH_SSH#*@}}
ORCH_PASS_FILE=${ORCH_PASS_FILE:?}
ORCH_OUT=${ORCH_OUT:-./evidence-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$ORCH_OUT"

SSHO=(-i "$SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8)
peer() { ssh "${SSHO[@]}" "$PEER_SSH" "$@"; }
orch() { ssh "${SSHO[@]}" "$ORCH_SSH" "$@"; }
step() { echo; echo "##### $* #####"; }

login() {
  local tmp; tmp=$(mktemp)
  printf '{"username":"admin","password":"%s"}' "$(cat "$ORCH_PASS_FILE")" > "$tmp"
  curl -sk -c "$ORCH_OUT/cookies" -H 'Content-Type: application/json' -d @"$tmp" "$ORCH_API/api/v1/auth/login" >/dev/null
  rm -f "$tmp"
}

step "peer network namespaces (idempotent)"
peer "bash -s" <<EOF
set -e
sysctl -qw net.ipv6.conf.all.disable_ipv6=1
for ns in wan lan; do ip netns list | grep -q "^\$ns " || ip netns add \$ns; done
for pair in "wan $PEER_WAN_IF $SERVER_IP" "lan $PEER_LAN_IF $CLIENT_IP"; do
  set -- \$pair; ns=\$1; ifc=\$2; addr=\$3
  if ip link show dev \$ifc >/dev/null 2>&1; then
    ip link set dev \$ifc down || true
    sysctl -qw net.ipv6.conf.\$ifc.disable_ipv6=1 || true
    ip link set dev \$ifc netns \$ns
  fi
  ip netns exec \$ns ip link set lo up
  ip netns exec \$ns ip link set \$ifc up
  ip netns exec \$ns ip addr flush dev \$ifc 2>/dev/null || true
  ip netns exec \$ns ip addr add \$addr/24 dev \$ifc
done
ip netns exec wan ip -br addr; ip netns exec lan ip -br addr
EOF

step "listeners (server 8080/9000 on WAN, reverse server 8082 on LAN)"
peer "bash -s" <<'EOF'
pkill -f "http.server" 2>/dev/null || true
mkdir -p /tmp/www; head -c 2097152 /dev/urandom > /tmp/www/blob.bin
echo hello-orchestrator > /tmp/www/index.html
setsid ip netns exec wan python3 -m http.server 8080 --bind 10.99.0.2 --directory /tmp/www </dev/null >/tmp/srv8080.log 2>&1 &
setsid ip netns exec wan python3 -m http.server 9000 --bind 10.99.0.2 --directory /tmp/www </dev/null >/tmp/srv9000.log 2>&1 &
setsid ip netns exec lan python3 -m http.server 8082 --bind 10.99.0.3 --directory /tmp/www </dev/null >/tmp/srv8082.log 2>&1 &
sleep 2
ip netns exec wan ss -ltn | grep -E ':8080|:9000'
ip netns exec lan ss -ltn | grep ':8082'
EOF

step "bidirectional ICMP"
{
  echo '--- LAN client -> WAN server'; peer "ip netns exec lan ping -c 5 -W 2 $SERVER_IP"
  echo '--- WAN server -> LAN client'; peer "ip netns exec wan ping -c 5 -W 2 $CLIENT_IP"
} 2>&1 | tee "$ORCH_OUT/icmp.txt"

step "VPP state before TCP"
orch 'vppctl show ly-route orchestrator | sed -n "1,6p"; vppctl show interface' | tee "$ORCH_OUT/vpp-before.txt"

step "policy positive: TCP 8080 (default direct)"
peer "ip netns exec lan curl -sS -m 20 -o /dev/null -w 'http_code=%{http_code} bytes=%{size_download} time=%{time_total}\n' http://$SERVER_IP:8080/blob.bin" | tee "$ORCH_OUT/tcp-8080.txt"

step "policy negative: TCP 9000 (drop rule) while the listener is up"
peer "ip netns exec lan curl -sS -m 6 -o /dev/null -w 'http_code=%{http_code} time=%{time_total}\n' http://$SERVER_IP:9000/index.html; echo curl_rc=\$?" | tee "$ORCH_OUT/tcp-9000.txt"

step "reverse direction: WAN client -> LAN server 8082"
peer "ip netns exec wan curl -sS -m 15 -o /dev/null -w 'http_code=%{http_code} bytes=%{size_download} time=%{time_total}\n' http://$CLIENT_IP:8082/index.html" | tee "$ORCH_OUT/reverse-8082.txt"

step "VPP counters / plugin state"
orch 'vppctl show interface; echo ---; vppctl show ly-route orchestrator; echo ---; vppctl show errors' | tee "$ORCH_OUT/vpp-after.txt"

step "informational throughput (2 MiB, not a performance claim)"
peer "ip netns exec lan curl -sS -m 180 -o /dev/null -w 'bytes=%{size_download} time=%{time_total} speed_Bps=%{speed_download}\n' http://$SERVER_IP:8080/blob.bin" | tee "$ORCH_OUT/throughput.txt"

login
step "runtime status"
curl -sk -b "$ORCH_OUT/cookies" "$ORCH_API/api/v1/runtime/status" | tee "$ORCH_OUT/runtime-status.json" \
  | python3 -c 'import json,sys;d=json.load(sys.stdin);print("status",d["status"],"last_apply",d["last_apply"]["status"]);print([(c["name"],c["state"],c.get("available")) for c in d["components"]])'

echo; echo "evidence: $ORCH_OUT"
