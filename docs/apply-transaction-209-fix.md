# 209 clean apply transaction fix

Branch: `feat/apply-order-209` (based on `209c51ebd2ab`).

## Root causes

1. `routes`/`vpp.dns-service.network` resolved a `pppoe-runtime:<peer>`
   placeholder before the PPPoE helper had published its status contract on a
   clean install. The helper was only started in the post-gateway service pass.
2. VPP command execution read the status file once and failed immediately when
   it was missing; the boot `vpp-apply` replay did not resolve the placeholder
   at all.
3. The Linux policy-routing artifact waited 60s twice for a PPPoE underlay
   (proxy + DNS) and then hard-failed, exceeding the 2-minute service apply
   budget on a clean boot.
4. Rollback cleanup deleted the native attachment (and its VPP interface)
   before `vpp.lan-control-lcp.rollback-delete` detached ABF policy 65000. VPP
   then reported `show abf attach: unknown input` and the policy survived,
   producing `deleted policy 65000 remains attached or configured`.
5. VPP prints interface identity mappings in `show nat44 static mappings`; the
   snapshot parser rejected them, so an unchanged re-apply tried to repair NAT
   instead of being a no-op.
6. An expired native capability proof was reused for up to 15 minutes, so a
   fresh apply failed `fresh_runtime_proof`.

## Fix

- Start PPPoE helper artifacts in the pre-gateway bootstrap pass (the same
  artifacts are replayed after the gateway commit for authoritative evidence).
  `splitRuntimeArtifactsByPhase` keeps Linux policy routing in the pre pass only
  when it does not depend on a live runtime PPPoE underlay.
- `vppctlChannel` waits (bounded, env-tunable) for a delayed status file and
  for the session interface to exist before resolving `pppoe-runtime:` tokens.
  It never reads a status contract that is absent and never sends an unresolved
  placeholder to VPP.
- The boot `ly-route-vpp-apply` replay resolves the same placeholders and waits
  for status + interface.
- Linux `wait_vpp_underlay` treats a runtime PPPoE underlay as ready when the
  session interface exists (connecting is enough) and defers underlay-dependent
  DNS routing instead of aborting/failing readback.
- Supplemental cleanup is emitted in reverse dependency order; LCP/ABF cleanup
  runs before native attachment deletion. Unknown-ingress attachment output is
  treated as detached and a policy-id-only delete is tried as a fallback.
- NAT44 snapshots accept `identity mapping <address> [vrf <n>]` rows.
- Native capability proofs are reused only within `LY_ROUTE_VPP_PROOF_TTL_SECONDS`
  (default 5 minutes), then regenerated.
- PPPoE status publication budget is 30s (env `LY_ROUTE_PPPOE_STATUS_TIMEOUT`),
  covering slow virtualized/TCG helpers.

## Regression tests

- `backend/internal/runtime/vpp/pppoe_runtime_readiness_test.go`
  - delayed status file
  - delayed session interface creation
  - fail-closed when the status never appears
- `backend/internal/runtime/vpp/rollback_cleanup_order_test.go`
  - LCP cleanup precedes native attachment deletion
  - unknown ingress counts as detached; a live policy still fails verification
- `backend/internal/runtime/vpp/nat44_identity_snapshot_test.go`
  - identity mapping does not break NAT snapshot parsing
- `backend/internal/runtime/apply/gateway_rollback_reapply_test.go`
  - phased cleanup runs for every applied resource
  - re-apply succeeds after a failed generation
- `backend/internal/httpapi/runtime_artifact_phases_test.go`
  - PPPoE precedes the gateway; Linux routing is deferred for runtime PPPoE
- `scripts/test-gateway-apply-order-209.sh`
  - clean import + apply committed
  - PPPoE connected and pingable
  - repeat apply is idempotent

## Live evidence (independent 209 instance)

Host: `10.1.18.100`, pure 209 disk
`dist-209c51ebd2ab/disk/ly-route-gateway-bookworm-amd64-4g.img` plus a fresh
qcow2 overlay (no changes to the 1665 lab or the main worktree).

Clean state -> import `lab-desired-209.json` -> apply:

```text
apply_status committed runtime_state running
txn runtime-648424098cc19eb62938b1d31ad3087c
component vpp running typed gateway apply and live VPP readback verified
pppoe state connected assigned_ipv4 100.64.0.102
```

PPPoE dataplane:

```text
vppctl ping 100.64.0.1 source lyppp-8a258199 repeat 3
116 bytes from 100.64.0.1: icmp_seq=1 ttl=64 time=18.3192 ms
116 bytes from 100.64.0.1: icmp_seq=2 ttl=64 time=17.6436 ms
116 bytes from 100.64.0.1: icmp_seq=3 ttl=64 time=14.0053 ms
Statistics: 3 sent, 3 received, 0% packet loss
```

DNS service network / NAT / LCP:

```text
show ip fib table 59377
  0.0.0.0/0 [@5]: ipv4 via 0.0.0.0 lyppp-8a258199
show nat44 interfaces
  lyroute-eth2 in
  lyppp-8a258199 out
  lydnscdd4e1 in
show lcp
  itf-pair: [0] lyroute-eth2 tap4096 lylan-eth2 6 type tap
show abf policy 65000
  abf:[0]: policy:65000 acl:0
```

## Known lab limitation

Repeating the live apply on the same TCG guest after VPP has been running for a
while reproduced a VPP 26.06 CLI hang (`show interface` stops answering) during
NAT/QoS replay. The transaction itself is covered by the tests above; a fresh
VPP restart and a fresh 209 instance are required for each long live run. This
is independent of the ordering/rollback changes and is reported as a separate
NAT/QoS dataplane risk.

A repeat apply after a *bare* VPP restart (live objects gone, desired unchanged)
can also fail closed at `prepare` with `QoS readback ... returned incomplete
output`: an absent QoS object is currently treated as incomplete instead of
absent. That is P0-5 QoS reconciliation scope and is not changed here; the
ordering/rollback work does not depend on it. A normal reboot replays the
persisted VPP operations before the runtime apply and does not hit this path
when the replay succeeds.

## Cherry-pick

```bash
git fetch /root/work/ly-route-gateway-applyfix feat/apply-order-209
git cherry-pick <commit>
```

The branch contains one commit on top of `209c51ebd2ab`.
