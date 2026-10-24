#!/bin/sh
# Independent netns acceptance for the fixed proxy failure page.
#
# Renders the production capture plan, then proves inside throwaway network
# namespaces:
#   1. normal mode: TPROXY captures selected traffic and the upstream receives
#      the proxied request,
#   2. failure mode (helper enter): TCP/80 is answered locally with the
#      packaged failure page, TCP/443 is reset, DNS/53 stays direct, every
#      other captured TCP/UDP packet is dropped, and the upstream receives
#      zero bytes,
#   3. recovery (re-install capture, then helper leave): proxying works again.
#
# Requires root, ip, nft, go, python3. nginx is used when available, otherwise
# python's static file server serves the same packaged page. The Xray binary is
# used when LY_ROUTE_XRAY_BINARY (or `xray` in PATH) is available; otherwise a
# transparent TCP relay stands in for the proxy engine. It never touches the
# host firewall or the lab.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BACKEND="$ROOT/backend"
HELPER="$ROOT/packaging/rootfs-overlay/usr/lib/ly-route/proxy-failure"
PAGE="$ROOT/packaging/rootfs-overlay/usr/share/ly-route/proxy-failure/index.html"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ly-proxy-failure.XXXXXX")
SUFFIX=$$
CLIENT="lyfcc$SUFFIX"
PROXY="lyfpr$SUFFIX"
UPSTREAM="lyfup$SUFFIX"
PAGE_PORT=8081
XRAY_PID=
PAGE_PID=
UPSTREAM_PID=
KEEP=${KEEP:-0}

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
    set +e
    for pid in "$XRAY_PID" "$PAGE_PID" "$UPSTREAM_PID"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null
    done
    if [ -f "$TMP/nginx.pid" ]; then
        kill "$(cat "$TMP/nginx.pid" 2>/dev/null)" 2>/dev/null
    fi
    if [ "$KEEP" = "1" ]; then
        log "KEEP=1; namespaces and $TMP left in place"
        return
    fi
    ip netns del "$CLIENT" 2>/dev/null
    ip netns del "$PROXY" 2>/dev/null
    ip netns del "$UPSTREAM" 2>/dev/null
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

[ "$(id -u)" -eq 0 ] || fail "must run as root"
for command in ip nft go python3; do
    command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
[ -x "$HELPER" ] || fail "proxy failure helper is missing or not executable: $HELPER"
[ -f "$PAGE" ] || fail "packaged proxy failure page is missing: $PAGE"

export GOMODCACHE=${GOMODCACHE:-/root/go/pkg/mod-alt}
export GOPROXY=${GOPROXY:-off}
cd "$BACKEND"

log "== render generated capture plan =="
go run ./internal/runtime/service/testdata/render -mode json -lan 10.200.1.0/24 >"$TMP/plan.json"
go run ./internal/runtime/service/testdata/render -mode nft -lan 10.200.1.0/24 >"$TMP/capture.nft"
python3 - "$TMP/plan.json" >"$TMP/fields" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1], encoding="utf-8"))
network = plan["network"]
capture = plan["capture"]
routing = plan["routing"]
print(network["ingress_host_interface"])
print(network["listener_port"])
print(capture["mark"])
print(routing["table_id"])
print(routing["rule_priority"])
PY
INGRESS=$(sed -n 1p "$TMP/fields")
PORT=$(sed -n 2p "$TMP/fields")
MARK=$(sed -n 3p "$TMP/fields")
TABLE=$(sed -n 4p "$TMP/fields")
PRIORITY=$(sed -n 5p "$TMP/fields")
# This is the exact state contract written by the control API
# (proxyFailurePageConfig in backend/internal/httpapi/proxy_reconciler.go).
cat >"$TMP/failure-state.json" <<EOF
{
  "version": 1,
  "enabled": true,
  "active": false,
  "egress_id": "proxy-netns",
  "ingress_interface": "$INGRESS",
  "page_port": $PAGE_PORT,
  "bypass_destinations": ["10.200.0.0/24", "10.200.1.0/24"]
}
EOF
log "ingress=$INGRESS tproxy-port=$PORT mark=$MARK table=$TABLE rule-priority=$PRIORITY"

log "== choose proxy engine =="
XRAY_BINARY=${LY_ROUTE_XRAY_BINARY:-}
if [ -z "$XRAY_BINARY" ] && command -v xray >/dev/null 2>&1; then
    XRAY_BINARY=$(command -v xray)
fi
if [ -n "$XRAY_BINARY" ] && [ -x "$XRAY_BINARY" ]; then
    ENGINE=xray
    log "proxy engine: xray ($XRAY_BINARY)"
else
    ENGINE=relay
    log "proxy engine: transparent Python relay (set LY_ROUTE_XRAY_BINARY to use xray)"
fi

