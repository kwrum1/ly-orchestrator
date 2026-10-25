# Orchestrator ESXi acceptance harness

Scripts used for the Orchestrator transparent acceptance in AF_PACKET mode
(`docs/ORCHESTRATOR-ACCEPTANCE.md`). They run against an existing appliance and
peer fixture; they do not create or destroy VMs.

Fixture:

- appliance with the transparent topology committed (WAN + LAN boundary ports),
- one peer machine with two network namespaces, one per isolated data leg,
- `LY_ROUTE_VMXNET3_AF_PACKET_ACCEPTANCE=true` and the two data interfaces in
  `/etc/ly-route/runtime.env` (the product marks the path `acceptance_only`).

Environment:

```bash
export ORCH_SSH=root@10.1.18.203
export PEER_SSH=root@10.1.18.204
export SSH_KEY=/path/to/acceptance_ed25519
export ORCH_API=https://10.1.18.203
export ORCH_PASS_FILE=/path/to/orch-admin.pass
export PEER_WAN_IF=ens224 PEER_LAN_IF=ens256
export CLIENT_IP=10.99.0.3 SERVER_IP=10.99.0.2   # same L2, client on the LAN leg
export ORCH_OUT=/root/work/orch-acceptance/evidence/$(date -u +%Y%m%dT%H%M%SZ)
```

Run:

```bash
./datapath-test.sh              # ICMP both ways, policy allow/drop, counters, throughput (informational)
./restart-persistence-test.sh   # VPP restart and control-api restart re-readback
./failclosed-test.sh            # stop VPP: honest apply_failed/degraded, no commit, no leak, recovery
ORCH_BASE=$ORCH_API ORCH_OUT=$ORCH_OUT/ui node ui-sweep.mjs
```

Policy rules are canonicalised to the client→server direction, where LAN
ingress is the request direction. Placing the client on the WAN side requires
mirroring source and destination in the rule.

## Fail-closed check

`failclosed-test.sh` deliberately stops VPP and asserts that
`POST /api/v1/runtime/apply` reports `apply_failed` / `runtime_state=degraded`
(not a handler-level `dataplane_locked`), that `/api/v1/runtime/status` is
degraded, that no transparent transaction journal remains, that traffic does
not leak through the appliance, and that the same generation returns after
VPP is restarted. It also calls `/api/v1/config/apply` while broken to confirm
the Gateway-line non-fatal capability probe (no 423 `dataplane_locked`).

## UI bundles

`ui-sweep.mjs` targets the accepted 11-page Vite bundle (`08af368`, absolute
`/app.js` / `/styles.css`, per-page URLs). This is what the Orchestrator
product image ships by default: `scripts/build-rootfs.sh` and
`scripts/build-upgrade-package.sh` use the pinned bundle in
`packaging/orchestrator-frontend/bundle` (see
`packaging/build-profiles/orchestrator.json` and
`packaging/orchestrator-frontend/README.md`).

`ui-sweep-release.mjs` targets the legacy 12-page source-tree controller-shell
bundle (`frontend/orchestrator`, sidebar `[data-page]` navigation, no per-page
URLs). It is produced only by an explicit
`scripts/build-controller-shell.sh --product orchestrator` invocation, which
the product pipeline no longer uses; the 506cc4c regression image shipped that
bundle. Use it only when testing such an explicitly built bundle.
