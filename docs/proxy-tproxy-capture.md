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

### DNS LCP guard (`runtime/service`, `httpapi`)

The P0-2 regression showed the `dns_lcp_guard` input chain dropping every
packet to the LAN gateway's port 53, including legitimate SmartDNS input:

```text
ip daddr 192.168.88.1 udp dport 53 drop
ip daddr 192.168.88.1 tcp dport 53 drop
```

The guard now requires `LCPGuardInterfaces` and renders an `iifname` match:

```text
iifname "lylan-eth2" ip daddr 192.168.88.1 udp dport 53 counter drop
```

`dnsLCPGuardPlan` derives the interface with
`vpp.LANControlPlaneHostInterface(assignment.LinuxInterface)` (falling back to
the `lyroute-*` VPP name). Only the duplicate copy seen on the VPP LCP is
dropped; normal LAN input and DNS service TAP input to the same gateway
address stay deliverable to SmartDNS. If no interface is supplied, the
renderer emits no drop (fail-open) instead of reintroducing the broad rule.

### Xray (`runtime/proxy`, `httpapi`)

`EnsureDirectFallback`:

- adds a `direct` freedom outbound,
- rewrites each proxy-selected rule to a single-node balancer with
  `fallbackTag: "direct"`,
- adds an explicit direct catch-all,
- enables the observatory so an unreachable node is marked dead and the
  balancer actually falls back.

`ApplyOutboundSocketMark` marks **every** Xray outbound, including `direct`.
VPP owns the WAN interface, so the proxy egress mark is the only Linux path
to the WAN; an unmarked direct socket would use the management/default table
and fail. The node and the direct fallback both leave through the same VPP
egress TAP/NAT role.

### systemd fail-open guard and boot restore

`packaging/rootfs-overlay/usr/lib/ly-route/proxy-failopen` is called by
`xray.service`:

- `ExecStartPre`: flush only the `proxy_prerouting` chain (never the DNS
  chains in the same table).
- `ExecStartPost (on)`: wait until every TPROXY inbound port is listening,
  re-apply `/etc/nftables.conf` (so a reboot needs no manual apply), then
  remove the VPP bypass rules.
- `ExecStopPost (off)`: add VPP pre-NAT rules for every `skip-nat` proxy
  policy at `priority - 1` with reserved ids `9000..9063`, then flush host
  capture. VPP immediately stops steering selected traffic into the proxy
  TAP and the normal LAN -> WAN NAT path takes over.

The bypass mirrors each proxy policy's source/destination/protocol/ports and
is idempotent. It is added before capture is flushed and removed only after a
healthy capture install, so a listener that never becomes ready installs the
bypass instead of a black hole.

`packaging/rootfs-overlay/etc/systemd/system/vpp.service.d/40-ly-route-policy-routing.conf`
pulls in the otherwise-static `ly-route-policy-routing.service` on every VPP
start. That service owns the Linux ingress/outbound fwmark rules and policy
tables (including the proxy egress table); without it a reboot leaves Xray
and capture healthy but every marked upstream unrouteable.

### Live fail-open / reboot evidence (P0-1 lab, before the shared lab reset)

- `systemctl stop xray`: helper logged `capture removed and proxy steering
  bypassed`; VPP gained `rule id 9001 priority 9 ... bypass 1`; client curl
  returned `200` in `0.035s` (previously 15s timeout).
- `systemctl start xray`: helper logged `capture installed and proxy steering
  enabled`; reserved bypass removed; `nft` showed both TCP/UDP TPROXY rules;
  client curl returned `200`.
- Cold reboot: `xray` and `vpp` active with `nftables.service` still disabled;
  `ly_route_proxy_capture proxy_prerouting` was present with both TPROXY
  rules without any manual apply; helper journal showed the ExecStartPost
  install.
- Direct LAN (`192.168.88.50:8080`) returned `200` while capture was active.

The shared lab VM was later reset to a pre-configuration disk state (PPPoE
session and the deployed helper/drop-ins disappeared). The reboot acceptance
above is from the verified run before that reset; the final acceptance must be
repeated on a freshly built image that contains these overlay files. The
reboot retry and DNS-only exemption in the policy-routing drop-in are covered
by `policy_routing_ready_test.go` and the helper fixture test.

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

bash scripts/test-lan-dns-lcp-guard-netns.sh
lan-udp -> DNS-OK
lan-tcp -> DNS-OK
lcp-udp -> TIMEOUT
lcp-tcp -> TIMEOUT
PASS: LAN 53 udp/tcp delivered; LCP duplicate udp/tcp dropped by scoped guard
```

The second script renders the real guard through `testdata/render`, so it
fails if an unscoped gateway-port-53 drop reappears.

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