log "== prepare failure page server and upstream =="
mkdir -p "$TMP/page"
cp "$PAGE" "$TMP/page/index.html"
# nginx workers drop privileges, so the throwaway page directory must be
# readable by them.
chmod 0755 "$TMP" "$TMP/page"
chmod 0644 "$TMP/page/index.html"
cat >"$TMP/upstream.py" <<'PY'
import socket, sys, threading

log_path = sys.argv[1]

def record(port, size):
    with open(log_path, "a", encoding="utf-8") as handle:
        handle.write("%d %d\n" % (port, size))
        handle.flush()

def http_connection(connection):
    try:
        data = connection.recv(4096)
        record(80, len(data))
        body = b"DIRECT-UPSTREAM"
        connection.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(body) + body)
    finally:
        connection.close()

def serve_http():
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("203.0.113.10", 80))
    listener.listen(16)
    while True:
        connection, _ = listener.accept()
        threading.Thread(target=http_connection, args=(connection,), daemon=True).start()

def serve_dns():
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    udp.bind(("203.0.113.10", 53))
    while True:
        data, peer = udp.recvfrom(2048)
        record(53, len(data))
        udp.sendto(b"DNS-OK", peer)

threading.Thread(target=serve_dns, daemon=True).start()
serve_http()
PY
cat >"$TMP/client.py" <<'PY'
import socket, sys

mode, host, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
if mode == "http":
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(4)
    try:
        sock.connect((host, port))
        sock.sendall(b"GET / HTTP/1.1\r\nHost: failure.invalid\r\nConnection: close\r\n\r\n")
        chunks = []
        while True:
            data = sock.recv(4096)
            if not data:
                break
            chunks.append(data)
        body = b"".join(chunks).decode("utf-8", "replace")
        if "LY-ROUTE-PROXY-FAILURE-PAGE" in body:
            print("FAILURE-PAGE")
        elif "DIRECT-UPSTREAM" in body:
            print("DIRECT-UPSTREAM")
        else:
            print("UNEXPECTED-RESPONSE")
    except socket.timeout:
        print("TIMEOUT")
    except Exception as exc:
        print("ERROR", exc)
    finally:
        sock.close()
elif mode == "tls":
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(4)
    try:
        sock.connect((host, port))
        sock.sendall(b"\x16\x03\x01\x00\x2f\x01\x00\x00\x2b\x03\x03" + b"a" * 32)
        try:
            data = sock.recv(16)
            print("CONNECTED" if data else "RESET")
        except ConnectionResetError:
            print("RESET")
        except socket.timeout:
            print("TIMEOUT")
    except (ConnectionResetError, ConnectionRefusedError):
        print("RESET")
    except socket.timeout:
        print("TIMEOUT")
    except Exception as exc:
        print("ERROR", exc)
    finally:
        sock.close()
elif mode == "tcp":
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(3)
    try:
        sock.connect((host, port))
        print("CONNECTED")
    except ConnectionResetError:
        print("RESET")
    except socket.timeout:
        print("TIMEOUT")
    except Exception as exc:
        print("ERROR", exc)
    finally:
        sock.close()
elif mode == "dns":
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(3)
    try:
        sock.sendto(b"\xab\xcd\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x03www\x07example\x03com\x00\x00\x01\x00\x01", (host, port))
        data, _ = sock.recvfrom(2048)
        print(data.decode("utf-8", "replace"))
    except socket.timeout:
        print("TIMEOUT")
    except Exception as exc:
        print("ERROR", exc)
    finally:
        sock.close()
PY

if command -v nginx >/dev/null 2>&1; then
    cat >"$TMP/nginx.conf" <<EOF
daemon off;
worker_processes 1;
pid $TMP/nginx.pid;
error_log $TMP/nginx-error.log warn;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path $TMP/nginx-body;
    proxy_temp_path $TMP/nginx-proxy;
    fastcgi_temp_path $TMP/nginx-fastcgi;
    uwsgi_temp_path $TMP/nginx-uwsgi;
    scgi_temp_path $TMP/nginx-scgi;
    server {
        listen $PAGE_PORT;
        server_name _;
        root $TMP/page;
        index index.html;
        add_header Cache-Control "no-store";
        location = /healthz { return 200 "proxy-failure\n"; }
        location / { try_files \$uri /index.html =404; }
    }
}
EOF
    PAGE_ENGINE=nginx
else
    PAGE_ENGINE=python
fi

log "== create isolated namespaces =="
ip netns add "$CLIENT"
ip netns add "$PROXY"
ip netns add "$UPSTREAM"
ip link add "$INGRESS" netns "$PROXY" type veth peer name c0 netns "$CLIENT"
ip link add p_up netns "$PROXY" type veth peer name u0 netns "$UPSTREAM"

