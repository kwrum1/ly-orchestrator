# Ly Route Orchestrator

LY Route 编排器（Orchestrator）独立仓库：控制面二进制 `orchestrator-control`、编排策略 API、
以及编排器的 product/build profile。编排器 UI 是 React/Vite 应用，源码暂在 `ly-route`，
本仓库不再包含任何 UI 源码或 UI 构建产物（见下节）。

本仓库从 `kurumi/ly-route` 拆分而来，用于让编排器拥有独立的构建、测试与发布节奏，
不再阻塞也不再被网关的发布流程牵连。

- 编排器产品仓库：`http://10.1.18.100:10000/kurumi/ly-orchestrator`（本仓库）
- 网关产品仓库：`http://10.1.18.100:10000/kurumi/ly-route`

## 当前状态：backend 仍与网关共享

拆分的第一阶段，`backend/` 是**全量拷贝**，不是精简后的编排器后端。原因是控制面与网关
在同一个 Go module 里共享 `internal/httpapi`、`internal/product`、`internal/persistence`、
`internal/runtime/vpp` 等包，直接切开会让两侧同时构建失败。

具体哪些内容已分离、哪些仍共享、后续怎么抽公共库，见 [`docs/REPO-BOUNDARY.md`](docs/REPO-BOUNDARY.md)。
在公共库抽出之前，**修改 `backend/internal/httpapi` 等共享包必须同时在两个仓库对齐**。

## 编排器 UI：冻结基线 + 增量页面（唯一 UI）

唯一有效的编排器 UI 是 `packaging/orchestrator-ui-baseline/` 的冻结外观，加上
`packaging/orchestrator-ui-extras/` 的镜像口/故障旁路/编排组页内页面。
`scripts/build-orchestrator-ui.sh` 负责组合这两部分（不改基线 app.js，只注入一行
`orchestrator-extras.js`），升级包从组合结果构建。`frontend/` 重建 shell 不再参与
部署或打包。

已淘汰的 controller-shell 静态 UI 已彻底删除：`frontend/orchestrator/`（13 个源码文件）、
仓库内固定的 `packaging/orchestrator-frontend/`（旧 11 页 bundle + manifest）、
`scripts/build-controller-shell.sh`、`scripts/lib/product-build-profile.sh`，以及只服务
它的 `test-orchestrator-ui-bundle-contract.sh`、`test-controller-shell-orchestrator-ui.mjs`、
`test-orchestrator-demo-ui.mjs`。它曾被误部署到 VM，覆盖了新 UI。

`scripts/check-retired-ui-boundary.sh` 在 CI 与发布流水线中把关：旧路径、controller-shell
源码（`nic-settings.js` / `group-settings.js` / `forms.css` / `responsive.css`）、
约 185 KB 的 shell bundle 重新出现即失败。历史 baseline 不再参与部署或 hash 门禁。

## 构建

需要 Go 1.22+（编译器实测 1.26）。

```bash
cd backend
go build ./...
go build -trimpath -ldflags "-s -w" -o ../dist/orchestrator-control ./cmd/orchestrator-control
```

`backend/go.mod` 的 module path 仍是 `ly-route/backend`，因此 `backend/` 目录布局必须保持
原样，跨包 import 才不会断。

## 测试

CI 中运行的契约（见 `.gitea/workflows/ci.yml`）：

```bash
# 编排器领域单测
cd backend && go test ./internal/orchestrator/... ./internal/orchestratorapi/... -count=1

# 编排器 HTTP / 产品边界契约
cd backend && go test ./internal/httpapi \
  -run 'TestOrchestrator|TestGatewayConversion|TestFirmwareUpgradeInstaller' -count=1

# 策略 / 反例 / 数据面契约
sh scripts/test-orchestrator-policy-contract.sh
sh scripts/test-orchestrator-negative-contract.sh
sh scripts/test-orchestrator-dataplane-contract.sh

# 已淘汰 UI 的边界门禁（旧 shell 源码/bundle 重新出现即失败）
sh scripts/check-retired-ui-boundary.sh
```

需要在真实数据面主机上运行的完整验收脚本仍保留在 `scripts/`，但**不在 CI 中执行**：

- `test-orchestrator-product-contract.sh`（跑整个 `internal/httpapi`，依赖 vppctl /
  `/usr/lib/ly-route/runtime-check.sh`）
- `test-orchestrator-container-api.sh`、`test-orchestrator-vpp-*.sh`、
  `test-vpp-orchestrator-transparent.sh`（需要 Docker + VPP）

旧 controller-shell 的 Playwright 用例（`test-orchestrator-demo-ui.mjs`、
`test-controller-shell-orchestrator-ui.mjs`）已随 UI 一起删除；编排器 UI 的浏览器验收
放在 `ly-route` 的 React/Vite 工程里。

## 发布

打 `v*` tag 触发 `.gitea/workflows/release.yml`，产出并上传：

- `orchestrator-control-linux-amd64`
- `orchestrator-control-linux-arm64`
- `SHA256SUMS`

发布只产出控制面二进制，不再构建或上传 UI bundle（UI 由 `ly-route` 的
`frontend/gateway` 工程构建）。上传走 `scripts/ci-upload-release.sh`（Gitea Release API）。

## 目录

| 路径 | 内容 |
|---|---|
| `backend/` | Go module `ly-route/backend`（当前全量共享） |
| `backend/cmd/orchestrator-control` | 编排器控制面入口 |
| `backend/orchestrator` | 控制面启动入口 (`orchestrator.Run()`) |
| `packaging/build-profiles`, `packaging/product-profiles` | 编排器 build / product profile |
| `packaging/orchestrator-ui-baseline` | 已发布 React UI 固定产物（部署用，CI 不重建） |
| `scripts/` | 编排器构建、契约测试、发布脚本、旧 UI 边界门禁 |
| `docs/` | 编排器文档与仓库边界说明 |

本仓库没有 `frontend/`：UI 源码与 React 构建都在 `ly-route`。
