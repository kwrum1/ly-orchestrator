# Orchestrator frontend bundle (accepted 11-page IA)

This directory vendors the accepted Orchestrator product UI so every
Orchestrator rootfs, upgrade package and disk image ships the same 11-page
bundle, independently of the legacy controller-shell generator.

## Provenance

| Field | Value |
|---|---|
| Product | `orchestrator` |
| Source commit | `1113170be5c057aeeefb8283e898a7d098bb4e79` (`feat/orchestrator-ui-align`) |
| Source tree | `frontend/gateway` (React/Vite), `LY_ROUTE_PRODUCT=orchestrator` |
| Build command | `cd frontend/gateway && LY_ROUTE_PRODUCT=orchestrator pnpm build` |
| Built at | 2026-10-25T20:12:36Z |
| Pages | 11 (单机总览, 单机拓扑, 编排组, 流量编排, 透明编排, 遥测, 运行态, 配置与快照, 升级 / 回滚, 产品转换, 用户管理) |
| `app.js` SHA-256 | `b3e4027e232226541aa77f6225da69005b5279850db7ce73d9c7a5be5ebd0dea` |

`manifest.json` records the SHA-256 of every bundled file. The pin that the
build enforces lives in `packaging/build-profiles/orchestrator.json`:

```json
"frontend_bundle": "packaging/orchestrator-frontend/bundle",
"frontend_app_sha256": "b3e4027e232226541aa77f6225da69005b5279850db7ce73d9c7a5be5ebd0dea",
"frontend_bundle_source": "1113170be5c057aeeefb8283e898a7d098bb4e79"
```

`scripts/lib/product-build-profile.sh` verifies the copied `app.js` against
`frontend_app_sha256` in `copy_product_frontend()` (rootfs and upgrade
packages) and `scripts/build-disk-image.sh` re-verifies the extracted rootfs
`app.js` before imaging. A mismatch fails the build; no Orchestrator product
artifact can silently ship a different UI.

The Gateway build path is unchanged: it has no pin and keeps the strict
`validate_frontend_bundle()` contract plus its controller-shell generator.

## Updating the bundle

1. Build the new Orchestrator UI from `frontend/gateway` as above.
2. Replace `bundle/` and refresh every hash in `manifest.json`.
3. Update `frontend_app_sha256` and `frontend_bundle_source` in
   `packaging/build-profiles/orchestrator.json`.
4. Re-run `scripts/test-product-builders.sh` and the Orchestrator UI sweep
   (`scripts/acceptance/orchestrator-esxi/ui-sweep*.mjs`).

Do not edit `bundle/app.js` in place: the pin exists to make an accidental or
unreviewed UI substitution a build failure.
