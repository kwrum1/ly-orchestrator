# Orchestrator Upgrade / Conversion API

## Scope

- Upgrade endpoints are **Orchestrator-only**. They reuse the existing
  `ly-route-upgrade` payload produced by `scripts/build-upgrade-package.sh`.
- Conversion endpoints implement a **backend skeleton**: manifest validation,
  task persistence, config snapshot, service-enable-state capture/restore and
  rollback. Real image conversion is intentionally not executed in this
  release (`apply` returns `501 conversion_execution_blocked`).
- No Orchestrator ISO is added.

## Upgrade API

| Method | Path | Permission | Description |
|---|---|---|---|
| GET | `/api/v1/orchestrator/upgrades` | read | current version plus upgrade tasks |
| POST | `/api/v1/orchestrator/upgrades` | admin | multipart upload (`package` or `firmware`) |
| GET | `/api/v1/orchestrator/upgrades/{id}` | read | task status |
| POST | `/api/v1/orchestrator/upgrades/{id}/install` | admin | `{"confirm_install":true,"reboot":false}` → **202 accepted** |
| POST | `/api/v1/orchestrator/upgrades/{id}/rollback` | admin | → **202 accepted** |

Install and rollback are **accepted before execution**:

- the handler persists `installing`/`rolling_back`, returns
  `202 {"task_id","status","poll","item"}` with `Location` and `Retry-After: 2`;
- the response is flushed, then a detached `systemd-run` unit starts the
  installer/rollback after a short delay, so restarting the control API cannot
  cut the already-accepted HTTP response;
- clients poll `GET /api/v1/orchestrator/upgrades/{id}` until
  `installed/failed/rolled_back/rollback_failed`.
- If the control process exits after acceptance but before the unit starts, the
  next GET marks the task `failed`/`rollback_failed` after 30 seconds instead of
  leaving it stuck in `installing`.

States: `staged → installing → installed`, with `failed`,
`rolling_back`, `rolled_back`, `rollback_failed`.

The install and rollback scripts:

- verify the package SHA-256, `ly-route-upgrade` package type,
  product=orchestrator and `checksums.sha256`;
- require the VPP orchestrator plugin on the system or in the payload;
- back up `/usr/lib/ly-route`, `/opt/ly-route/admin`, nginx config and firstboot;
- capture and restore systemd **enable/disable/mask** state for the shared units;
- restart the control API and verify `/api/v1/health`; on failure restore the
  backup and service state;
- write a JSON result file so the task reconciles after a control restart.

The upgrade package manifest now records `version` (`LY_ROUTE_VERSION`, falling
back to the source commit) in addition to `commit`. Rootfs and upgrade payloads
carry `/etc/ly-route/version.env`; `ly-route-control-api.service` loads it, and
install/rollback preserve it so the version inventory updates after an upgrade
and returns to the previous value on rollback.

## Conversion API

Available on both Gateway and Orchestrator servers:

| Method | Path | Permission | Description |
|---|---|---|---|
| GET | `/api/v1/orchestrator/conversions` | read | list conversion tasks |
| POST | `/api/v1/orchestrator/conversions` | admin | `{ "manifest": {...}, "prepare": true }` |
| POST | `/api/v1/orchestrator/conversions/validate` | read | validate manifest without writes |
| GET | `/api/v1/orchestrator/conversions/{id}` | read | task status |
| POST | `/api/v1/orchestrator/conversions/{id}/prepare` | admin | snapshot + service-state capture |
| POST | `/api/v1/orchestrator/conversions/{id}/rollback` | admin | restore captured service enable state |
| POST | `/api/v1/orchestrator/conversions/{id}/apply` | admin | `501 conversion_execution_blocked` in this release |

Manifest requirements:

- `package_type` must be `ly-route-conversion`;
- `from_product`/`to_product` differ and `from_product` matches the server;
- `base_version` must match the current version family;
- `dataplane.preserve=true` with explicit `verify_packages`, or
  `includes_dataplane=true` with an explicit payload/verification declaration;
- config migration is `preserve|migrate|reset`; reset/migrate requires a snapshot;
- rollback backup and service-enable-state preservation are mandatory;
- checksums must include `usr/lib/ly-route/ly-route-control`.

Example: `packaging/fixtures/product-conversion/gateway-to-orchestrator.json`.

The Gateway firmware installer recognizes `ly-route-conversion` explicitly:
when `dataplane.preserve=true` and `verify_packages` are present it does not
require the Gateway-only PPPoE plugin or a fabricated
`includes_dataplane=true`. The normal Gateway upgrade path still requires its
own dataplane/PPPoE check.

## Error and observability contract

| Case | Status | Code |
|---|---|---|
| Orchestration group/topology needs an unavailable dataplane tier | 422 | `prerequisite_unmet` (clean message, no internal prerequisite list) |
| Invalid upgrade payload/checksum/archive | 422 | `invalid_upgrade_package`; process signals such as `signal: killed` are sanitized |
| Unknown JSON field | 400 | `unknown_field` with the field name in the message |
| Malformed JSON | 400 | `invalid_json` |
| Config import confirmation | 200 | accepts both `package_hash`/`diff_hash` and `confirmation_package_hash`/`confirmation_diff_hash`; dry-run returns both names |

Read-only snapshot content:

```
GET /api/v1/config/snapshots/{id}
→ 200 { "snapshot": {...}, "payload": { "resources": {...} } }
```

Long topology/policy apply progress:

```
GET /api/v1/orchestrator/operations/current
→ { "operation": { "name": "transparent-apply", "state": "running|complete|idle", "started_at": ... } }
```

Topology/policy PUT remains synchronous for transaction/rollback integrity; the
operation endpoint lets clients show progress while the request is in flight.

## Persistence and audit

Both task types use the additive `product_tasks` table
(`product_id`, `task_type`, `state`, `payload_json`, timestamps). Tasks survive
control restarts; `GET` reconciles installer result files. All stage/install/
rollback/prepare/apply actions write the normal audit events.

## Known limitations

- No real conversion execution or image mutation yet; `apply` is blocked.
- No conversion package upload endpoint yet; manifest validation and
  prepare/rollback are the testable contract.
- No multi-site/node management; this remains out of scope.
