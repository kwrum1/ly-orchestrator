# Orchestrator upgrade packages

The orchestrator ships incremental upgrades as `ly-route-upgrade` packages and
installs them through its own upgrade API/console. Copying files into
`/opt/ly-route/admin` or `/usr/lib/ly-route` by hand is not a supported
deployment path: it leaves the version, backup and rollback contract behind.

## Build

```bash
scripts/build-orchestrator-upgrade-package.sh --version 0.1 --out dist \
  --vpp-apply /path/to/vpp-apply
```

The script rebuilds the console from `frontend/` (never from or into the frozen
`packaging/orchestrator-ui-baseline/`), builds the control binary with
`-X ly-route/backend/orchestrator.buildVersion=<version>`, and writes:

```text
manifest.json                      strict JSON, package_type=ly-route-upgrade
checksums.sha256                   sha256sum -c compatible
usr/lib/ly-route/ly-route-control  control-plane binary (0755)
usr/lib/ly-route/vpp-apply         required dataplane helper (0755)
opt/ly-route/admin/                console (index.html, app.js, styles.css, ...)
etc/ly-route/version.env           LY_ROUTE_VERSION=<version>
```

Output: `dist/ly-route-orchestrator-<version>-<arch>.tar.zst` plus `.sha256`.

`vpp-apply` belongs to the appliance base package, not this repository. The
receiver refuses a package without it, so the build carries the current helper
forward: pass `--vpp-apply`, or set `LY_ROUTE_VPP_APPLY`, or keep the helper at
`/usr/lib/ly-route/vpp-apply`.

## Manifest contract (enforced by code, not by this document)

`backend/internal/httpapi/upgrade_package.go` decodes the manifest with
`DisallowUnknownFields`; `backend/internal/httpapi/orchestrator_upgrade_install.go`
then greps for `"package_type": "ly-route-upgrade"` and
`"product": "orchestrator"` (note the space after the colon) and verifies
`sha256sum -c checksums.sha256`.

Allowed manifest fields only:

```json
{
  "package_type": "ly-route-upgrade",
  "version": "0.1",
  "product": "orchestrator",
  "from_version": "26.06+lyroute1",
  "to_version": "0.1",
  "suite": "bookworm",
  "arch": "amd64",
  "commit": "<short sha>",
  "created_at": "2026-10-31T00:00:00Z",
  "install_root": "/",
  "services": ["ly-route-control-api.service"],
  "checksums": { "usr/lib/ly-route/ly-route-control": "...", "...": "..." }
}
```

`checksums` must contain at least `usr/lib/ly-route/ly-route-control` and
`opt/ly-route/admin/app.js`, and every listed path must exist with that hash.
`version` (falling back to `to_version`) is what the upgrade task and console
display; `etc/ly-route/version.env` makes `/health` report the same value.

## Install on the appliance

The orchestrator product profile does not grant the `firmware` capability, so
`/api/v1/firmware/update/{stage,install}` are not registered on 10.1.18.234.
The equivalent console path is 系统 → 升级 / 回滚:

```bash
TOKEN=$(curl -sk -c /tmp/cookies -X POST https://127.0.0.1/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"..."}' | ...)
curl -sk -b /tmp/cookies -F package=@ly-route-orchestrator-0.1-amd64.tar.zst \
  https://127.0.0.1/api/v1/orchestrator/upgrades
curl -sk -b /tmp/cookies -X POST \
  https://127.0.0.1/api/v1/orchestrator/upgrades/<task-id>/install \
  -H 'Content-Type: application/json' -d '{"confirm_install":true}'
```

The detached installer:

1. verifies the package hash, `manifest.json`, `checksums.sha256`, the control
   binary and `vpp-apply`;
2. backs up `/usr/lib/ly-route`, `/opt/ly-route/admin`, the vpp orchestrator
   plugin (if changed), `version.env` and the service enable state into
   `/var/lib/ly-route/orchestrator/upgrades/backups/<task-id>/`;
3. replaces the payload, installs `version.env`, restarts
   `ly-route-control-api.service` and passes a health check; any failure
   restores the backup.

Existing configuration (`/var/lib/ly-route/orchestrator/ly-route.db`,
`config.json`) is not touched. Rollback is
`POST /api/v1/orchestrator/upgrades/<task-id>/rollback`.

## Versioning

Start at `0.1`; every further change is a new package (`0.1.1`, `0.2`, ...).
Do not hand-edit files on the appliance; the installed `version.env`, the
control binary and the console bundle must always come from the same package.
