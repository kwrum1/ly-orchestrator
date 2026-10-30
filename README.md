# Ly Route Orchestrator

LY Route 编排器（Orchestrator）独立仓库：控制面二进制 `orchestrator-control`、编排策略 API、
编排 UI（controller-shell 静态页 + 已验收 11 页 React bundle）、以及编排器的 product/build profile。

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

# UI bundle 契约（单入口、单 capability manifest、无网关词汇）
sh scripts/test-orchestrator-ui-bundle-contract.sh
```

需要在真实数据面主机上运行的完整验收脚本仍保留在 `scripts/`，但**不在 CI 中执行**：

- `test-orchestrator-product-contract.sh`（跑整个 `internal/httpapi`，依赖 vppctl /
  `/usr/lib/ly-route/runtime-check.sh`）
- `test-orchestrator-container-api.sh`、`test-orchestrator-vpp-*.sh`、
  `test-vpp-orchestrator-transparent.sh`（需要 Docker + VPP）
- `test-orchestrator-demo-ui.mjs`、`test-controller-shell-orchestrator-ui.mjs`（需要 Playwright 和在线实例）

## 发布

打 `v*` tag 触发 `.gitea/workflows/release.yml`，产出并上传：

- `orchestrator-control-linux-amd64`
- `orchestrator-control-linux-arm64`
- `ly-orchestrator-frontend.tar.zst`

上传走 `scripts/ci-upload-release.sh`（Gitea Release API）。

## 目录

| 路径 | 内容 |
|---|---|
| `backend/` | Go module `ly-route/backend`（当前全量共享） |
| `backend/cmd/orchestrator-control` | 编排器控制面入口 |
| `backend/orchestrator` | 控制面启动入口 (`orchestrator.Run()`) |
| `frontend/orchestrator` | controller-shell 静态编排 UI 源码 |
| `packaging/orchestrator-frontend` | 已验收 11 页 React bundle（随仓库固定的产物） |
| `packaging/build-profiles`, `packaging/product-profiles` | 编排器 build / product profile |
| `scripts/` | 编排器构建、契约测试、发布脚本 |
| `docs/` | 编排器文档与仓库边界说明 |