ip -n "$CLIENT" link set lo up
ip -n "$CLIENT" link set c0 up
ip -n "$CLIENT" addr add 10.200.0.2/24 dev c0
ip -n "$CLIENT" route add default via 10.200.0.1

ip -n "$PROXY" link set lo up
ip -n "$PROXY" link set "$INGRESS" up
ip -n "$PROXY" addr add 10.200.0.1/24 dev "$INGRESS"
ip -n "$PROXY" link set p_up up
ip -n "$PROXY" addr add 203.0.113.1/24 dev p_up

ip -n "$UPSTREAM" link set lo up
ip -n "$UPSTREAM" link set u0 up
ip -n "$UPSTREAM" addr add 203.0.113.10/24 dev u0
ip -n "$UPSTREAM" route add default via 203.0.113.1

ip netns exec "$PROXY" sysctl -q -w net.ipv4.ip_forward=1
ip netns exec "$PROXY" sysctl -q -w net.ipv4.conf.all.rp_filter=0
ip netns exec "$PROXY" sysctl -q -w "net.ipv4.conf.$INGRESS.rp_filter=0"
ip netns exec "$PROXY" sysctl -q -w "net.ipv4.conf.$INGRESS.accept_local=1"
ip netns exec "$PROXY" sysctl -q -w net.ipv4.ip_nonlocal_bind=1

log "== install generated capture and policy route =="
ip netns exec "$PROXY" nft -f "$TMP/capture.nft"
ip netns exec "$PROXY" ip rule add fwmark "$MARK/0xffffffff" table "$TABLE" priority "$PRIORITY"
ip netns exec "$PROXY" ip route add local 0.0.0.0/0 dev lo table "$TABLE"

: >"$TMP/upstream.log"
ip netns exec "$UPSTREAM" python3 "$TMP/upstream.py" "$TMP/upstream.log" >"$TMP/upstream-stdout.log" 2>&1 &
UPSTREAM_PID=$!

if [ "$ENGINE" = xray ]; then
    uuid=11111111-1111-1111-1111-111111111111
    cat >"$TMP/xray.json" <<EOF
{
  "log": {"level": "warning"},
  "inbounds": [{
    "tag": "capture-in",
    "listen": "0.0.0.0",
    "port": $PORT,
    "protocol": "dokodemo-door",
    "settings": {"network": "tcp", "followRedirect": true},
    "streamSettings": {"sockopt": {"tproxy": "tproxy"}}
  }],
  "outbounds": [{"tag": "upstream", "protocol": "freedom"}]
}
EOF
    ip netns exec "$PROXY" "$XRAY_BINARY" run -c "$TMP/xray.json" >"$TMP/xray.log" 2>&1 &
    XRAY_PID=$!
else
    cat >"$TMP/relay.py" <<'PY'
import socket, sys, threading

port = int(sys.argv[1])
listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
listener.setsockopt(socket.IPPROTO_IP, 19, 1)  # IP_TRANSPARENT
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("0.0.0.0", port))
listener.listen(16)

def handle(connection):
    try:
        destination = connection.getsockname()
        upstream = socket.create_connection(destination, timeout=3)
        connection.settimeout(3)
        payload = connection.recv(4096)
        if payload:
            upstream.sendall(payload)
            reply = upstream.recv(4096)
            if reply:
                connection.sendall(reply)
    finally:
        connection.close()

while True:
    connection, _ = listener.accept()
    threading.Thread(target=handle, args=(connection,), daemon=True).start()
PY
    ip netns exec "$PROXY" python3 "$TMP/relay.py" "$PORT" >"$TMP/relay.log" 2>&1 &
    XRAY_PID=$!
fi

if [ "$PAGE_ENGINE" = nginx ]; then
    ip netns exec "$PROXY" nginx -c "$TMP/nginx.conf" >"$TMP/nginx-stdout.log" 2>&1 &
else
    ip netns exec "$PROXY" python3 -m http.server "$PAGE_PORT" --bind 127.0.0.1 --directory "$TMP/page" >"$TMP/page.log" 2>&1 &
fi
PAGE_PID=$!
sleep 1

client_tcp() { ip netns exec "$CLIENT" python3 "$TMP/client.py" "$@"; }
upstream_http_bytes() { awk '$1 == 80 { total += $2 } END { print total + 0 }' "$TMP/upstream.log"; }
upstream_dns_bytes() { awk '$1 == 53 { total += $2 } END { print total + 0 }' "$TMP/upstream.log"; }

expect() {
    label=$1
    expected=$2
    shift 2
    output=$(client_tcp "$@" 2>&1) || true
    log "$label -> $output"
    printf '%s\n' "$output" | grep -q "$expected" || fail "$label expected $expected, got: $output"
}

