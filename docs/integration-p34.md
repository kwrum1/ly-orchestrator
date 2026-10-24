# P3 + P4 integration onto the P2 mainline

Status: merged; build, vet, full backend suite and rootfs scaffold validation are clean.

- Branch: `feat/integration-p34` (worktree `/root/work/ly-route-integration-p34`)
- Base: `a90083c` (`feat/arch-v1-p2`, includes the LAN DHCP ACL/NAT fix `d14fd8a`)
- P3: `feat/integration-p3` at `fc88d5f` (P3 proxy-failure `e2cddcc` merged on `84709b1`)
- P4: `feat/p4-ipv6` at `e87232f` (based on `84709b1`)

## Merge shape

Both merges used `git merge --no-ff` and produced **no textual conflicts**.

| Order | Merge commit | Parents |
| --- | --- | --- |
| 1 | `bf529ad` merge: integrate P3 proxy fail-closed and fixed failure page | `a90083c` (P2) + `fc88d5f` (P3) |
| 2 | `a5da813` merge: integrate P4 IPv6 RA/DHCPv6 and prefix delegation | `bf529ad` + `e87232f` (P4) |

Files changed on both sides auto-merged; both sides were kept and were verified by
diffing the merged files against each parent:

| File | P3 / P2 side | P4 side |
| --- | --- | --- |
| `backend/internal/httpapi/server.go` | P2 default-WAN-deny exclusion in `runtimePolicyForAddressAssignments`; P3 failure-page actuator/tracker and additive status fields | dnsmasq IPv6 planning, `currentDHCPServers` warnings, removal of the old `currentIPv6RAArtifacts` path |
| `backend/internal/runtime/service/runtime.go` | P3 `Runtime.InvalidateHealth()` | P4 `DnsmasqPlan` IPv6 fields (`ipv6_prefix`, `ipv6_mode`, `option6:dns-server`, `ra-param`) and dual-stack rendering |
| `scripts/build-rootfs.sh` | P2 overlay-after-debs ordering; P3 `ly-route-proxy-failure.conf`, `proxy-failure` helper, packaged page | P4 SmartDNS `bind [::]:53` / DHCPv6-advertised DNS |
| `scripts/validate-rootfs-scaffold.sh` | P2/P3 scaffold assertions (8081 listener, page marker, helper table) | P4 `bind [::]:53` assertion |

No source file needed manual conflict resolution.

## Post-merge fix (test fixture only)

`TestRuntimePlanMultipleLANsGetDistinctDelegated64s` failed after the merge with
`pppoe peer requires id, interface, and username`. The test fixture (P4) omitted the
PPPoE credentials for `wan0`, while `service.RenderPPPoEArtifacts` has required a
username since the original runtime work (`d46a179`). The same test fails on the
unmodified `feat/p4-ipv6` worktree, so this is a pre-existing P4 test defect, not a
merge regression. Fixed by adding `username`/`password` to that one fixture, matching
every other fixture in `runtime_ipv6_test.go`; no product code changed.

## Invariants preserved

- The seeded `Default WAN Inbound Deny` ACL is still excluded from rendering and
  attachment (`runtimePolicyForAddressAssignments`, ACL/QoS and security-ACL
  lifecycles, rollback replay). P2 tests `default_wan_acl_test.go` and
  `lan_dhcp_acl_test.go` pass.
- NAT44 pool still includes the WAN address and the DNS service tap in-interface
  (`vpp/adapter.go`, `dns_service_network_roles.go`).
- P3 fail-closed: `EnsureFailureFallback` replaces the fail-open direct outbound with
  a `failure` blackhole, the packaged `proxy-failure` helper drives the fixed local
  page on :8081, and the state file is written before the helper runs so the systemd
  stop/crash path stays fail closed.
- P4 IPv6: dnsmasq owns RA and DHCPv6 per LAN with one distinct /64 per delegated
  LAN, `enable-ra` only when a usable prefix exists, SmartDNS bound on `[::]:53`.

## Verification

Host: Linux x86_64, go1.24.4, `-p 1` (Gate A QEMU may be active; work kept CPU-light).

| Check | Result |
| --- | --- |
| `go build ./...` (backend) | exit 0, no output |
| `go vet ./...` (backend) | exit 0, no output (also clean on the pre-merge base) |
| `go test -p 1 -count=1 ./...` | pass: 22 packages `ok`, 6 `no test files`, 0 failures (`internal/httpapi` 346.240s) |
| `sh scripts/validate-rootfs-scaffold.sh` | pass (`rootfs scaffold validation passed`) |
| `sh scripts/test-proxy-failopen-helper.sh` | pass (`PASS: proxy fail-open fast path keeps steering and publishes the local failure page/blackhole; direct bypass only without a proxy egress`) |
| `gofmt -l` on the 24 touched Go files | clean (only the deleted `adaptive_proxy_reconciler.go` is reported missing) |

Not run here: `scripts/test-proxy-failure-page-netns.sh` and
`scripts/test-dnsmasq-dhcpv6-netns.sh` (they create netns/dnsmasq fixtures and the
host is shared with an active Gate A run); Gate A/QEMU acceptance.
