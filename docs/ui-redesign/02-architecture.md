# 02 · 技术架构与工程方案

> 目标：在“设备离线、同源 API、静态托管、无 Node 运行时”的约束下，
> 交付可维护、可验收、可回退的 React 控制台。本文只定方案，不写实现。

## 1. 技术选型

| 层 | 选择 | 理由 |
| --- | --- | --- |
| 框架 | React 19 + TypeScript 5.9+ | 生态成熟；TanStack/Radix/ECharts 支持好；团队可替换 |
| 构建 | Vite 7+，`base: '/'` | 静态产物、快速 HMR、按路由拆包 |
| 路由 | TanStack Router v1 | 类型安全路由、懒加载、搜索参数类型化 |
| 服务端状态 | TanStack Query v5 | 缓存、失效、轮询、并发去重、错误重试可控 |
| 客户端状态 | Zustand | 只存 UI 偏好（侧栏/密度/筛选/抽屉），不复制服务端数据 |
| 表格 | TanStack Table v8 | 无头、列可见性/排序/分页/密度可控 |
| 图表 | Apache ECharts 6（按需引入） | 时间轴、双轴、tooltip、图例、性能稳定 |
| 样式 | Tailwind CSS 4 + CSS Variables | token 化；不依赖运行时 CSS-in-JS |
| 无障碍原语 | Radix UI | Dialog/Drawer/Tooltip/Select/Tabs 行为可靠 |
| 表单 | React Hook Form + Zod | 非受控性能好；客户端只做格式校验 |
| 图标 | lucide-react | 线性、中性、无品牌素材 |
| 测试 | Vitest + Testing Library + Playwright + axe | 单测/交互/截图/可访问性 |
| 包管理 | pnpm + 锁文件 | 可复现构建；CI 缓存友好 |

**不选**：Vue/Element Plus（现有栈是原生 JS，迁移到 Vue 没有额外收益）；Ant Design
（视觉语言偏企业后台，难以贴近 iKuai/UniFi 的轻量感）；Next.js（设备不需要 SSR，
增加运行时与打包复杂度）。

版本在实现启动时锁定并提交 `pnpm-lock.yaml`；不在文档中写死小版本，避免与构建机
缓存/镜像不一致。

## 2. 运行与构建约束

| 约束 | 事实 | 方案 |
| --- | --- | --- |
| 静态托管 | nginx/Pingora 从 `/opt/ly-route/admin` 提供文件 | Vite 产物直接放该目录 |
| SPA 回退 | nginx `try_files $uri $uri/ /index.html`；Pingora 同 | 使用 history 路由；直接访问深链可用 |
| 同源 API | `/api/v1/*` 反代到 `127.0.0.1:8080` | `fetch` 同源 + `credentials: same-origin` |
| 离线 | 设备可能无外网 | **禁止** CDN、Google Fonts、远程图标、远程 sourcemap |
| 无 Node | 设备只有静态文件 | 构建在 CI/开发机完成；产物不含 Node |
| 缓存 | 静态资源 7 天；`index.html` 不缓存 | 文件名带 hash；`index.html` 保持无 Cache-Control |
| 升级包 | 现有脚本硬编码 `app.js/shell.js/styles.css/commercial.css` 与 SHA256 | 必须改打包脚本为“整目录 + manifest 哈希”，见 §7 |
| CSP | 当前未配置 | 建议后续由 nginx/Pingora 加 CSP；前端先保证无内联脚本、无外链 |
| 大小 | 现有前端资源最大约 277KB | 新产物预算见 §9；ECharts 按需拆包 |

## 3. 目录结构

建议在现有 `frontend/gateway/` 内演进，保留目录名，避免打包脚本大改：

