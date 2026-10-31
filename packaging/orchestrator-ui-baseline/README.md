# Orchestrator UI baseline (2026-10-27) — historical reference

This directory is **no longer the deployed UI and no longer a build/hash gate**.
It is kept as a historical reference for the 2026-10-27 console shell (navigation
and layout) that the current source tree was aligned with.

- The deployment artifact is the React/Vite build from `frontend/`, produced by
  `scripts/build-orchestrator-ui.sh` or `scripts/build-orchestrator-upgrade-package.sh`.
- The frozen files here are not modified; in particular `app.js` remains
  985377 bytes / sha256 `0d197d8daa420d01...`.
- `scripts/check-retired-ui-boundary.sh` still rejects the removed
  controller-shell generator, but it no longer requires this directory or its
  size/hash.
- Historical nav: 流量监控 / 网卡设置 / 编排组 / 流量编排 / IP 群组 / 系统配置 / 用户管理.
  The current source adds 镜像口 and 故障旁路 and keeps the same interaction model.
- Historical title: `ly Orchestrator`.
