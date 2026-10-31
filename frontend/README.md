# 编排器 UI 源码（Vite + TypeScript + React）

本目录是 Ly Route 流量编排器的唯一 UI 源码，2026-10-31 从
`kurumi/ly-route` 的 `frontend/gateway` 迁入（源提交 `3b74302b`）。
迁入后本仓库不再依赖 ly-route 的任何源码或构建脚本。

## 构建

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm exec tsc -b          # 类型检查（tsc -b，输出到 tsconfig.tsbuildinfo）
pnpm exec vite build      # 产出 dist/：index.html、app.js、styles.css
```

或在仓库根目录打出与冻结基线同结构的 bundle：

```bash
sh scripts/build-orchestrator-ui.sh --out /tmp/orchestrator-ui
```

bundle 结构（与 `packaging/orchestrator-ui-baseline/` 一致）：

| 文件 | 说明 |
|---|---|
| `index.html` | 入口，`app.js?v=<sha256 前 12 位>` / `styles.css?v=...` |
| `app.js` | 单文件 bundle（`inlineDynamicImports`，入口固定 `app.js`） |
| `app.js.sha256` | `app.js` 的 sha256 |
| `styles.css` | 单文件样式（`cssCodeSplit: false`） |
| `capabilities.json` | 复制自 `packaging/product-profiles/orchestrator.json` |
| `shell.js`、`commercial.css`、`product.css` | 打包契约占位文件 |

## 与线上部署的关系

线上仍使用冻结基线 `packaging/orchestrator-ui-baseline/`（`app.js` 为 2026-10-27
发布的构建）。本目录的构建产物**不自动发布**：源码修复先在 CI 里构建验证，
再经产品负责人确认后才替换基线。不要用 `frontend/dist` 覆盖线上 `/opt/ly-route/admin`。

## 目录

- `src/products/orchestrator/routes.tsx`：唯一的产品路由（单一产品，无产品开关）
- `src/app/orchestrator-nav.tsx`：导航、命令面板动作、旧 hash 路由映射
- `src/app/nav.tsx`：只保留导航类型
- `src/pages/orchestrator/`：编排器页面（含 `PortMirrorPage.tsx` 镜像口）
- `src/pages/{ConfigPage,RuntimePage,UsersPage,LoginPage,NotFoundPage}.tsx`：共享页面
- `src/api`、`src/components`、`src/lib`、`src/state`、`src/styles`：UI 基础设施

网关专属页面（概览、在线用户、WAN/LAN、DHCP、代理、策略路由、端口映射、DNS、
QoS、对象管理、固件升级、管理员账号等）留在 ly-route 的网关 UI，不在这里。