```text
frontend/gateway/
  package.json
  pnpm-lock.yaml
  vite.config.ts
  tsconfig.json
  index.html
  public/
    favicon.svg                 # 自有图形，不使用第三方商标
  src/
    main.tsx
    app/
      router.tsx                # 路由树、懒加载、旧 hash 兼容
      providers.tsx             # QueryClient、Theme、Toast、ErrorBoundary
      shell/
        AppShell.tsx            # 侧栏 + 顶栏 + 内容区
        Sidebar.tsx
        Topbar.tsx
        PageHeader.tsx
        CommandPalette.tsx      # Ctrl/Cmd+K 页面/动作搜索
      guards/
        RequireSession.tsx
        RequireAdmin.tsx
    pages/
      login/
      overview/
      traffic/
      clients/
      connections/
      domains/
      interfaces/
      wan/
      wan-groups/
      dhcp/
      routing/
      port-mapping/
      dns/
      qos/
      objects/
      system/
    features/
      auth/
      telemetry/
      interfaces/
      wan/
      routing/
      nat/
      dns/
      dhcp/
      qos/
      objects/
      runtime/
      config/
      firmware/
    api/
      client.ts                 # fetch 封装、错误、超时、request_id
      endpoints.ts              # 所有路径集中定义
      types.ts                  # 后端 JSON 的 TS 类型（按页面拆分亦可）
      queries/
        keys.ts
        useHealth.ts
        useTelemetry.ts
        useResources.ts
        useRuntime.ts
      mutations/
        useDesiredMutation.ts
        useApply.ts
      contract/
        parse.ts                # zod 解析 + 未知字段透传
    components/
      ui/                       # Button/Input/Select/Tabs/Badge/Tooltip...
      data/
        DataTable.tsx
        TableToolbar.tsx
        ColumnChooser.tsx
        Pagination.tsx
        EmptyState.tsx
        ErrorState.tsx
        Skeleton.tsx
      charts/
        TrafficChart.tsx
        Sparkline.tsx
        chartTheme.ts
      feedback/
        Toast.tsx
        ConfirmDialog.tsx
        ApplyBanner.tsx
      forms/
        FormSection.tsx
        AddressConditionEditor.tsx
        PasswordField.tsx
        DirtyGuard.tsx
    stores/
      uiStore.ts
      sessionStore.ts
      applyStore.ts
    styles/
      tokens.css
      globals.css
    lib/
      format.ts                 # bps/bytes/time/duration
      errors.ts                 # 后端错误 → 用户文案
      validation.ts
      routeLegacy.ts            # 旧页面 ID → 新路由
  tests/
    unit/
    e2e/
    visual/
      baseline/
```

> `frontend/gateway` 仍是网关 UI 源目录；`frontend/orchestrator` 不动。
> 构建产物不放源码目录，统一 `dist/`，由脚本复制到 rootfs。

## 4. API Client 设计

### 4.1 统一入口

```ts
type ApiErrorBody = {
  error?: { code?: string; message?: string; request_id?: string; capability?: string };
};

class ApiError extends Error {
  status: number;
  code: string;
  requestId?: string;
  body?: unknown;
}

async function apiFetch<T>(path: string, init?: RequestInit & { timeoutMs?: number }): Promise<T>;
```

规则：

- 默认 `credentials: 'same-origin'`，`headers.Accept = 'application/json'`；JSON 写入才加 `Content-Type`。
- 默认超时：普通 GET/POST `15s`；`runtime/status` `20s`（后端约 10s，留余量）；
  `runtime/apply` `620s`（后端 apply 超时 600s）；固件上传按文件大小单独设置。
- `AbortController` 支持页面卸载/参数变化取消；`AbortError` 不弹错误。
- 非 2xx：解析后端 `{"error":{code,message,request_id}}`；解析失败时用 HTTP 状态兜底。
- 所有请求/响应记录 `request_id`（响应体或 `X-Request-ID` 头）；错误面板可复制。
- **不**自动重试写操作；GET 可对 502/503/504 做一次短退避重试。
- 401：清 session store，跳登录；保留 `returnTo`，登录后回原页。
- 403：提示“当前账号只读/无权限”，不跳转。
- 422：展示后端 message；若字段可映射则标红，否则顶部表单错误。
- 202：不是失败。进入 Apply 事务状态机（见 05 文档）。
- 204：返回 `undefined`，调用方按资源刷新处理。

### 4.2 响应封套

后端常见形态（实现时用 zod 解析并 `passthrough()` 保留新字段）：

