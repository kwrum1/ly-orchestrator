# Proxy capture / TPROXY fail-open (P0-1)

Branch `feat/p0-proxy-tproxy` (base `0ce76c0`). Scope: proxy capture
generation only. No VPP/apply graph (P0-5/P0-3), frontend or security changes.

## Problem

The generated capture chain tproxied every TCP/UDP packet arriving on the VPP
proxy ingress TAP. That had three failure modes:

- LAN, DNS, link-local and the proxy service `/30` peers were captured too, so
  same-source LAN upstreams and the proxy handoff itself could be black-holed.
- A stopped/failed Xray left a live TPROXY rule pointing at a dead listener;
  every selected packet was dropped.
- Fixed-node and subscription runtimes installed only the proxy outbound. A
  failed node had no direct path, and `ApplyOutboundSocketMark` marked every
  outbound, so even a direct outbound would have been sent back through the
  proxy underlay.
- IPv6 packets traversed the IPv4 TPROXY rule.

## Changes

### nft capture (`runtime/proxy`)

`CaptureGuardRules` now precedes the terminal TPROXY statements:

1. return already-marked packets,
2. return non-ingress traffic,
3. return TCP/UDP port 53 (DNS interception owns it),
4. return every bypass prefix: `0.0.0.0/8`, `127.0.0.0/8`,
   `169.254.0.0/16`, `224.0.0.0/4`, `240.0.0.0/4`, broadcast, both proxy
   service `/30` peers, and all resolved LAN return routes,
5. only then `meta nfproto ipv4 ... tproxy ip to :<port> accept` for TCP/UDP.

`BindServiceNetwork` refreshes the guard prefix without dropping adaptive lane
rules. The chain remains `type filter hook prerouting priority -150` with
`policy accept`, so an empty/absent rule set is fail-open by construction.

`FailOpenNftablesCapture` renders a valid, intentionally empty table
(`flush table inet ly_route_proxy_capture` + no rules). When a proxy egress is
configured but its runtime cannot be compiled, the runtime plan emits this
artifact instead of leaving the previous TPROXY generation installed. The
nftables readback accepts this generation only via the explicit
`ly-route-proxy-fail-open` marker and only for the managed table.

### Xray (`runtime/proxy`, `httpapi`)

`EnsureDirectFallback`:

- adds a `direct` freedom outbound,
- rewrites each proxy-selected rule to a single-node balancer with
  `fallbackTag: "direct"`,
- adds an explicit direct catch-all,
- enables the observatory so an unreachable node is marked dead and the
  balancer actually falls back.

`ApplyOutboundSocketMark` skips the `direct` tag: a failed proxy uses the
ordinary WAN route instead of re-entering the proxy underlay.

### systemd fail-open guard

`packaging/rootfs-overlay/etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf`
flushes only `inet ly_route_proxy_capture` in `ExecStartPre` and `ExecStopPost`.
An Xray restart/crash therefore removes capture before the listener can be
unavailable; the next verified apply reinstalls it (service order is Xray then
Nftables).

## Verification

Unit/regression (all pass on the independent worktree):

```
GOMODCACHE=/root/go/pkg/mod-alt GOPROXY=off go test -count=1 \
  ./internal/runtime/... ./internal/httpapi
```

- `proxy/capture_test.go`: guard ordering, LAN/DNS/service bypass, adaptive
  lane preservation, fail-open plan, idempotent direct fallback, unmarked
  direct outbound.
- `service/nftables_failopen_test.go`: fail-open rendering and readback.
- `httpapi/task8_service_runtime_test.go`: a bad subscription still emits the
  capture flush and no Xray/Linux-routing artifact.

Independent netns acceptance (no lab, own namespaces):

```
bash scripts/test-proxy-tproxy-netns.sh
PASS: selected=TPROXY, lan/dns=direct, flush=fail-open, reapply=idempotent
```

It renders the real plan with `testdata/render`, applies it with `nft -f`,
and proves: selected -> TPROXY listener, LAN/DNS -> forwarded direct,
flush -> forwarded direct, second `nft -f` -> same behaviour.

Generated Xray config is accepted by the tested binary:

```
/root/work/xray-node-test/xray -test -config <rendered config>
Configuration OK.
```

and nft syntax is accepted by `nft -c -f` for both capture and fail-open.

## Rollback

- Revert the commits below; `nft` tables are owned and re-flushed by the next
  apply.
- Remove the xray drop-in and `systemctl daemon-reload` to drop the
  ExecStartPre/ExecStopPost guard on a live appliance.

## Cherry-pick

```bash
git fetch /root/work/ly-route-gateway-proxy feat/p0-proxy-tproxy
git cherry-pick <commit>
```
