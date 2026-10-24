# P3 proxy-failure integration into the P2 line

Status: merged; build, vet and targeted tests are clean.

- Branch: `feat/integration-p3`
- Merge commit: `4695b03` (`84709b1` P2 line + `e2cddcc` P3 proxy-failure)
- Merge base: `fb23dbc`
- Source: `feat/p3-proxy-failure` (5 commits, HEAD `e2cddcc`)
- Target: `feat/arch-v1-p2` line at `84709b1`

## Merge shape

The merge used `git merge --no-ff` and produced **no textual conflicts**.

Two files changed on both sides and were auto-merged; both sides were kept:

| File | P2 side | P3 side |
| --- | --- | --- |
| `scripts/build-rootfs.sh` | re-applies the product overlay after `install_extra_debs` so package payloads cannot revert product files | requires and installs `ly-route-proxy-failure.conf`, `usr/lib/ly-route/proxy-failure`, and the packaged failure page |
| `scripts/validate-rootfs-scaffold.sh` | asserts overlay-after-debs ordering | asserts 8081 listener, packaged page marker/no external assets, helper table name, helper executable bit |

Verified by diffing the merged files against `fb23dbc`: both additions are present, and
`scripts/validate-rootfs-scaffold.sh` (which now checks both invariants) passes.

Backend overlap was one-sided, so no semantic conflict:

- `backend/internal/runtime/service/runtime.go`: P3 adds only `Runtime.InvalidateHealth()`; P2 did not touch this file.
- `backend/gateway/main.go`, `backend/internal/httpapi/server.go`: P3 only (`StartAdaptiveProxyReconciler` -> `StartProxyReconciler`, failure-page actuator/tracker wiring, additive status fields).
- `adaptive_proxy_reconciler.go` is replaced by `proxy_reconciler.go`; no references to the old reconciler remain (`grep` clean).
- P2 dnsmasq/DHCP/firstboot/SmartDNS work is intact: `git diff 84709b1..HEAD` contains only P3 files, and
  `systemctl_readback.go` still validates `dnsmasq --test -C /etc/dnsmasq.d/ly-route-dhcp.conf`.

## Verification

Host: Linux x86_64, go1.24.4, tests run with `-p 1` (Gate A VM active on CPUs 8-15).

| Check | Result |
| --- | --- |
| `gofmt -l` on merged tree | no new unformatted files; the 10 listed files are unchanged pre-existing drift (identical list on `84709b1`) |
| `go build ./...` (backend) | exit 0, no output |
| `go vet ./internal/runtime/proxy ./internal/runtime/service ./internal/httpapi` | exit 0 |
| `go test -p 1 ./internal/runtime/proxy ./internal/runtime/service -count=1` | pass (0.078s, 0.300s) |
| `go test -p 1 ./internal/httpapi -run 'Proxy\|Xray\|Adaptive\|DHCP' -count=1` | pass (33.552s, 51 matching tests) |
| `sh scripts/validate-rootfs-scaffold.sh` | pass (`rootfs scaffold validation passed`) |
| `sh scripts/test-proxy-failopen-helper.sh` | pass (`PASS: proxy fail-open fast path keeps steering and publishes the local failure page/blackhole; direct bypass only without a proxy egress`) |
| `sh -n` on changed shell scripts/helpers | all ok |
| isolated `nginx -t` with `packaging/nginx/ly-route-proxy-failure.conf` | syntax ok |

Not run (deliberately or out of scope): `go test ./...`, `scripts/test-proxy-failure-page-netns.sh`
(needs netns and a quiet host), Gate A/QEMU, full rootfs and upgrade-package builds.

## Integration gaps and follow-ups

1. **In-place firmware upgrade does not deliver the P3 artifacts (blocker for upgrades; fresh
   rootfs images are fine).** `backend/internal/httpapi/firmware_install.go` installs a fixed
   allowlist (control binary, `vpp-apply`, VPP plugin, PPPoE helper, `recover-runtime.sh`,
   `runtime-check.sh`, `firstboot.sh`, admin bundle, admin nginx config, artifact manifest) and
   has no `proxy` references. Consequences on an upgraded device:
   - the old fail-open `proxy-failopen` helper stays installed, so `xray` stop still installs the
     VPP bypass and returns selected traffic to the direct Internet - the P3 fail-closed fix is
     not effective until reflash;
   - `/usr/lib/ly-route/proxy-failure` and the page/nginx config are absent, so the reconciler's
     first `Deactivate` (`proxy_reconciler.go`, stale-generation clear) fails every tick and
     adaptive capture rebalancing is skipped.
   Fix belongs in `firmware_install.go` (install + backup/restore) plus `build-upgrade-package.sh`
   manifest plumbing. Not changed here to avoid redefining the validated upgrade transaction in a
   merge commit.
2. **Upgrade checksum manifest omits new artifacts.** `checksums.sha256`/`manifest.json` only hash
   `ly-route-admin.conf`; `ly-route-proxy-failure.conf`, `proxy-failopen`, `proxy-failure`, and
   `usr/share/ly-route/proxy-failure/index.html` are copied into the package but not hashed or
   listed. Integrity verification (`sha256sum -c`) therefore ignores them.
3. **The xray systemd fast path is not shipped by upgrades.** `build-upgrade-package.sh` does not
   include `etc/systemd/system/xray.service.d/10-ly-route-proxy-fail-open.conf` at all; only fresh
   images get the `ExecStopPost` failure-page hook. This compounds item 1.
4. **Rollback leaves failure-page artifacts behind.** The upgrade backup/restore covers the admin
   nginx config and `/usr/lib/ly-route` tree but not `ly-route-proxy-failure.conf` or
   `/usr/share/ly-route/proxy-failure`, so a rollback to P2 can leave nginx listening on 8081.
5. **UI expectations.** The new status fields (`all_nodes_down`, `failure_page_active`,
   `failure_reason`, `failure_since`, `recovered_at`, `last_health_sample_at`,
   `consecutive_all_down_samples`, `dead_node_ids`, `failure_error`) are additive on
   `/proxy/xray/runtime` and do not change existing keys; `frontend/gateway/app.js` does not read
   them, so nothing breaks, but an all-dead proxy is not surfaced in the UI. Plan a follow-up to
   show failure-page state in the proxy panel.
6. Netns-level dataplane assertions (`test-proxy-failure-page-netns.sh`) still need a run on a quiet
   host or Gate A before release.