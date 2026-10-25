# Orchestrator acceptance on ESXi (AF_PACKET mode)

## Fix run — release-blocking regressions resolved (2026-10-25)

Run `20261025-fix-release-orch-regressions` on branch
`fix/release-orch-regressions`, built from the branch HEAD. This re-runs the
Orchestrator acceptance against an image built by the fixed pipeline after the
two regressions found on `506cc4c`. Data path is the same explicit AF_PACKET
acceptance mode (`hook=af_packet`, `mode=linux_packet_socket`,
`acceptance_only=true`, `high_performance=false`); no performance claim is
made. Evidence:
`/root/work/orch-acceptance/evidence/20261025-fix-release-orch-regressions/`
(`summary.json`, `README.md`, `analysis-status-regression-fix.md`,
`analysis-ui-bundle-fix.md`). The exact build commit and artifact SHA-256
values are recorded in `build/git.txt`, `build/SHA256SUMS`,
`build/rootfs-artifact-manifest.json` and `build/disk-image.manifest.json`; the
image artifact manifest must report `product=orchestrator` and
`source_commit` equal to the branch HEAD that was built.

Commits on `fix/release-orch-regressions` (not pushed):

- `af7f1a6` — `fix(orchestrator): keep runtime status on the transparent datapath`
- `3e0ca15` — `fix(packaging): pin the accepted 11-page Orchestrator UI bundle`
- the acceptance-documentation commits on top of `3e0ca15` (this file); the
  final image is built from the resulting branch HEAD so the artifact
  manifest's `source_commit` matches it

### Verdict

| # | Requirement | Verdict | Evidence |
|---|---|---|---|
| 1 | Deploy the rebuilt release; manifest product `orchestrator`, `source_commit` = built branch HEAD | **PASS** | `build/*`, `final/identity-and-vpp.txt` |
| 2 | `health` ok; password change/login; Orchestrator profile | **PASS** | `phase-a.log`, `health*.json`, `login*.json`, `product.json` |
| 3 | AF datapath: VPP owns both NICs, apply committed, `/runtime/status` running + generation; stable across ≥5 polls and after a control-api restart | **PASS** | `phase-b.log`, `functional/*`, `status-stability/*`, `final/api-runtime-status-{1..5}.json` |
| 4 | Control-api restart and VPP restart keep the same generation and forwarding | **PASS** | `restart-persistence-test.log`, `persistence/*` |
| 5 | Bidirectional reachability, policy allow TCP/8080, policy drop TCP/9000, reverse direction | **PASS** | `functional/*`, `datapath-test.log` |
| 6 | Fail-closed: broken dataplane reports `apply_failed`/`degraded`, no generation committed, no direct leak, recovers | **PASS** | `failclosed-test.log`, `failclosed/*` |
| 7 | UI 11/11 pages in real Chromium, zero 404s, clean console | **PASS** | `ui/result.json`, `ui/*.png` |
| 8 | Evidence + acceptance document update, committed (not pushed) | **PASS** | this document, `evidence/20261025-fix-release-orch-regressions/` |

### Regression 1 — runtime status (fixed)

`handleRuntimeStatus` no longer consults the cached Gateway component inventory
or plan-operation evidence for the Orchestrator profile; every poll is answered
from `orchestratorStatusComponents` (the transparent VPP generation plus local
persistence). `statusHealthComponents` also filters by `profile.AllowsService`,
and `reprobeRuntimeComponents` is transparent-aware. Regression tests in
`backend/internal/httpapi/orchestrator_runtime_status_regression_test.go` cover
the post-apply component set, five polls from the post-restart state, the cached
reprobe (`no VPP operations rendered` before the fix) and the unchanged Gateway
inventory.

Observed on the deployed image:

- Immediately after `POST /runtime/apply` (`committed`/`running`, transaction
  `runtime-aae81c869410c69f776ff80cc0e4f456`) the status was `running` with
  exactly `vpp` + `persistence` and generation
  `fd3afd6b162faed4fe22188ab88cf55295ef096e4c742dad35d7bee9441b28e0`.
- 6 consecutive polls before an extra control-api restart and 6 consecutive
  polls after it: all `running`, same two components, same generation, no
  oscillation (`status-stability/*-summary.jsonl`, `*-poll-{1..6}.json`).
- After the fail-closed/recovery cycle, 5 more polls were `running`
  (`final/api-runtime-status-{1..5}.json`).

### Regression 2 — UI bundle (fixed)

The Orchestrator build profile now pins the accepted 11-page bundle
(`packaging/orchestrator-frontend/bundle`, `app.js` SHA-256 `0787685e…`,
source `08af368`). `build-rootfs.sh`/`build-upgrade-package.sh` default to it,
`validate_frontend_bundle`/`copy_product_frontend` and `build-disk-image.sh`
enforce the pin, and artifact manifests record the provenance. The Gateway
profile has no pin and its strict validation/controller-shell path is
unchanged.

Verified on the rebuilt artifacts and the deployed image:

| Artifact | `app.js` SHA-256 |
|---|---|
| rootfs | `0787685e8117c87e2e87a932c5e2f54e389ba668621c92bff65af00466199d35` |
| upgrade package | `0787685e8117c87e2e87a932c5e2f54e389ba668621c92bff65af00466199d35` |
| disk image / `/opt/ly-route/admin/app.js` | `0787685e8117c87e2e87a932c5e2f54e389ba668621c92bff65af00466199d35` |

