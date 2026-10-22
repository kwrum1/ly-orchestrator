# Config apply: asynchronous HTTP + status

The appliance enables `LY_ROUTE_CONFIG_APPLY_ASYNC=1` in the control-api
systemd unit.

`POST /api/v1/config/apply` now returns immediately:

```json
{
  "status": "apply_in_progress",
  "runtime_state": "applying",
  "transaction_id": "txn-...",
  "query": "/api/v1/config/apply/status?transaction_id=txn-..."
}
```

with HTTP 202. The transaction continues with a context detached from the
client request. Poll:

```text
GET /api/v1/config/apply/status?transaction_id=txn-...
```

The result status is one of `apply_in_progress`, `committed`, `degraded` or
`apply_failed`; the response includes the underlying apply reason and HTTP
status. `GET /api/v1/runtime/status` remains the long-term last-apply view.

## Component timeout boundary

The systemd command window is four minutes, and SmartDNS has
`TimeoutStartSec=240s`, because a VPP TAP can be created late during a TCG boot.
This avoids reporting a valid slow service start as a component failure. The
apply transaction itself remains bounded by the existing eight-minute runtime
budget.

This does **not** claim sub-60-second apply on TCG. Real hardware latency must be
measured separately. A 202 response means the control plane accepted the
transaction, not that the dataplane is already committed.
