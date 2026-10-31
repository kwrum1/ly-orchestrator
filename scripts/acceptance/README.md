# Orchestrator acceptance

Two passes run against the lab appliance (10.1.18.234):

1. **User journey** (U0–U6): the flows a real operator performs from a clean
   console. This is the gate for a release. The executed evidence for 0.2.7 is
   under `/root/work/acceptance/` on the build host (per-scenario scripts:
   `u0u1`, `u1c`, `u2`, `u3`, `u4`, `u5`, `u6`).
2. **Full functional matrix** (O0–O6): the repeatable harness in this directory.

## Run

```sh
node scripts/acceptance/orchestrator-gate.mjs
```

Environment:

| Variable | Default |
| --- | --- |
| `ORCH_BASE` | `https://10.1.18.234` |
| `ORCH_USER` / `ORCH_PASS` | `admin` / `LabOrch2026!` |
| `SSH_HOST` / `SSH_USER` / `SSH_PASS` | `10.1.18.234` / `root` / `Iamyours..123` |
| `PLAYWRIGHT_PATH` | `/root/work/ui-test/node_modules/playwright` |
| `EXPECT_VERSION` | `0.2.7` |
| `ACCEPT_DIR` | `/root/work/acceptance` |

Requires `sshpass`, `ssh`, Playwright and network access to the appliance. The
script writes `results.json`, API snapshots, `vppctl` dumps and screenshots
under `ACCEPT_DIR/<timestamp>/`.

## Case list

| ID | Case |
| --- | --- |
| O0.1 | login, health, version |
| O0.2 | frozen baseline shell (`app.js` 985377 bytes) |
| O0.3 | console renders without JS errors |
| O1.1 | role options LAN / WAN / 镜像口 / 未配置 |
| O1.2 | role switch writes `topology.port_roles` |
| O1.3 | second mirror port rejected with the device message |
| O1.4 | compatibility (af_packet) bond pre-check |
| O1.5 | bond delete removes orphaned `bond-*` interface documents |
| O1.6 | bond delete blocked while the topology still uses it |
| O1.7 | control-api restart keeps roles/groups/mirror |
| O2.0 | mirror page has no status card and only 添加镜像策略 |
| O2.1 | native single-source select, output limited to the mirror port |
| O2.2 | add/edit/delete policy, direction, SPAN readback, 已生效 |
| O2.3 | add button disabled without a mirror port |
| O3.1 | group create with both bypass switches, 20s window, prefill |
| O3.2 | link down bypasses only that group, link up recovers |
| O3.3 | business bypass semantics (ENV_LIMIT when no traffic source) |
| O3.4 | deleting a group removes its bypass entry |
| O4.1 | ordered rules, IP object reference, apply, compile hit |
| O4.2 | restore original policy |
| O5.1 | dashboard/trend live data, degraded false |
| O5.2 | no degraded during an apply window |
| O5.3 | 2.5s collector cache |
| O5.4 | 1280/1600/1920 rendering |
| O6.1 | installed 0.2.7 version + health |
| O6.2 | UI artifacts match the built package |
| O6.3 | service set audit, no failed units, no heal errors |
| O6.4 | repeated applies stay clean |

Result classes: `PASS`, `PRODUCT_FAIL`, `FIXTURE_FAIL`, `ENV_LIMIT`.
`ENV_LIMIT` is used only when the lab cannot exercise the case safely (no
traffic source behind a temporary group, forced VPP failure, reboot in the
middle of a shared run).