The accepted 11-page sweep passed **11/11** pages in headless Chromium with
**0 × HTTP 404** and **0 unexpected console errors** (the single 401 is the
expected pre-auth `/api/v1/auth/session` probe). A build with the legacy
controller-shell bundle is rejected:
`Frontend bundle app.js sha256 mismatch for orchestrator: expected 0787685e…,
got 018563a2…`.

### Build identity

Artifact SHA-256 values are recorded in the evidence directory:
`build/SHA256SUMS`, `build/disk-image.manifest.json`,
`build/rootfs-artifact-manifest.json`, `build/ui-app-js.sha256`, and
`build/git.txt` (branch + HEAD). The image artifact manifest reports
`product=orchestrator` and `source_commit` equal to that HEAD; the pinned UI
`app.js` is
`0787685e8117c87e2e87a932c5e2f54e389ba668621c92bff65af00466199d35` in the
rootfs, the upgrade package and the deployed image.

Deployment: one fresh VM on datastore `data (1)` with 4 vCPU / 4 GB and three
vmxnet3 NICs (`VM Network` management, `lyacc-orch-pg` WAN,
`lyacc-orch-b-pg` LAN), created by
`/root/work/ly-route-acceptance-esxi/esxi/deploy-esxi-vm.sh`. The exact VM
name, management address, MACs and deployment log are recorded in the
evidence directory (`deploy.log`, `build/git.txt`). Previous acceptance VMs
connected to the same isolated data portgroups were powered off (not deleted)
for the run so they could not duplicate datapath traffic. Gateway VMs,
`dist-arch-v1-*`, `palog`, `DNS-8`, `vCLS` and
`/root/work/ly-route-acceptance-v1` were not touched.

### Verification commands

`cd backend && go build ./... && go vet ./...` and
`go test -p 1 -count=1 ./...` pass (`BUILD_OK`, `VET_OK`, `TEST_EXIT=0`).
`scripts/test-product-builders.sh` is not green on this host for a
pre-existing Gateway reason (`Gateway runtime env misses command: smartdns`,
reproduced at `7d0a9ab` in a clean worktree); the Orchestrator-specific
pipeline paths were validated directly: the pinned default build embeds
`0787685e…` in rootfs/upgrade/disk with provenance, and the legacy bundle is
rejected.

### Remaining gaps

- The upgrade package was verified statically (`includes_dataplane=false`,
  plugin and pinned UI present); upgrade/rollback was not executed.
- No throughput/line-rate claim in AF_PACKET acceptance mode.
- No orchestration-group (`via`) hop, IPv6 policy, or host reboot.
- Gateway Gate A/B and the Gateway product profile were not run here; the
  Gateway status path is covered by unit tests only.

## Historical (superseded): release build 506cc4c — merged Gateway + Orchestrator line (2026-10-25)

Run `20261025T0730Z-release`. This is the re-run of the Orchestrator acceptance
against the single release line `feat/release-integration` @ `506cc4cc6361`
(merge of the Gateway line `42da508` and the Orchestrator backend `e2c16ef`
plus `feat/orch-acceptance`). The build under test is
`/root/work/dist-release-orchestrator-506cc4cc6361` (rootfs, upgrade package,
disk image; SHA256SUMS re-verified independently). Data path is the same
explicit AF_PACKET acceptance mode (`hook=af_packet`,
`mode=linux_packet_socket`, `acceptance_only=true`,
`high_performance=false`). No performance claim is made.

### Verdict

| # | Requirement | Verdict | Evidence |
|---|---|---|---|
| 1 | Deploy release build; image/manifest `506cc4cc6361`, product `orchestrator` | **PASS** | `identity-*.txt`, `build-verification.txt` |
| 2 | `health` ok; password change/login; product profile correct | **PASS** | `health*.json`, `login*.json`, `password-change*.json`, `product.json` |
| 3 | AF datapath: VPP owns both NICs, `runtime/apply` committed, `runtime/status` running + generation | **FAIL (status)** | `af-env-and-proof.json`, `api-runtime-apply.json`, `api-runtime-status.json`, `status-after-restart-call1.json` … `call3.json` |
| 4 | Control-api restart and VPP restart keep the same generation and forwarding | **PASS** | `persistence/*`, `failclosed/fc-generation-*.txt` |
| 5 | Bidirectional reachability, policy allow TCP/8080, policy drop TCP/9000, reverse direction | **PASS** | `functional/*`, `final-functional.txt` |
| 6 | Fail-closed: broken dataplane reports `apply_failed`/`degraded`, no generation committed, no direct leak, recovers | **PASS** | `failclosed/*` |
| 7 | UI 11/11 pages in real Chromium, zero 404s, clean console | **FAIL as specified** | `ui/result.json`, `ui-release/result.json`, `ui-accepted-bundle-diagnostic/result.json` |
| 8 | Evidence + acceptance document update, committed (not pushed) | **PASS** | this document, `/root/work/orch-acceptance/evidence/20261025T0730Z-release/` |

Item 3 is a real regression in the merged line: the commit itself succeeds and
the datapath is correct, but `GET /api/v1/runtime/status` immediately after the
apply reports `degraded` (and adds gateway-only `smartdns`, `dnsmasq`, `xray`,
`pppoe` components) instead of `running` with the transparent generation. The
first status call after a control-api restart reports `running` with the
generation, but the second and later calls degrade again with
`vpp unavailable: no VPP operations rendered`. Root cause and exact evidence are
in `analysis-status-regression.md`.