```ts
type Collection<T> = { items: T[]; capabilities?: Capability[]; request_id?: string };
type Item<T> = { item: T; capabilities?: Capability[]; request_id?: string };
type Data<T> = { data: T; capabilities?: Capability[]; request_id?: string };
type Capability = { name: string; available: boolean; state: string; reason?: string };
type ErrorEnvelope = { error: { code: string; message: string; request_id?: string; capability?: string } };
```

### 4.3 端点集中定义

`api/endpoints.ts` 只放路径与查询参数构造，不写业务：

```ts
export const api = {
  auth: { session: '/api/v1/auth/session', login: '/api/v1/auth/login', ... },
  health: '/api/v1/health',
  capabilities: '/api/v1/capabilities',
  summary: '/api/v1/dashboard/summary',
  telemetry: {
    dashboard: '/api/v1/telemetry/dashboard',
    interfaces: '/api/v1/telemetry/interfaces',
    trafficTrend: (window: TrafficWindow, points: number) =>
      `/api/v1/telemetry/traffic-trend?window=${encodeURIComponent(window)}&points=${points}`,
    ...
  },
  resources: {
    interfaces: '/api/v1/interfaces',
    wanLinks: '/api/v1/gateway/wan-links',
    ...
  },
  runtime: { status: '/api/v1/runtime/status', preview: '/api/v1/runtime/preview', apply: '/api/v1/runtime/apply' },
} as const;
```

### 4.4 查询键与刷新

```ts
const qk = {
  session: ['session'],
  health: ['health'],
  capabilities: ['capabilities'],
  summary: ['dashboard', 'summary'],
  telemetry: (kind: string, params?: unknown) => ['telemetry', kind, params],
  resources: (kind: string) => ['resources', kind],
  resource: (kind: string, id: string) => ['resources', kind, id],
  runtime: ['runtime', 'status'],
  preview: ['runtime', 'preview'],
  audit: ['telemetry', 'audit'],
};
```

建议的 `staleTime` / `refetchInterval`（可调）：

| 数据 | staleTime | 轮询 | 说明 |
| --- | --- | --- | --- |
| session | 30s | 无 | 失效即 401 跳登录 |
| health / capabilities | 10s | 15s | 依赖状态变化不频繁 |
| dashboard summary | 5s | 10s | CPU/内存/负载 |
| traffic trend | 5s | 10s | 跟随所选时间窗；页面隐藏时停 |
| interfaces telemetry | 2s | 5s | 实时速率 |
| online users | 5s | 10s | 数据来自租约+邻居 |
| top sessions/domains | 5s | 15s | 排行不宜过频 |
| runtime status | 10s | 15s | 后端慢；不要和首屏关键请求串行 |
| resources | 30s | 不自动 | 写操作后 invalidate |
| audit | 30s | 30s | 低频 |
| firmware/proxy/pppoe | 10s | 15s | 配置页按需 |

并发控制：

- 页面级 `useQueries` 并行；不要 `await` 串行拉取。
- 自动刷新在 `document.hidden`、弹窗/抽屉打开、表单有焦点/未保存时暂停。
- 同一 query 的多个组件共享；使用 `select` 只订阅所需字段，避免大对象重渲染。
- 写操作成功后：`invalidateQueries(resources(kind))` + 触发 apply；不直接手改缓存（除
  立即反馈的临时行，需在 readback 后替换）。

## 5. 状态管理

### 5.1 服务端状态（TanStack Query）

- 所有 API 数据都在 Query Cache；**不**复制到 Zustand/Context。
- 页面只通过 hooks 读数据；卸载后缓存保留 `gcTime: 5min`。
- `placeholderData: keepPreviousData` 用于分页/筛选，避免闪烁。
- 失败保留旧数据并显示“数据可能过期 + 重试”，不要清空表格。

### 5.2 客户端 UI 状态（Zustand）

```ts
type UiState = {
  sidebarCollapsed: boolean;
  density: 'compact' | 'default' | 'comfortable';
  trafficWindow: 'realtime' | '5m' | '1h' | '24h';
  hiddenEgressIds: string[];
  tablePrefs: Record<string, { columns: string[]; pageSize: number }>;
  commandOpen: boolean;
  drawer: { kind: 'client' | 'session' | null; id?: string };
};
```

