# Orchestrator UI apply, refresh and delete semantics

This note records the gateway (Ly Route 1.2) interaction lessons that were
ported to the orchestrator console.

## What the frozen baseline already does

- Every save page calls the shared `ApplyProvider` (`runtime/apply`), which
  keeps one in-flight apply and a pending flag. A save that arrives while an
  apply is running shows `已保存，等待当前应用结束后自动应用`; when the first
  transaction settles the pending apply runs against the newest desired state.
- The policy page uses `PUT /api/v1/orchestrator/policy?async=1` with
  `Prefer: respond-async`, polls `policy/status` and shows
  `顺序已生效（Xs）` when the transaction lands. A second edit while the first
  is applying is queued (`g.current`) and replayed after it settles.
- The shared apply provider polls `runtime/status` every 3.5 s and invalidates
  `runtime`, `resources` and `telemetry` after the transaction settles.

## What the extras add

The deployed shell is the frozen baseline plus
`packaging/orchestrator-ui-extras/orchestrator-extras.js`; the baseline is not
rebuilt. The extras add two gateway-parity safeguards:

1. **Refresh actually refreshes.** The shell dispatches `lyroute:refresh` for
   the toolbar action but the baseline has no listener. The extras reload the
   active route so the operator sees the committed generation instead of a
   toast over stale cards.
2. **Apply mutations are serialized.** The shell's ApplyProvider already gates
   the main apply, but the policy page and the extras issue async policy PUTs.
   The extras wrap `fetch` and queue
   `runtime/apply`, `orchestrator/policy` and
   `orchestrator/service-chains/apply` so a second save waits for the first
   request instead of racing the VPP transaction.

## Delete does not restart VPP

A policy or policy-group delete is a desired-policy change. It is committed
through the VPP CLI:

```text
set ly-route orchestrator candidate clear
set ly-route orchestrator candidate boundary ...
set ly-route orchestrator candidate group ...
set ly-route orchestrator candidate rule ...
set ly-route orchestrator commit generation <hash>
```

There is no `systemctl restart vpp` and no control-plane restart in that path.
The console must therefore show `配置应用中`, not a generic "system
restarting" state, for a policy delete. Only a topology/port change that
requires a dataplane transition may restart VPP; that is a different operation
and is already surfaced as `配置应用中` with the apply result.

## Verification

```bash
# UI bundle is baseline + extras; the baseline app.js stays byte-identical
sh scripts/build-orchestrator-ui.sh --out /tmp/ui-orch
sha256sum /tmp/ui-orch/app.js   # 0d197d8daa420d01...

# policy apply and delete
curl -s -X PUT -H 'Prefer: respond-async' \
  -H 'Content-Type: application/json' \
  --data @policy.json 'http://127.0.0.1:8080/api/v1/orchestrator/policy?async=1'
curl -s 'http://127.0.0.1:8080/api/v1/orchestrator/policy/status?transaction_id=...'
vppctl show ly-route orchestrator
systemctl show -p NRestarts --value vpp.service
```

Expected: `policy/status=applied`, `vpp.service` `NRestarts` unchanged, and
the console shows `顺序已生效`/`配置应用中`, never a dataplane restart for a
policy-only delete.

## Create/delete concurrency contract

The gateway once crashed when a policy delete raced a policy create that was
still applying. The orchestrator closes that class of race in three layers:

1. `orchestratorapi.Handler.mutationMu` wraps every write handler
   (`PUT/DELETE /topology`, groups, policy) from decode through the VPP apply,
   the repository write and the journal commit. A delete that arrives during a
   create waits for the whole create transaction before it starts.
2. `productionOrchestratorRuntime.transparentMu` serialises every VPP
   `candidate clear/add/commit` sequence, including the explicit
   `/api/v1/runtime/apply` path and the periodic reconciler. Two apply paths
   cannot interleave VPP candidate state.
3. `transparentJournalPending` stops the periodic reconciler from comparing the
   durable (older) policy against a mutation that is between its VPP apply and
   its persistence.

Regression test:
`backend/internal/orchestratorapi/concurrency_test.go` starts a policy create,
waits until its `ApplyTransparent` is in flight, then sends the delete and
asserts the two transparent applies never overlap and the final repository has
no policy.