Item 7 is a release-packaging regression, not a backend regression: the image
ships the 12-page source-tree controller-shell UI (`app.js` sha256
`018563a2…`) instead of the accepted 11-page Vite bundle (`08af368`,
`app.js` sha256 `0787685e…`) used by the e2c16ef acceptance. The shipped UI
passes 12/12 of its own pages, and the accepted 11-page bundle passes 11/11 when
overlaid as a diagnostic, but the 11 required pages are **not present in the
built image**. The e2c16ef image itself shipped an 11-page bundle
(`app.js` `dba17962…`), so the release build script dropped the UI overlay; see
`analysis-ui-bundle.md`.

### 1. Build identity and deployment

`sha256sum -c SHA256SUMS` in the release directory: all four artifacts OK.
The disk-image manifest and the in-image artifact manifest both report
`source_commit=506cc4cc6361`, `product=orchestrator`,
`frontend_product=orchestrator`, `database_path=/var/lib/ly-route/orchestrator/ly-route.db`.
The running control binary answers the `--version` preflight with
`506cc4cc6361` (the 506cc4c fix); `/health` reports the product version
`26.06+lyroute1`.

| Artifact | SHA-256 |
|---|---|
| control binary | `3d1017aa92f18e0022477012c28e95ff11d504e18b48da18030e27f89568a7d0` |
| VPP orchestrator plugin | `29a5617266f5109002b55fa100644bc6c67562dbe41c151978d7a3a5a668236a` |
| UI `app.js` (shipped) | `018563a2fa6c73664b7c28fc6a526aa2d83edc83915ebfadbca4b9094d2ed5f6` |
| UI `index.html` (shipped) | `b9adcc2d4f01561871f4307427ff7dd6ddf90c0bde48eacbdfb2e4dcd63914c3` |

The upgrade package declares `commit=506cc4cc6361`, `product=orchestrator`,
`includes_dataplane=false`, and carries the Orchestrator VPP plugin plus the
control binary/UI/runtime scripts (verified from its `manifest.json` and
`checksums.sha256`; see `build-verification.txt`). The upgrade path itself was
not exercised in this run.

Deployed VM: `lyacc-orch-506cc4c` on datastore `data (1)`, 4 vCPU / 4 GB, three
vmxnet3 NICs (`VM Network` mgmt `10.1.18.244`, `lyacc-orch-pg` WAN,
`lyacc-orch-b-pg` LAN). Existing VM `lyacc-orch-e2c16ef` was gracefully powered
off and kept; peer `lyacc-orch-peer` was powered on. No forbidden VM, port
group or datastore was touched.

### 2. Health, authentication, product profile

`GET /api/v1/health` → `{"status":"ok","version":"26.06+lyroute1","degraded":false}`
with `vpp` and `persistence` available. `GET /api/v1/product` → `orchestrator`
with the orchestrator capability set. The first login with the generated
first-boot password returned `password_change_required=true`; the password was
changed through `POST /api/v1/auth/change-password`, the old credential was
rejected with 401, and a fresh login/session succeeded. Evidence:
`identity-*.txt`, `health*.json`, `login*.json`, `password-change*.json`,
`auth-initial-password-rejected.json`, `product.json`.

### 3. AF_PACKET datapath and apply status

The runtime proof file records both data NICs as `tier=vpp_native`,
`hook=af_packet`, `mode=linux_packet_socket`, `acceptance_only=true`,
`high_performance=false`, driver `vmxnet3`:

```
ens224  00:50:56:aa:bd:02    ens256  00:50:56:aa:bd:03
```

After the topology/policy PUT and `POST /api/v1/runtime/apply`, VPP owns both
NICs (`lyroute-ens224`, `lyroute-ens256`, Linux PACKET socket interface v3) and
the transparent generation is
`fd3afd6b162faed4fe22188ab88cf55295ef096e4c742dad35d7bee9441b28e0`
(same digest as the e2c16ef acceptance for the same topology/policy):

```
state running
generation fd3afd6b162faed4fe22188ab88cf55295ef096e4c742dad35d7bee9441b28e0
boundary wan lyroute-ens224 lan lyroute-ens256
policy drop-9000 group-position 10 sequence 10 action drop packets 0 bytes 0
```

`POST /api/v1/runtime/apply` returned `committed` / `running` with transaction
`runtime-e715b30f8a2f4b42f4cd03aaa0f8576c` and vpp/persistence components
`running` + `available` (with the generation in the vpp reason).

**Regression:** the immediately following `GET /api/v1/runtime/status` returned
`status=degraded` and a component list of `smartdns`, `dnsmasq`, `xray`,
`pppoe` (all degraded because those gateway services do not exist on the
Orchestrator product), plus `vpp` and `persistence` running. After
`systemctl restart ly-route-control-api`, the **first** status call returned
`status=running` with exactly `vpp` + `persistence` and the generation reason
(`persistence/status-after-api.json`), but the second and third calls returned
`status=degraded` with `vpp unavailable: no VPP operations rendered`
(`status-after-restart-call1.json` … `call3.json`,
`analysis-status-oscillation.txt`) and the endpoint stayed degraded. This is a
status-reporting regression in the merged Gateway line, not a datapath failure.

### 4. Restart persistence

`systemctl restart vpp`: VPP comes back locked, the transparent recovery loop
re-attaches both AF_PACKET interfaces and re-commits in ~2 s with the **same**
generation; ICMP across the path succeeds immediately afterwards.
`systemctl restart ly-route-control-api`: startup reconcile re-applies the
persisted topology/policy, the generation is unchanged, and forwarding
continues. Evidence: `persistence/restart-persistence-test.log`,
`persistence/status-*.json`, `persistence/vpp-after-*.txt`,
`persistence/ping-after-*.txt`, `failclosed/fc-generation-*.txt`.