持久化到 `localStorage` 的只有 UI 偏好（键前缀 `lyroute.ui.`），**不存**会话、密码、token、
完整 API 响应。清空偏好不影响功能。

### 5.3 会话状态

- 启动 `GET /auth/session`；成功存 `{username, role, passwordChangeRequired}`。
- `role === 'readonly'`：隐藏/禁用所有写操作；直接访问写路由显示 403 页。
- `passwordChangeRequired`：强制打开改密弹窗；改密成功前不允许进入主界面（保留退出）。
- 登出调用 `/auth/logout` 后清缓存并回登录页；失败也清本地状态（Cookie 由后端清）。

### 5.4 Apply 事务状态机

```text
idle
  └─ save() → saving
       ├─ 失败 → save_failed（保留表单，展示 error.request_id）
       └─ 成功 → saved_pending（显示“已保存，待应用”）
            └─ apply() → applying（POST /runtime/apply）
                 ├─ 200 committed/running → applied
                 ├─ 202 apply_in_progress → applying（继续轮询 /runtime/status）
                 ├─ 202 dataplane_locked / apply_failed → degraded
                 └─ 超时/网络错误 → unknown（继续查询 status，不重复提交）
```

约束：

- 同一时间只允许一个 apply；再次触发时复用已有 `transaction_id` 并提示。
- `apply` 期间禁用表单提交/删除/角色变更；允许取消“等待”但不取消后端事务。
- 不显示百分比；显示事务号、开始时间、已用时长、当前阶段（来自 status/components）。
- 刷新页面后从 `runtime/status.last_apply` 恢复显示，不依赖内存。

## 6. 表格 / 图表 / 表单

### 6.1 表格

- `DataTable` 基于 TanStack Table，列定义包含：`id, header, accessor, cell, priority, align, width, sortable, hideable`。
- 默认客户端排序/筛选/分页：后端列表接口没有分页参数，当前数据量（接口/规则/租约）可控。
- 分页默认 20；选项 20/50/100；页脚显示“共 N 条”；空表不渲染占位行。
- 列可见性：每表独立持久化；`priority: 1` 在 1440 必显，`priority: 3` 在 1920 才默认显示。
- 操作列固定右侧；长表格横向滚动时加左/右阴影；表头 sticky。
- 数字右对齐、等宽数字；IP/MAC/端口用 mono；状态用 Badge（颜色+文字，不只靠颜色）。
- 行点击用于“打开详情”（在线用户/会话），操作按钮 `stopPropagation`；批量选择用复选框。
- 批量操作只在后端有对应批量语义时开放；当前多数接口没有 batch 端点，UI 不伪造“批量成功”。

### 6.2 图表

- `echarts/core` + `LineChart` + `GridComponent` + `TooltipComponent` + `LegendComponent` + `DataZoom`（按需）。
- 颜色来自 token；每个逻辑出口固定颜色，隐藏后不重新分配颜色（避免图例跳色）。
- 缺失采样：`null` 断线，不跨越 gap 插值。
- Tooltip：时间 + 每个出口的上/下行 + 合计；数值带单位；无数据时显示“该时刻无采样”。
- 图例可点击开关；状态点显示 health（healthy/degraded/unknown），不只靠颜色。
- 刷新时 `setOption` 更新 series，不重建实例；`ResizeObserver` 处理容器变化。
- 空态：无出口/无采样分别显示不同文案；降级时图表上方显示 reason。
- 可访问性：图表容器 `role="img"` + `aria-label`；提供“查看数据表”折叠表格（真实采样）。

### 6.3 表单

