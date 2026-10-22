# Orchestrator 产品 UI

> 分支：`feat/ui-redesign`
> 基线：Gateway UI `fa55f1a`；Orchestrator 后端 `601625d`
> 状态：**Orchestrator UI 实现完成，待全栈测试在真实 router/VPP 验收**
> 未修改后端 API、安全策略、nginx 默认入口；未合并 Pingora。

## 1. 产品边界

- Orchestrator 是**单机本地 VPP 流量编排器**。
- 拓扑只有本机一个：管理口、逻辑 LAN/WAN、物理数据口、编排组（服务链路径）。
- 没有站点注册、跨网关心跳、多站点集中下发；UI 不虚构这些入口。
- 透明编排是单机本地 L3 服务链，不是 L2 桥接，也不是多站点控制器。
- 产品转换只实现 manifest 校验、任务持久化、准备/快照/回滚；真实镜像转换 `apply` 返回 `501 conversion_execution_blocked`，UI 明确显示。

## 2. 双产品实现

- 同一套 React 19 + TypeScript + Vite 源码：`frontend/gateway/`。
- 构建期通过 `LY_ROUTE_PRODUCT=gateway|orchestrator` 选择产品路由与登录品牌；Vite `define` 注入 `__LY_PRODUCT__`。
- 共享：登录/改密、AppShell、App 布局、Toast/Modal/Drawer、运行态、配置、用户管理、遥测图表。
- 产品独立：导航、概览、拓扑、编排组、流量编排、透明编排、升级、转换页面。
- Gateway 页面集合固定为 **21 页**，功能未回归；因共享产品层重构，Gateway bundle 的 hash 已变化，以本文记录的新版本为准。
- Orchestrator 页面集合为 **11 页**。

### 产物

| 产品 | 输出目录 | `app.js` | `styles.css` | capabilities |
| --- | --- | --- | --- | --- |
| Gateway | `frontend/gateway/dist/` | 1,082.89 KB（gzip 331.87 KB） | 50.36 KB（gzip 9.25 KB） | `packaging/product-profiles/gateway.json` |
| Orchestrator | `frontend/gateway/dist-orchestrator/` | 992.45 KB（gzip 314.78 KB） | 50.36 KB（gzip 9.25 KB） | `packaging/product-profiles/orchestrator.json` |

最终 controller-shell 产物（`/tmp/*-ui-controller`）sha256：

```text
Gateway      app.js  4793ab44e67369a190de5c85f26a7d67200c97f5ba976b81ee48e8a6e326ed67
Gateway      styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
Orchestrator app.js  9cbcec6d6aa0794c2f790c72d1a833671c813d66757cbda82c7329a30b734485
Orchestrator styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
```

`app.js/styles.css/shell.js/commercial.css/capabilities.json` 文件名保持不变；`shell.js` 与 `commercial.css` 为兼容占位文件，不加载业务逻辑。`index.html` 标题按产品改为“Ly Route 出口网关”或“Ly Route 流量编排器”。

## 3. 页面与路由

| # | 路由 | 页面 | 关键内容 | 主要真实 API |
| --- | --- | --- | --- | --- |
| 1 | `/` | 单机总览 | 本机拓扑、运行态、边界流量、策略摘要、升级/转换边界 | `/product`、`/health`、`/runtime/status`、`/orchestrator/topology`、`/orchestrator/policy`、`/orchestrator/operations/current`、`/telemetry/dashboard`、`/orchestrator/upgrades`、`/orchestrator/conversions` |
| 2 | `/topology` | 单机拓扑 | 管理口、逻辑 LAN/WAN、单口/链路聚合、物理口占用、编排组草稿 | `/interfaces`、`/orchestrator/topology` GET/PUT |
| 3 | `/groups` | 编排组 | 成对 LAN/WAN-facing 端口、增删改 | `/orchestrator/orchestration-groups`、`/orchestrator/orchestration-groups/{name}` |
| 4 | `/policy` | 流量编排 | IP 对象、策略组、规则、默认 direct/drop/via、编译测试 | `/orchestrator/policy` GET/PUT、`/orchestrator/policy/compile` |
| 5 | `/transparent` | 透明编排 | 当前操作、服务链 flow/绑定、编译/应用入口、404 边界说明 | `/orchestrator/operations/current`、`/orchestrator/service-chains/compile|apply` |
| 6 | `/telemetry` | 遥测 | 边界累计、策略命中、流量趋势、Top 连接、在线用户 | `/telemetry/dashboard`、`/telemetry/policy-hits`、`/telemetry/traffic-trend`、`/telemetry/top-sessions`、`/telemetry/online-users` |
| 7 | `/runtime` | 运行态 | 组件、运行态预览、应用、最近事务 | `/runtime/status`、`/runtime/preview`、`/runtime/apply` |
| 8 | `/config` | 配置与快照 | 管理口、导出、导入预检/确认、快照、恢复、初始化 | `/management/network`、`/config/export`、`/config/import`、`/config/snapshots`、`/config/factory-reset` |
| 9 | `/upgrades` | 升级 / 回滚 | 当前版本、上传升级包、暂存、安装、回滚、异步任务轮询 | `/orchestrator/upgrades`、`/upgrades` POST、`/upgrades/{id}/install|rollback` |
| 10 | `/conversions` | 产品转换 | manifest 校验、创建准备任务、Apply 501、回滚 | `/orchestrator/conversions`、`/conversions/validate`、`/conversions/{id}/prepare|apply|rollback` |
| 11 | `/system/users` | 用户管理 | 管理员/只读账号 | `/auth/users` |