### 5. Functional results

Peer: `netns lan` = 10.99.0.3/24 (client), `netns wan` = 10.99.0.2/24 (server),
plus a reverse listener on 10.99.0.3:8082.

| Check | Result |
|---|---|
| ICMP LAN→WAN | 5/5, RTT 8.8–27.4 ms |
| ICMP WAN→LAN | 5/5, RTT 9.7–15.8 ms |
| TCP/8080 (default direct) | HTTP 200, 1 553 704 B in 20 s (slow AF_PACKET path) |
| TCP/9000 (drop rule, listener up) | timeout, `curl rc=28`; VPP policy counter `packets 3 bytes 222`; node `packets dropped by orchestration policy` 6 |
| Reverse TCP 8082 | HTTP 200 |
| Informational 2 MiB download | 2 097 152 B / 23.47 s = 89 357 B/s (≈0.71 Mbit/s, not a claim) |

Interface counters moved on both legs and the final functional re-check after
the fail-closed/restart cycle still passes. Evidence: `functional/*`,
`final-functional.txt`.

### 6. Fail-closed status semantics

The dataplane was broken by stopping VPP (`systemctl stop vpp`), then:

- `POST /api/v1/runtime/apply` → HTTP 202, `status=apply_failed`,
  `runtime_state=degraded`, reason from the transparent runtime
  (`dataplane_locked: failed prerequisites …`). No handler-level
  `dataplane_locked` status was returned.
- `GET /api/v1/runtime/status` → `status=degraded`; both `vpp` and
  `persistence` unavailable with `connect: Connection refused`.
- The transparent transaction journal
  `/var/lib/ly-route/orchestrator/transparent-transaction.json` was absent, and
  no new generation was committed (after restore the generation was still
  `fd3afd6b…`).
- No direct leak: LAN→server ping 100% loss, TCP/8080 timed out; the host had
  no bridge and no route for 10.99.0.0/24 over the data NICs.
- `/api/v1/config/apply` (async) did **not** return 423/`dataplane_locked`: it
  accepted the request (202, `apply_in_progress`) and the async transaction
  terminated as `apply_failed` / `runtime_state=degraded` (HTTP status 422).
  This is the Gateway-line non-fatal capability probe behaviour for the
  Orchestrator profile; the Gateway profile itself was not part of this run.
- After `systemctl start vpp`, the same generation `fd3afd6b…` returned,
  `POST /runtime/apply` committed again, and forwarding was restored.

Evidence: `failclosed/failclosed-test.log`, `fc-apply-broken.*`,
`fc-status-broken.*`, `fc-no-generation.txt`, `fc-ping-broken.txt`,
`fc-tcp-broken.txt`, `fc-host-broken.txt`, `fc-config-apply-*`,
`fc-generation-verdict.txt`, `fc-apply-restored.*`.

### 7. UI acceptance

The release image serves the 12-page source-tree controller-shell
(`frontend/orchestrator`): 系统概况, 流量概况, 在线用户, Top连接, 网卡设置,
编排设置, 流量编排, 流量控制, 安全控制, IP管理, 系统用户管理, 配置管理.
The release sweep navigated every sidebar page in real headless Chromium:
**12/12 passed, 0 × HTTP 404, 0 unexpected console errors** (the only 401 is
the expected pre-auth session probe). Evidence: `ui-release/result.json` and 12
PNG screenshots.

The **required 11-page UI is not shipped**. Running the accepted
`ui-sweep.mjs` (11-page routes/pages) against the built image fails 0/11: the
bundle has no `/topology`, `/transparent`, `/runtime`, `/upgrades`, … pages and
its relative asset paths produce `/system/*` 404s when the harness deep-links.
As a diagnostic only, the accepted `08af368` bundle was copied into
`/opt/ly-route/admin` (the shipped bundle was backed up first): it then passed
**11/11** with 0 × 404 and the expected single pre-auth 401, proving the release
backend supports the accepted UI. The shipped bundle was restored and verified
(`app.js` `018563a2…`). This overlay result is **not** a pass for the built
image; it isolates the failure to packaging. Evidence: `ui/result.json` (0/11),
`ui-accepted-bundle-diagnostic/result.json` (11/11),
`ui-accepted-bundle-overlay.txt`, `ui-accepted-bundle-restored.txt`.

### 8. Regressions versus the e2c16ef acceptance

1. **`/api/v1/runtime/status` after an apply is degraded.** The merged
   `handleRuntimeStatus` short-circuits: immediately after an apply it uses
   `statusHealthComponents` (which does not filter by product profile, so the
   Orchestrator reports absent gateway services); after a restart the first
   call rebuilds the plan and is correct, but subsequent calls re-probe the
   cached inventory and `vppRuntimeComponent` returns
   `unavailable: no VPP operations rendered` for the transparent runtime. This
   is a P1 reporting regression; the datapath and apply commit are unaffected.
2. **The release image ships the wrong UI bundle.** e2c16ef image: 11-page
   bundle (`app.js` `dba17962…`); accepted overlay: 11-page `08af368`
   (`0787685e…`); release image: 12-page controller-shell
   (`018563a2…`). The release build script does not pass `--frontend-bundle`,
   so `build-rootfs.sh` defaults to `build-controller-shell.sh`. The 11-page
   UI contract in the acceptance plan is therefore not met by the built image.
3. Not regressions: generation, restart recovery, functional policy and
   fail-closed semantics match the e2c16ef results (same generation digest for
   the same topology/policy).

### 9. Gaps