- 所有表单分节：基本信息 / 匹配条件 / 执行动作 / 高级（默认折叠）。
- 地址条件编辑器支持：IPv4/IPv6 前缀、IPv4 范围、IP 组；空 = 任意，UI 明确显示“任意”。
- 端口：单端口或范围；协议 TCP/UDP/ICMP/Any（按后端支持）。
- 密码：不回填；显示“已保存（不显示）”；修改时留空表示不变。
- 必填校验：前端只做非空/范围/格式；业务约束（依赖、冲突、端口占用）以后端 422 为准。
- 提交：`保存并应用` 主按钮；`仅保存` 次按钮（是否保留由产品确认；默认两态）。
- 脏表单：关闭弹窗/切换路由前确认；提交中禁止关闭。
- 错误：顶部汇总 + 字段就近提示；保留用户输入；显示 `request_id` 可复制。

## 7. 构建与打包集成（关键）

当前 `scripts/build-controller-shell.sh` 对网关 UI 做了硬编码：

- 必需 `index.html`、`styles.css`、`shell.js`；
- `app.js` 由多个源文件 `cat` 拼接；
- `build-upgrade-package.sh` 对 `app.js`、`shell.js`、`styles.css`、`commercial.css`、`capabilities.json`、`index.html` 逐个计算 SHA256；
- `validate-rootfs-scaffold.sh` 要求这些文件名存在。

新构建不能继续 `cat`，必须改成“目录 + manifest”。建议步骤（实现阶段执行）：

1. 新增 `scripts/build-gateway-ui.sh`：
   - `pnpm install --frozen-lockfile`
   - `pnpm build`
   - 复制 `packaging/product-profiles/gateway.json` 为 `dist/capabilities.json`
   - 删除 `.map`（或仅 CI 保留）
   - 扫描 `dist` 中是否含 `http://`/`https://` 外链、`mock`、第三方商标字符串
   - 生成 `dist/ui-manifest.json`：`{ files: [{path, sha256, bytes}], built_at, source_commit }`
2. 修改 `scripts/build-controller-shell.sh`：
   - `gateway` 分支改为调用 `build-gateway-ui.sh`，不再 `cat app.js`；
   - `orchestrator` 分支保持现状，避免连带风险。
3. 修改 `build-rootfs.sh` / `build-upgrade-package.sh`：
   - 以 `ui-manifest.json` 为准校验/写入 `checksums.sha256`；
   - 删除对 `app.js/shell.js/styles.css/commercial.css` 的硬编码；
   - `manifest.json.checksums` 改为遍历 UI 文件。
4. 修改 `validate-rootfs-scaffold.sh`：
   - 只要求 `index.html`、`capabilities.json`、至少一个 `assets/*.js`、至少一个 `assets/*.css`；
   - 保留 `Panabit`/`assets/`/`VRRP` 等禁用词检查，新增 `iKuai`、`Ubiquiti`、`UniFi` 商标检查；
   - 保留 “production admin UI must not load mock-api.js”。
5. `packaging/nginx/ly-route-admin.conf` 与 `edge.toml` 的 `static.root` 不变；
   SPA 回退已支持，无需改代理。
6. 过渡期可让 Vite 额外输出一个 `app.js` 兼容入口，但**不要**长期保留双份代码；
   验收通过后删除硬编码。

> 此节是 UI 实现能否进入 rootfs/升级包的关键。未完成前，即使页面在开发机可用，
> 也不能进入发布链路。

## 8. 路由与兼容

- 新路由使用 history 模式（`/interfaces` 等）；nginx/Pingora 的 SPA fallback 已覆盖。
- 旧 URL 兼容：
  - `#system/system_overview` → `/`
  - `#dashboard/dashboard` → `/traffic`
  - `#monitor/ipobj_main` → `/clients`
  - `#monitor/flow_topn` → `/connections`
  - `#monitor/domain_topn` → `/domains`
  - `#monitor/interface_list` → `/interfaces`
  - `#network/proxy_main` → `/wan`
  - `#network/wangroup_manager` → `/wan-groups`
  - `#route/route_policy_main` → `/routing`
  - `#route/portmap_list` → `/port-mapping`
  - `#route/dnspolicy_main` → `/dns`
  - `#network/dhcpsvr_main` → `/dhcp`
  - `#flowcontrol/flowct_main` → `/qos`
  - `#object/urlgrp_list` → `/objects/domains`
  - `#object/iptab_list` → `/objects/ips`
  - `#system/webuser_main` → `/system/users`
  - `#system/sys_config` → `/system/runtime`（配置/升级拆分为子页）
