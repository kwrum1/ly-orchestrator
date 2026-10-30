# 仓库边界：ly-route 与 ly-orchestrator

本文记录编排器从 `kurumi/ly-route` 拆分到 `kurumi/ly-orchestrator` 时实际划出的边界，
以及还没有划清的部分。拆分时间是 2026-10-31，源提交 `7a8ad0e5`（`release/lyroute2`）。

原则：**能独立构建的才搬，搬不动的留在原地并写清楚**。宁可记一笔“仍共享”，
也不复制一份会产生漂移的副本。

## 已分离（只存在于本仓库）

| 内容 | 路径 | 说明 |
|---|---|---|
| 编排器控制面入口 | `backend/cmd/orchestrator-control` | `main.go` → `orchestrator.Run()` |
| 控制面启动逻辑 | `backend/orchestrator/` | 编排器启动、装配 |
| 编排器领域模型 | `backend/internal/orchestrator/` | topology / policy / bond / service-chain |
| 编排器 HTTP API | `backend/internal/orchestratorapi/` | 编排器专属 handler、DTO、reconcile |
| 编排器静态 UI | `frontend/orchestrator/` | controller-shell 13 个文件，自包含 |
| 已验收 UI bundle | `packaging/orchestrator-frontend/` | 11 页 React 产物 + manifest 哈希 |
| 编排器 build profile | `packaging/build-profiles/orchestrator*.json` | 含 frontend bundle sha256 钉 |
| 编排器 product profile | `packaging/product-profiles/orchestrator.json` | 能力清单 |
| 编排器契约测试 | `scripts/test-orchestrator-*.sh`、`test-orchestrator-ui-bundle-contract.sh` | 策略/反例/数据面/UI bundle/升级转换 |
| 编排器容器与 VPP 验收 | `scripts/test-orchestrator-container-api.sh`、`test-orchestrator-vpp-*.sh`、`test-vpp-orchestrator-transparent.sh` | 需要 Docker/VPP 主机 |
| 编排器文档 | `docs/ORCHESTRATOR-POLICY-ORDER.md` | 策略执行顺序语义 |
| 构建脚本 | `scripts/build-controller-shell.sh`、`scripts/lib/product-build-profile.sh` | UI/产物装配 |
| 发布脚本 | `scripts/ci-upload-release.sh` | Gitea Release 资产上传 |
| CI / 发布流水线 | `.gitea/workflows/ci.yml`、`release.yml` | runner label `x86-physical` |

## 仍共享（两边都有，且必须手动同步）

`backend/` 在当前阶段是**全量拷贝**。下面这些包同时服务控制面与网关，任何一侧单独删除
都会让另一侧 `go build ./...` 失败：

| 共享包 | 为什么切不开 |
|---|---|
| `backend/internal/httpapi` | 单一 mux + 单一 server，编排器路由与网关路由在同一 `server.go` 注册；product profile 决定启用哪一组 |
| `backend/internal/product` | 产品 ID / 能力 allowlist 的唯一来源，两侧 handler 都引用 |
| `backend/internal/persistence` | 同一 SQLite store / schema |
| `backend/internal/runtime/vpp` | 编排器的 bond、service chain、transparent 数据面实现与网关 DPDK 路径同包 |
| `backend/internal/configgraph` | 依赖图编译，两侧共用 |
| `backend/internal/api`、`thermal`、`productupdate`、`geodata`、`vppfake` | 两侧共用或仅测试用 |
| 其余 `backend/cmd/*` | `gateway-control` 等网关二进制仍在拷贝里，未删除，避免共享包因缺少调用方而失去编译覆盖 |

另外两个小文件也属于“共享契约”，不是编排器专属：

- `packaging/product-profiles/gateway.json`（571 字节）：`backend/internal/product` 的
  `TestProfileManifestsMatchCanonicalAllowlists` 会遍历所有 product profile 校验
  能力 allowlist 与后端表一致。缺这个文件，共享包的单测直接失败。
- `packaging/fixtures/product-conversion/gateway-to-orchestrator.json`：升级转换契约 fixture，
  `internal/productupdate` 测试读取。

## 有意不搬的内容

- `frontend/gateway/`（182 MB）：网关 React/Vite 源码，编排器的 11 页 bundle 是它的
  构建产物，已作为固定产物放在 `packaging/orchestrator-frontend/` 并带 sha256 钉。
  重建该 bundle 仍需回到 `ly-route`。
- `packaging/geodata/`（29 MB）：路由地理库，`internal/httpapi` 的 geodata 测试在文件缺失时
  自动 `t.Skip`，构建不需要。
- `packaging/rootfs-overlay`、`iso-assets`、`runtime`、`deploy/`、`installer/`、`e2e/`、
  `pingora/`：网关整机/镜像/安装器流水线，编排器仓库不负责出镜像。

## 已知缺口（不伪造通过）

`go test ./internal/httpapi`（整包）在本仓库 CI 中**不跑**，因为其中 45 个网关数据面测试
需要真实 VPP + `/usr/lib/ly-route/runtime-check.sh`，在普通构建机上必然失败：

```
dataplane_locked: runtime_capability_proof ... native_hook ... high_performance
```

这与拆分无关，`ly-route` 的 CI 同样不跑整包。CI 只跑编排器相关的
`TestOrchestrator*` / `TestGatewayConversion` / `TestFirmwareUpgradeInstaller` 子集。
需要在数据面主机上验证时，用 `scripts/test-orchestrator-product-contract.sh`。

## 后续（公共库抽取）

1. 新建 `ly-route-core`（或 Go workspace），抽出 `product`、`persistence`、`configgraph`、
   `internal/orchestratorapi` 的 DTO 层。
2. `httpapi` 拆成 `httpapi/core` + `httpapi/gateway` + `httpapi/orchestrator` 三组路由注册器。
3. `runtime/vpp` 拆成公共 vppctl 客户端与两侧各自的数据面编排。
4. 两侧改为依赖 `ly-route-core` 的固定版本，`backend/` 副本随之删除，本文件同步改写。

## 同步基线（重要）

本仓库的 `backend/` 是 `kurumi/ly-route` **`7a8ad0e5`**（`release/lyroute2`）时刻的全量快照。
拆分完成后 `ly-route` 又合入了两个网关数据面提交（`f33f1c3e`、`67f007e8`，
内容是 AF_PACKET consent 与 Rockchip AF_XDP takeover，涉及
`backend/internal/runtime/takeover/`、`backend/internal/httpapi/takeover_*.go` 共 14 个文件）。

也就是说：

| 仓库 | `backend/` 对应提交 | 状态 |
|---|---|---|
| `ly-orchestrator`（本仓库） | `7a8ad0e5` | 可独立 `go build ./...`，CI 绿 |
| `ly-route` | `3b348a91`（含上述数据面提交） | 网关 CI 绿 |

这些差异是网关数据面能力，不影响编排器构建与运行，但**共享包的修改必须两边同步**。
同步方式（在 `ly-route` 仓库执行）：

```bash
# 查看共享后端相对本仓库快照之后的所有改动
git diff --name-status 7a8ad0e5 HEAD -- backend
# 把这些文件复制到本仓库 backend/ 对应路径后，跑一次
cd backend && go build ./... && go test ./internal/orchestrator/... ./internal/orchestratorapi/... -count=1
```

在 `ly-route-core` 抽出来之前，建议每次 `ly-route` 合并到 `release/lyroute2` 后同步一次，
并把上表的 `backend/` 提交号更新为最新值。