- Gateway-product `/api/v1/config/apply` semantics were not exercised here
  (only the Orchestrator profile); the Gateway line has its own acceptance.
- Upgrade/rollback of this build was not executed; the upgrade package was
  verified statically (`includes_dataplane=false`, plugin present).
- Orchestration-group (`via`) hops, IPv6 policy and host reboot remain
  unexercised.
- AF_PACKET acceptance mode is not a performance claim; the observed
  ~0.71 Mbit/s is informational only.

### Evidence

`/root/work/orch-acceptance/evidence/20261025T0730Z-release/` — `summary.json`
(machine-readable verdicts), `README.md` (file index), `deploy.log`, API dumps,
VPP/host dumps, functional logs, fail-closed logs, UI results and screenshots,
and `analysis-status-regression.md` / `analysis-ui-bundle.md`. The
status-regression sequence is in `api-runtime-status.json`,
`status-after-restart-call1.json` … `call3.json` and
`analysis-status-oscillation.txt`.

## Historical record: e2c16ef acceptance (2026-10-24)

Run `20261024T1530Z-orch-af`. This is a functional and UI acceptance of the
**Orchestrator product** only; Gateway results are never substituted for it.
The data path runs in the product's explicit **AF_PACKET acceptance mode**
(`hook=af_packet`, `mode=linux_packet_socket`, `acceptance_only=true`,
`high_performance=false`). Nothing here is a line-rate, 1G or 10G claim.

## Verdict

| Requirement | Result | Evidence |
|---|---|---|
| VPP drives both data NICs in AF_PACKET mode | **PASS** | `vpp-and-host.txt`, `api-*`, capability proof |
| `POST /api/v1/runtime/apply` commits | **PASS** (`committed` / `running`) | `api-runtime-apply.json` |
| `/api/v1/runtime/status` committed with non-empty generation, vpp + persistence running/available | **PASS** | `api-runtime-status.json` |
| Transparent generation visible in VPP | **PASS** | `show ly-route orchestrator` in `vpp-and-host.txt` |
| Survives control-api restart | **PASS** (same generation) | `persist-status-after-api.json`, `persist-vpp-after-api-restart.txt` |
| Survives VPP restart | **PASS** (auto-recovered in ~2 s) | `persist-vpp-after-restart.txt`, `persist-ping-after-vpp-restart.txt` |
| Bidirectional reachability across the transparent path | **PASS** (ICMP 5/5 both ways) | `fn-icmp.txt` |
| Policy positive case | **PASS** (TCP 8080 HTTP 200) | `fn-tcp-8080.txt` |
| Policy negative case | **PASS** (TCP 9000 dropped while a listener is up; rule counter increments) | `fn-tcp-9000.txt`, `fn-vpp-after.txt` |
| Reverse-direction TCP | **PASS** (WAN client → LAN server 8082, HTTP 200) | `fn-reverse-8082.txt` |
| UI: 11 pages in real Chromium | **PASS** 11/11 | `ui/result.json`, `ui/*.png` |
| UI console: no raw 404s | **PASS** (0 × 404; one expected pre-auth 401 session probe) | `ui/result.json` |
| `go build ./...` | **PASS** | `regression-tests.txt` |
| Targeted orchestrator tests | **PASS** | `regression-tests.txt` |
| Product code changes needed | **none** | this branch adds docs + the acceptance harness only |