- 未知路由 → `NotFound`，不静默回首页。
- 页面懒加载；首屏只加载 Shell + 登录/首页依赖。

## 9. 性能预算

| 指标 | 预算 | 测量 |
| --- | --- | --- |
| 首屏 JS（gzip） | ≤ 350 KB | `vite build --report` / `rollup-plugin-visualizer` |
| 首屏 CSS（gzip） | ≤ 60 KB | 构建报告 |
| 图表 chunk（gzip） | ≤ 450 KB，懒加载 | 构建报告 |
| 首页 LCP（实验室 LAN） | ≤ 1.5s | Playwright trace / Lighthouse |
| 路由切换 | ≤ 300ms 可交互 | Performance panel |
| 表格 1000 行首屏 | ≤ 200ms 渲染 | 基准测试；超过则虚拟化 |
| 图表刷新 | 不重建实例，≤ 100ms/帧 | Performance panel |
| API 并发 | 首页关键请求 ≤ 6 个并行 | 网络面板 |
| CLS | < 0.1 | Lighthouse |

## 10. 安全与隐私

- 会话 Cookie `ly_route_session` 为 HttpOnly/SameSite=Strict；JS 不读取、不写 localStorage。
- 所有写操作同源；不引入 CSRF token（后端当前未提供），依赖 SameSite + 同源；若后续加
  CSRF token，API client 预留 header hook。
- 禁止 `dangerouslySetInnerHTML`；日志/错误/备注按文本渲染；长文本截断 + title。
- 上传：固件仅 `.tar.zst/.zst`，对象组仅文本/GeoSite/GeoIP；前端限制大小并显示进度，
  后端仍为最终校验。
- 不在前端保存代理节点 URI/密码；编辑时留空表示不变，列表只显示脱敏信息（后端已脱敏）。
- 错误面板可复制 `request_id`，但不得包含 Cookie、密码、节点 URI。
- 不接入任何外部统计/错误上报；控制台错误只在本机。

## 11. 测试策略

| 层 | 工具 | 覆盖 |
| --- | --- | --- |
| 单元 | Vitest | 格式化、校验、错误映射、apply 状态机、旧路由映射 |
| 组件 | Testing Library | 表格空/错/载入、弹窗焦点、表单校验 |
| 契约 | Vitest + zod | 用真实 API 响应样本解析；字段缺失/类型变化即失败 |
| E2E | Playwright | 登录、首页、每页 CRUD、apply、会话抽屉、401/403/422、慢 status |
| 视觉 | Playwright screenshot | 1440×900、1920×1080；每页默认/空/错/暗色（如启用） |
| 可访问性 | axe-core | 每页 critical/serious 为 0 |
| 打包 | shell + rootfs smoke | `ui-manifest.json`、checksums、无 mock/外链 |

测试数据：

- 单元/组件可用 MSW/fixture；**生产构建不得包含 mock**。
- E2E 必须打真实 lab API；配置写入走真实 UI；证据采集只读 API。
- 视觉基线在 UI 门禁解除后生成；每次变更需人工确认 diff。

## 12. 实施顺序（门禁解除后）

1. 脚手架 + tokens + Shell + 登录 + 首页骨架（真实 API）。
2. 通用 DataTable/Toolbar/Empty/Error/Skeleton/Badge/Tabs/Dialog/Drawer。
3. 在线用户 + 会话抽屉（验证高密度表格与筛选）。
4. 网卡设置 + LAN/WAN（验证复杂表单/角色/PPPoE/代理）。
5. 策略路由 + 端口映射 + DNS + DHCP（验证优先级/条件编辑器）。
6. WAN 群组 + 流量控制 + 对象管理。
7. 运行态/配置/固件 + 用户管理。
8. 打包脚本改造 + rootfs/升级包冒烟。
9. 全量视觉/交互/可访问性/真实 API 验收。
10. 合并 Pingora（前端验收后），nginx 保留默认与回退。

每一步都以“真实 API + 真实 UI 写入 + 回读”收口，不积压到最后。
