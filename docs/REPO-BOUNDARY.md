# 仓库边界：ly-route（网关）与 ly-orchestrator（编排器）

本文记录两个仓库在 2026-10-31 的最终边界。此前的“共享后端”阶段已结束：
`ly-route` 只保留网关产品，`ly-orchestrator` 自带编排器后端、UI 源码、镜像口功能与
构建脚本。两侧**没有 submodule、没有交叉 import、没有构建时依赖**，CI 各自只在本仓库
执行。

- 网关仓库：`kurumi/ly-route`，分支 `release/lyroute2`
- 编排器仓库：`kurumi/ly-orchestrator`（本仓库），分支 `main`

## 编排器专属（只在本仓库）

| 内容 | 路径 |
|---|---|
| 控制面入口 | `backend/cmd/orchestrator-control`、`backend/orchestrator/` |
| 编排器领域模型 | `backend/internal/orchestrator/`（topology / policy / bond / service-chain） |
| 编排器 HTTP API | `backend/internal/orchestratorapi/` |
| 编排器 HTTP 边界 | `backend/internal/httpapi/orchestrator_*.go`、`conversion_http.go` |
| 透明数据面 | `backend/internal/runtime/vpp/orchestrator_transparent.go`、`orchestrator_bond_transition.go`、`service_chain_*.go` |
| 镜像口（端口镜像） | `backend/internal/httpapi/interface_mirror.go`、`backend/internal/runtime/vpp/interface_mirror.go`、`docs/port-mirror.md` |
| 编排器 UI 源码 | `frontend/`（Vite + React + TS，单一产品，无产品开关） |
| UI 构建脚本 | `scripts/build-orchestrator-ui.sh`（产出与基线同结构的 bundle，不覆盖基线） |
| 已发布 UI 固定产物 | `packaging/orchestrator-ui-baseline/`（部署用；CI 只校验，不重建） |
| product / build profile | `packaging/product-profiles/orchestrator.json`、`packaging/build-profiles/orchestrator*.json` |
| 契约测试 | `scripts/test-orchestrator-*.sh`、`scripts/test-vpp-orchestrator-transparent.sh` |
| 文档 | `docs/*`、`frontend/README.md` |
| CI / 发布 | `.gitea/workflows/ci.yml`、`release.yml` |

## 网关专属（只在 ly-route）

| 内容 | 路径 |
|---|---|
| 网关入口 | `backend/gateway/`、`backend/cmd/gateway-control` |
| 网关 UI 源码 | `frontend/gateway/`（只构建网关界面） |
| 网关 product profile | `packaging/product-profiles/gateway.json` |
| 整机镜像 / 安装器 | `packaging/rootfs-overlay`、`iso-assets`、`installer/`、`deploy/`、`pingora/` |
| 镜像口 | **无**：镜像口是编排器功能，网关不再提供 API 与 UI |

## 两侧同名但独立的包

拆分后两侧仍各有一份同名包（`internal/httpapi`、`internal/product`、
`internal/persistence`、`internal/runtime/vpp`、`internal/configgraph` 等），但它们是
**各自的源码副本**，不再是“共享包”，也不要求逐文件同步：任何一侧的改动只影响本仓库。
需要把某个修复带到另一侧时，按普通 commit 复制，而不是靠构建依赖。

两侧的 Go module 名都写作 `ly-route/backend`。这是拆分时保留的模块名，只影响本仓库内的
import 前缀，不指向对方仓库；改名会牵动发布流水线的 ldflags，留待公共库抽取时统一处理。

## 无法完全分离的部分（如实说明）

1. **同名后端包**：两侧各有一份 `internal/*`。它们不再共享文件，但概念上仍是同一套
   API/配置模型的两份实现，后续若要消除重复，需要抽 `ly-route-core`。
2. **VPP 插件产物**：`ly_route_orchestrator_plugin.so` 的构建脚本
   （`scripts/build-vpp-orchestrator-plugin.sh`）与运行时包仍在 `ly-route`，
   因为网关镜像/升级包的脚本链会引用它。它属于 VPP 数据面插件，不是编排器控制面。
3. **UI 静态基线**：`packaging/orchestrator-ui-baseline/` 是 2026-10-27 的冻结产物，
   与 `frontend/` 当前源码并不同步（源码含镜像口页面等新内容）。部署仍用基线，
   源码构建只在 CI 验证。

## CI 与构建

本仓库 CI（`.gitea/workflows/ci.yml`）执行：

```bash
cd backend && go build ./...
cd backend && go test ./internal/orchestrator/... ./internal/orchestratorapi/... ./orchestrator/... ./cmd/orchestrator-control/...
cd backend && go test ./internal/httpapi -run 'TestOrchestrator|TestGatewayConversion|TestFirmwareUpgradeInstaller'
cd backend && go test ./internal/httpapi ./internal/runtime/vpp -run 'Mirror'
cd backend && go test ./internal/product ./internal/persistence ./internal/productupdate
sh scripts/test-orchestrator-{policy,negative,dataplane}-contract.sh
sh scripts/check-retired-ui-boundary.sh
cd frontend && pnpm install --frozen-lockfile && pnpm exec tsc -b && pnpm exec vite build
```

`go test ./internal/httpapi` 整包在 CI 中不跑：其中网关数据面用例需要真实 VPP。
需要数据面验收时使用 `scripts/test-orchestrator-product-contract.sh`。

## 后续（可选）

1. 抽 `ly-route-core`，把两侧同名的 `product` / `persistence` / `configgraph` /
   `runtime/vpp` 收敛成版本化公共库。
2. UI 基线刷新流程：产品负责人确认后，用 `scripts/build-orchestrator-ui.sh` 的产物
   替换 `packaging/orchestrator-ui-baseline/` 并记录新 sha256。