Pre-existing, unrelated failures in the shared `internal/runtime/vpp` package
(16 gateway-path tests, stale command-string expectations) are recorded in
`regression-tests.txt`; see [Regression](#regression) for the exact list and why
they are not part of this acceptance.

## 1. What was tested

The Orchestrator is an inline (bump-in-the-wire) transparent policy node: two
data ports are the WAN/LAN boundary and the VPP plugin `ly-route-orchestrator`
forwards frames between them, evaluating ordered IP/port policy in the
client-to-server orientation (`runtime/vpp-orchestrator/.../node.c`).

```
   netns "lan"  10.99.0.3/24                         netns "wan"  10.99.0.2/24
   (peer VM)  ── ens256 ── [ lyacc-orch-b-pg ] ── ens256 ─┐
                                                          │ lyacc-orch-e2c16ef
                                                          │  VPP AF_PACKET
   (peer VM)  ── ens224 ── [ lyacc-orch-pg  ] ── ens224 ─┘  (generation fd3afd6b…)
```

The peer is a real VM (`lyacc-orch-peer`, 10.1.18.204) with two network
namespaces, one per isolated data leg. Every frame crosses a vSwitch and the
VPP data plane; nothing is generated inside the appliance.

## 2. Environment and fixtures

| Item | Value |
|---|---|
| vCenter / host | `https://10.1.18.110` / `10.1.18.121` |
| Orchestrator VM | `lyacc-orch-e2c16ef`, datastore `data (1)`, 4 vCPU / 4 GB |
| Management | `10.1.18.203/24` on `ens192` (00:50:56:aa:bb:31) |
| Data NICs | `ens224` 00:50:56:aa:bb:32 (`lyacc-orch-pg`, WAN) and `ens256` 00:50:56:aa:bb:33 (`lyacc-orch-b-pg`, LAN) |
| Product image | `/root/work/dist-orchestrator-e2c16ef/…` (tag `orchestrator-rc-e2c16ef`) |
| Backend source | `feat/orchestrator-backend` @ `e2c16ef` (product mainline) |
| Binary | `/usr/lib/ly-route/ly-route-control` sha256 `fcd8ee51f4e2d3e49993b65eb09bb4438f2d88e35966a6d62c288ee43dd5b0db` |
| UI bundle | commit `08af368`, `app.js` sha256 `0787685e8117c87e2e87a932c5e2f54e389ba668621c92bff65af00466199d35` |
| Peer VM | `lyacc-orch-peer` on `data (1)`, Debian 12 cloud image, 3 vmxnet3 NICs |
| Data legs | `lyacc-orch-pg` and `lyacc-orch-b-pg`, both isolated vSwitches without uplinks |

### Lab changes made for this acceptance (additive, reversible)

1. Orchestrator VM: one extra vmxnet3 NIC (`00:50:56:aa:bb:33`) on the new
   `lyacc-orch-b-pg`. The VM shipped with a single data NIC; the transparent
   profile requires two distinct boundary ports.
2. Host `10.1.18.121`: new isolated vSwitch `lyacc-orch-b` and port group
   `lyacc-orch-b-pg` (no uplink, permissive security policy). No existing
   switch, port group or VM was modified.
3. New disposable peer VM `lyacc-orch-peer` (10.1.18.204) on `data (1)`.
4. Orchestrator VM `/etc/ly-route/runtime.env`: the two AF_PACKET acceptance
   lines below (backup kept as `/root/runtime.env.bak-*`).
5. Orchestrator control DB: the `auth_users.admin` hash row was reset to a
   freshly generated lab password (`/root/work/esxi-acceptance/.secrets/orch-admin.pass`,
   root-only) because the previous credential was not recorded. This is a lab
   recovery step, not a product change.

Not touched: `palog`, `DNS-8`/`dns8`, `vCLS`, any cluster-control VM, and the
Gateway acceptance VMs (`lyacc-gw-*`, `lyacc-lan-client`, `lyacc-wan-peer`).
No shared infrastructure was rebooted.

## 3. AF_PACKET bring-up

The exact switch is the one the product code reads (not a guess):

- `packaging/rootfs-overlay/usr/lib/ly-route/runtime-check.sh` probes a
  temporary AF_PACKET host-interface only when
  `LY_ROUTE_VMXNET3_AF_PACKET_ACCEPTANCE=true` and the driver is `vmxnet3`,
  and records `acceptance_only=true`, `high_performance=false`.
- `backend/internal/runtime/vpp/selection.go` (`LoadNativePathRequest`) reads
  the same variable and `acceptanceAFPacketProof()` accepts the proof only when
  that flag is set, the proof is acceptance-only, and the driver is
  `vmxnet3`/`e1000`.
- Both the probe service and the control API consume `/etc/ly-route/runtime.env`.

Applied on the appliance:

```
LY_ROUTE_VMXNET3_AF_PACKET_ACCEPTANCE=true
LY_ROUTE_VPP_DATA_INTERFACES=ens224,ens256
# already present in /etc/ly-route/control-api.env:
LY_ROUTE_MANAGEMENT_INTERFACE=ens192
```

```bash
systemctl restart ly-route-runtime-check.service
systemctl restart ly-route-control-api.service
```

Capability proof (`/var/lib/ly-route/vpp-native-capabilities.json`):

```json
{"management_interface":"ens192","proofs":[
 {"linux_interface":"ens224","candidates":[{"tier":"vpp_native","hook":"af_packet",
  "mode":"linux_packet_socket","runtime_verified":true,"native":true,
  "high_performance":false,"acceptance_only":true,"kernel_driver":"vmxnet3",
  "mac_address":"00:50:56:aa:bb:32"}]},
 {"linux_interface":"ens256","candidates":[{"tier":"vpp_native","hook":"af_packet",
  "mode":"linux_packet_socket","runtime_verified":true,"native":true,
  "high_performance":false,"acceptance_only":true,"kernel_driver":"vmxnet3",
  "mac_address":"00:50:56:aa:bb:33"}]}]}
```

Before the topology was committed VPP had only `local0` and
`show ly-route orchestrator` was `state locked` (`baseline-vpp.txt`,
`baseline-journal.txt`): the earlier `degraded` status was honest.

After the topology PUT the VPP data plane owned both NICs:

```
$ vppctl show interface
lyroute-ens224   1  up  9000/0/0/0
lyroute-ens256   2  up  9000/0/0/0

$ vppctl show hardware-interfaces lyroute-ens256
  Ethernet address 00:50:56:aa:bb:33
  Linux PACKET socket interface v3
  Link speed: unknown
```

`lyroute-<linux-if>` is created by `DataplaneAttachOperation`
(`backend/internal/runtime/vpp/adapter.go`) with `?create host-interface name
<linux-if> cksum-gso-disable`, renamed, given the physical MAC and brought up.
The tolerant `?` prefix makes the attach idempotent across repeated applies and
startup reconcile.

## 4. Configuration through the public API

The Orchestrator profile is single-site: its target model is exactly
`PUT /api/v1/orchestrator/topology` (management interface, the logical LAN/WAN
interface pair and optional orchestration groups) plus
`PUT /api/v1/orchestrator/policy`; there are no separate site or network
resources to create. No files were edited to create the topology. Sequence
(admin session cookie):

```bash
curl -sk -c cookies -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<lab>"}' https://10.1.18.203/api/v1/auth/login

curl -sk -b cookies -X PUT -H 'Content-Type: application/json' -d @topology.json \
  https://10.1.18.203/api/v1/orchestrator/topology
# 200 {"checksum":"bf358a50…"}

curl -sk -b cookies -X PUT -H 'Content-Type: application/json' -d @policy.json \
  https://10.1.18.203/api/v1/orchestrator/policy
# 200 {"checksum":"ef06f111…"}
```

`topology.json`:

```json
{"schema_version":1,"management_interface":"ens192",
 "interfaces":[{"name":"wan0","role":"wan","port":"ens224"},
               {"name":"lan0","role":"lan","port":"ens256"}],
 "orchestration_groups":[]}
```

`policy.json` (client = LAN, server = WAN; drop TCP/9000 to the server):

```json
{"schema_version":1,
 "ip_objects":[{"id":"wan_server","prefixes":["10.99.0.2/32"]}],
 "policy_groups":[{"id":"g1","position":10,"rules":[
   {"id":"drop-9000","sequence":10,
    "match":{"sources":["any"],"destinations":["wan_server"],"protocol":"tcp",
             "destination_ports":[{"start":9000,"end":9000}]},
    "action":{"kind":"drop"}}]}],
 "default":{"kind":"direct"}}
```

The control-plane compiler agrees with the intent before it is programmed:

```
port 8080 -> lan []
port 9000 -> drop [{'policy_group': 'g1', 'rule_id': 'drop-9000', 'sequence': 10, 'action': 'drop'}]
```

Runtime apply and status:

```json
POST /api/v1/runtime/apply -> {"status":"committed","runtime_state":"running",
  "transaction_id":"runtime-ff564e771c6fc35d944737daa2142160"}

GET /api/v1/runtime/status -> {"status":"running",
  "components":[
    {"name":"vpp","state":"running","available":true,
     "reason":"transparent VPP generation fd3afd6b… verified"},
    {"name":"persistence","state":"running","available":true,
     "reason":"persisted topology and policy are available"}],
  "last_apply":{"status":"committed","runtime_state":"running"}}
```

`GET /api/v1/health` → `{"status":"ok", … "vpp":"available","persistence":"available"}`.

## 5. Generation proof and restart persistence

```bash
$ vppctl show ly-route orchestrator
state running
generation fd3afd6b162faed4fe22188ab88cf55295ef096e4c742dad35d7bee9441b28e0
boundary wan lyroute-ens224 lan lyroute-ens256
default direct
policy drop-9000 group-position 10 sequence 10 action drop packets 3 bytes 222
```

The generation is the SHA-256 of the persisted topology + policy, so a
re-apply of unchanged state reproduces the same value (verified on every
restart below).

**control-api restart** (`systemctl restart ly-route-control-api`):
`reconcileTransparentStartup` re-applies the persisted state; the process
serves again with `transaction_id=orchestrator-startup`, the **same**
generation, `status=committed`, and the datapath still forwards
(`persist-ping-after-api-restart.txt`).

**VPP restart** (`systemctl restart vpp`): VPP comes back with
`state locked`; `runTransparentRecovery` (2 s interval) re-attaches the
AF_PACKET interfaces and re-commits in **~2 s**, with the same generation.
ICMP across the path works immediately afterwards
(`persist-vpp-after-restart.txt`, `persist-ping-after-vpp-restart.txt`).

## 6. Functional results

Peer: `netns lan` = 10.99.0.3/24 (client), `netns wan` = 10.99.0.2/24 (server),
plus a reverse listener on 10.99.0.3:8082.

| Check | Command (peer) | Result |
|---|---|---|
| ICMP LAN→WAN | `ip netns exec lan ping -c 5 10.99.0.2` | 5/5, RTT 8.8–14.8 ms |
| ICMP WAN→LAN | `ip netns exec wan ping -c 5 10.99.0.3` | 5/5, RTT 9.3–15.5 ms |
| Positive: TCP/8080 | `ip netns exec lan curl http://10.99.0.2:8080/blob.bin` | HTTP 200, 960 024 B in 20 s (curl deadline) |
| Negative: TCP/9000 | `ip netns exec lan curl -m 6 http://10.99.0.2:9000/index.html` | timeout (`rc=28`), listener was up |
| Reverse TCP | `ip netns exec wan curl http://10.99.0.3:8082/index.html` | HTTP 200 |
| Informational throughput | 2 MiB download over the path | 2 097 152 B / 37.55 s = **55 852 B/s ≈ 0.45 Mbit/s** |

The negative case is a policy drop, not a closed port: the peer had
`10.99.0.2:9000` listening (`fn-listeners.txt`), the client timed out rather
than receiving a reset, and VPP recorded the match:

```
policy drop-9000 group-position 10 sequence 10 action drop packets 3 bytes 222
ly-route-orchestrator  packets dropped by orchestration policy  12
```

Interface counters moved on both legs (`fn-vpp-after.txt`): `lyroute-ens224`
rx 2102 / tx 2345, `lyroute-ens256` rx 2348 / tx 2099 during the run.

**Policy orientation (important):** rules are canonicalised to the
client→server direction, where **LAN ingress is the request direction**
(`node.c`: WAN ingress is the return tuple and is reversed before matching).
A deployment whose clients sit on the WAN side must express rules with
source/destination mirrored. This acceptance proves the supported orientation.

## 7. UI acceptance

Served bundle `08af368` (`app.js` sha256 `0787685e…`) on `https://10.1.18.203`,
real Chromium (headless, HTTPS errors ignored only for the lab certificate),
authenticated as `admin`, sidebar navigation per page, 3 s settle, full-page
screenshot, console + response capture.

| # | Page | Path | H1 | Result |
|---|---|---|---|---|
| 1 | 单机总览 | `/` | 单机总览 | PASS |
| 2 | 单机拓扑 | `/topology` | 单机拓扑 | PASS |
| 3 | 编排组 | `/groups` | 编排组 | PASS |
| 4 | 流量编排 | `/policy` | 流量编排 | PASS |
| 5 | 透明编排 | `/transparent` | 透明编排 | PASS |
| 6 | 遥测 | `/telemetry` | 遥测 | PASS |
| 7 | 运行态 | `/runtime` | 运行态 | PASS |
| 8 | 配置与快照 | `/config` | 配置与快照 | PASS |
| 9 | 升级 / 回滚 | `/upgrades` | 升级 / 回滚 | PASS |
| 10 | 产品转换 | `/conversions` | 产品转换 | PASS |
| 11 | 用户管理 | `/system/users` | 用户管理 | PASS |

Console / network (machine-readable in `ui/result.json`):

- **0 × HTTP 404** and no page shows raw 404/`Not Found` text.
- 1 × console error: `GET /api/v1/auth/session → 401` **before** login. That is
  the session probe returning the correct "unauthenticated" semantic; it is not
  a page error and disappears after login.
- **No gateway-only endpoint is requested at all.** The sweep issued 19
  `/api/v1/...` calls, all product/orchestrator-scoped
  (`/product`, `/health`, `/runtime/status`, `/orchestrator/{topology,policy,
  conversions,upgrades,operations/current}`, `/telemetry/*`, `/interfaces`,
  `/management/network`, `/dashboard/summary`, `/config/snapshots`,
  `/auth/*`). `/dhcp/*`, `/dns/*` and `/firmware/update/*` are never called by
  the Orchestrator profile, so the DHCP/DNS/firmware panels are hidden rather
  than broken.
- The runtime page shows `vpp: 运行中` and `persistence: 运行中` with generation
  `fd3afd6b…`; the overview shows LAN `ens256`, WAN `ens224`, 1 policy group /
  1 rule and the applied generation.

The earlier acceptance note ("UI console 404s") was **not reproducible** with
the currently served bundle: those resources are not referenced by any page
query in this profile.

## 8. Regression

On `feat/orch-acceptance` (base `e2c16ef`, no product code changed):

```
cd backend && go build ./...                 -> BUILD_OK
go test ./internal/orchestrator/... ./internal/orchestratorapi/... ./orchestrator/...
                                             -> ok (4 packages)
go test ./internal/runtime/vpp/ -run 'TransparentOrchestrator|TransparentBond|…'
                                             -> 12/12 PASS
go test ./internal/httpapi/ -run Orchestrator -> 62 PASS / 0 FAIL (incl. subtests)
```

Known baseline noise, **not introduced here and not fixed here**: the shared
`internal/runtime/vpp` package has 16 failing gateway-path tests at `e2c16ef`
(e.g. `TestBuildOperationsIncludesRoutePolicyAndSecurityACL`,
`TestRoutePolicyCatchAllPreservesLocalLAN`,
`TestBuildOperationsAddsGatewayDNSTransparentInterception`). Sampled output
shows stale expectations: the tests still assert the legacy
`ip route add table …` command text while the compiler emits
`?ip route add <prefix> table …`. They belong to the Gateway route/NAT/ACL
compiler and do not touch the transparent orchestrator. The full list and
output are in `evidence/…/regression-tests.txt`.

## 9. Findings and limitations

1. **Policy orientation is LAN→WAN.** See §6. A WAN-side client cannot express
   `destination_ports` directly; it must mirror the rule. Documented, not
   changed (changing it would be a product semantics change).
2. **AF_PACKET is slow.** ~0.45 Mbit/s and ~12 ms RTT in this lab. The product
   itself marks the path `acceptance_only=true`, `high_performance=false`.
   No performance claim is made beyond the labelled informational number.
3. **`af-packet-input` "timed out block" errors** appear continuously in this
   mode; they are the expected interrupt-mode artefact and do not indicate
   packet loss (counters and traffic agree).
4. **Idempotent attach matters.** Repeated `PUT` (topology then policy),
   control-api restart and VPP restart all re-run the attach; the tolerant
   `?create host-interface` / `?set interface name` commands make this safe.
5. **UI wording:** the overview states the transparent mode is an "L3 service
   chain, not L2 bridging". The proven data path forwards frames between the
   two boundary ports (L2 cross-connect) and matches on IP/port policy. Worth a
   copy review, not an acceptance blocker.
6. **Not exercised:** orchestration-group hops (`via`) — they need a second
   data-port pair; IPv6 policy path; upgrade/rollback of the transparent
   generation; host reboot.
7. The appliance clock lags the control host by ~2 minutes; all timestamps in
   this document are UTC from the control host, product timestamps come from
   the appliance.

## 10. Reproduce

```bash
export ORCH_HOST=10.1.18.203 ORCH_SSH=root@10.1.18.203
export PEER_SSH=root@10.1.18.204
export SSH_KEY=/root/work/esxi-acceptance/.secrets/acc_ed25519
export ORCH_PASS_FILE=/root/work/esxi-acceptance/.secrets/orch-admin.pass
scripts/acceptance/orchestrator-esxi/datapath-test.sh
scripts/acceptance/orchestrator-esxi/restart-persistence-test.sh
ORCH_BASE=https://10.1.18.203 ORCH_OUT=./ui \
  node scripts/acceptance/orchestrator-esxi/ui-sweep.mjs
```

The scripts assume the fixture in §2 (two isolated legs, peer in two
namespaces) and do not create or destroy VMs.

## Evidence

`/root/work/orch-acceptance/evidence/20261024T1530Z-orch-af/` — see its
`README.md` for the file index, `summary.json` for the machine-readable
verdict, `ui/result.json` + 11 screenshots for the UI, and
`regression-tests.txt` for the build/test transcript.