log "== normal mode: captured traffic reaches the upstream through the proxy =="
before=$(upstream_http_bytes)
expect normal-proxy DIRECT-UPSTREAM http 203.0.113.10 80
after=$(upstream_http_bytes)
[ "$after" -gt "$before" ] || fail "normal mode did not deliver bytes to the upstream ($before -> $after)"
log "upstream bytes after normal mode: $before -> $after"

log "== failure mode: helper enter =="
: >"$TMP/upstream.log"
ip netns exec "$PROXY" env LY_ROUTE_PROXY_FAILURE_STATE="$TMP/failure-state.json" "$HELPER" enter
ip netns exec "$PROXY" nft list table inet ly_route_proxy_failure >"$TMP/failure.nft"
ip netns exec "$PROXY" nft list table inet ly_route_proxy_failure_guard >>"$TMP/failure.nft"
if ip netns exec "$PROXY" nft list chain inet ly_route_proxy_capture proxy_prerouting | grep -q tproxy; then
    fail "failure mode left TPROXY rules in the normal capture chain"
fi
grep -q 'tcp dport 53 return' "$TMP/failure.nft" || fail "failure generation does not bypass DNS"
grep -q 'meta mark != 0x00000000 return' "$TMP/failure.nft" || fail "failure generation does not bypass marked packets"
grep -q 'redirect to :8081' "$TMP/failure.nft" || fail "failure generation does not redirect TCP/80"
grep -q 'reject with tcp reset' "$TMP/failure.nft" || fail "failure generation does not reset TCP/443"

expect failure-page FAILURE-PAGE http 203.0.113.10 80
[ "$(upstream_http_bytes)" -eq 0 ] || fail "failure mode leaked HTTP bytes to the upstream: $(upstream_http_bytes)"
expect failure-tls-reset RESET tls 203.0.113.10 443
[ "$(upstream_http_bytes)" -eq 0 ] || fail "failure mode leaked TLS bytes to the upstream"
expect failure-other-tcp TIMEOUT tcp 203.0.113.10 8443

before_dns=$(upstream_dns_bytes)
expect failure-dns-direct DNS-OK dns 203.0.113.10 53
[ "$(upstream_dns_bytes)" -gt "$before_dns" ] || fail "failure mode did not keep DNS direct"
[ "$(upstream_http_bytes)" -eq 0 ] || fail "failure mode leaked HTTP bytes to the upstream: $(upstream_http_bytes)"
log "upstream bytes in failure mode: http=$(upstream_http_bytes) dns=$(upstream_dns_bytes)"

log "== failure mode without the page server: blackhole, still no direct Internet =="
kill "$PAGE_PID" 2>/dev/null || true
PAGE_PID=
if [ -f "$TMP/nginx.pid" ]; then
    kill "$(cat "$TMP/nginx.pid" 2>/dev/null)" 2>/dev/null || true
fi
attempt=0
while [ "$attempt" -lt 30 ]; do
    if ! ip netns exec "$PROXY" ss -H -ltn 2>/dev/null | awk '{print $4}' | grep -q ":$PAGE_PORT\$"; then
        break
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
if ip netns exec "$PROXY" ss -H -ltn 2>/dev/null | awk '{print $4}' | grep -q ":$PAGE_PORT\$"; then
    fail "could not stop the page listener on :$PAGE_PORT"
fi
: >"$TMP/upstream.log"
ip netns exec "$PROXY" env LY_ROUTE_PROXY_FAILURE_STATE="$TMP/failure-state.json" LY_ROUTE_PAGE_WAIT_ATTEMPTS=1 "$HELPER" enter
if ip netns exec "$PROXY" nft list table inet ly_route_proxy_failure | grep -q 'redirect to'; then
    fail "page-down failure mode still redirects TCP/80 to a dead page server"
fi
expect blackhole-http TIMEOUT http 203.0.113.10 80
expect blackhole-tls-reset RESET tls 203.0.113.10 443
[ "$(upstream_http_bytes)" -eq 0 ] || fail "blackhole mode leaked HTTP bytes to the upstream"
log "blackhole upstream bytes: http=$(upstream_http_bytes)"

log "== recovery: re-install capture, then helper leave =="
ip netns exec "$PROXY" nft -f "$TMP/capture.nft"
ip netns exec "$PROXY" env LY_ROUTE_PROXY_FAILURE_STATE="$TMP/failure-state.json" "$HELPER" leave
ip netns exec "$PROXY" nft list table inet ly_route_proxy_failure 2>/dev/null && fail "failure generation survived recovery" || true
before=$(upstream_http_bytes)
expect recovered-proxy DIRECT-UPSTREAM http 203.0.113.10 80
[ "$(upstream_http_bytes)" -gt "$before" ] || fail "recovery did not restore proxying to the upstream"

log "PASS: normal=upstream, failure=page+443-reset+DNS-direct+zero-upstream-bytes, recovery=proxy"