旧 Orchestrator hash 路由映射在 `src/app/orchestrator-nav.tsx` 的 `ORCHESTRATOR_LEGACY_ROUTES`：
`dashboard/overview`、`telemetry/flow-summary`、`telemetry/online-users`、`telemetry/top-connections`、`orchestrator/nic-settings`、`orchestrator/group-settings`、`orchestrator/policy`、`flow-control/traffic`、`security/policies`、`object/ip`、`system/users`、`system/config`。

## 4. 写操作与状态语义

- 拓扑/策略保存是同步事务：后端先验证，再触发透明编排；失败返回 `422` 并回滚，UI 显示后端原因，不显示假成功。
- 运行态应用统一走 `POST /runtime/apply`；`202 apply_in_progress` 不重复提交，Banner 展示事务号/耗时/降级原因。
- 升级安装/回滚返回 `202` 异步任务，页面每 5 秒轮询任务；控制服务重启期间允许短暂断连。
- 转换 `apply` 预期 `501 conversion_execution_blocked`；UI 显示专用提示，不把它写成“完成”。
- 独立 `service-chains/compile|apply|status` 端点只在运行时**未实现** `TransparentRuntime` 时注册；当前运行时通过拓扑/策略保存同步应用，页面在端点未注册时显示 404 说明。
- 只读账号的写按钮禁用；后端仍做最终权限校验。

## 5. 构建与预览

### Gateway

```sh
cd frontend/gateway
pnpm install
pnpm build                      # -> dist/
```

### Orchestrator

```sh
cd frontend/gateway
LY_ROUTE_PRODUCT=orchestrator pnpm exec tsc -b
LY_ROUTE_PRODUCT=orchestrator pnpm exec vite build --outDir dist-orchestrator
```

推荐使用产品构建脚本（会校验产物、复制 capabilities、做第三方名称/远程资源扫描）：

```sh
cd /root/work/ly-route-ui-redesign
./scripts/build-product-ui.sh --product gateway --out /tmp/gw-ui-final
./scripts/build-product-ui.sh --product orchestrator --out /tmp/orch-ui-final
```

兼容旧入口：

```sh
./scripts/build-gateway-ui.sh --out /tmp/gw-ui-final
./scripts/build-controller-shell.sh --product gateway --out /tmp/gw-ui-controller
./scripts/build-controller-shell.sh --product orchestrator --out /tmp/orch-ui-controller
```

### 本地预览

```sh
# Gateway：API 指向独立 control 沙箱
cd frontend/gateway
LY_ROUTE_UI_API=http://127.0.0.1:18090 pnpm exec vite preview --host 0.0.0.0 --port 4174

# Orchestrator：API 指向 orchestrator sandbox
LY_ROUTE_UI_API=http://127.0.0.1:18095 pnpm exec vite preview --host 0.0.0.0 --port 4175 --outDir dist-orchestrator
```

Vite 开发代理同样支持 `LY_ROUTE_UI_API`。

## 6. 验收证据

截图目录：`docs/ui-redesign/evidence/orchestrator/`

- `overview-1440.png` / `overview-1920.png`
- `topology-1440.png` / `topology-1920.png`
- `policy-1440.png` / `policy-1920.png`
- `transparent-1440.png`
- `telemetry-1440.png`
- `conversions-1440.png`
- `conversion-apply-501-1440.png`
- `config-import-preflight-1440.png`
- `readonly-1440.png`

浏览器验收（Playwright，构建机）：

- Gateway 侧栏 **21 项**，Orchestrator 侧栏 **11 项**；Gateway 无 Orchestrator 导航，Orchestrator 无 Gateway 导航。
- Orchestrator 全部 11 路由 1440×900 可访问；关键页 1920×1080 截图。
- 控制台除登录前 `/auth/session` 预期 401 外无错误；无未处理的 404。
- 策略编译返回真实 `path`（示例：`direct` → `exit: lan`）。
- 转换 manifest 校验通过、创建 `ready` 任务、Apply 返回真实 501 并在 UI 中显示。
- 配置导出后导入预检通过，展示 package/diff hash 与确认有效期。
- 只读账号 `orch-readonly` 登录后，拓扑保存、转换创建等写按钮禁用。
- 拓扑保存在本 sandbox 返回真实 `422 dataplane_locked`（VPP 未就绪）；UI 正确显示错误，不伪造保存成功。为保证只读/表单展示，测试拓扑与策略由临时 sandbox DB fixture 提供，**不是 UI 伪造数据**。

## 7. 已知缺口 / 后端限制

| 缺口 | 说明 | UI 处理 |
| --- | --- | --- |
| 真实镜像转换 | `apply` 501 | 明确显示 501，不显示成功 |
| 多站点/跨网关 | 后端未设计，超出产品边界 | 不提供入口，不虚构 |
| 转换镜像上传 | 后端没有上传端点，只接受 manifest JSON | 只上传 manifest |
| 独立服务链端点 | 运行时实现 `TransparentRuntime` 时未注册 | 显示 404 说明，实际由拓扑/策略同步应用 |
| 沙箱 VPP | dataplane locked，拓扑保存 422，遥测降级 | 显示真实原因；真实 router 验收待补 |
| 升级取消 | 当前 API 无取消端点 | 不显示取消按钮 |
| 策略命中 | 仅能展示 `/telemetry/policy-hits` 返回字段 | 缺失字段显示 `--`，不推断 |
| 转换完成态 | 只实现骨架 | 状态 `ready`/`failed`/`rolled_back` 来自真实任务 |

## 8. 提交

- 分支：`feat/ui-redesign`
- Orchestrator 后端基线：`601625d`
- 本文件随 Orchestrator UI 提交；最终 commit 以 `git log -1` 为准。
- 未 push：构建机没有 GitHub 凭据。
