# UI 重构实施报告

> 分支：`feat/ui-redesign`
> 基线：Gateway `fa3abd68`（API 契约冻结）
> 状态：**实现完成，待测试专家全栈验收**；未合并 Pingora，未改 nginx 默认。

## 1. 交付范围

- 新前端：`frontend/gateway/`，React 19 + TypeScript + Vite + TanStack Query/Table + ECharts。
- 保留并重做现有全部页面；新增“安全策略”“代理线路”“静态 NAT”使用后端已有 API。
- 首页重新设计；其余页面保留功能，重做视觉/交互。
- 设计文档：`docs/ui-redesign/01..07`。
- 构建集成：`scripts/build-gateway-ui.sh`、`scripts/build-controller-shell.sh`、`scripts/validate-rootfs-scaffold.sh`。
- 未修改 `frontend/orchestrator`、`backend/`、`packaging/nginx/ly-route-admin.conf`、Pingora 配置。

## 2. 页面清单（旧 → 新）

| 旧页面 ID | 旧标题 | 新路由 | 新页面 | 备注 |
| --- | --- | --- | --- | --- |
| `system/system_overview` | 系统概况 | `/` | 总览 | 合并系统/流量核心信息，新增健康/接口/待办/Top/事件 |
| `dashboard/dashboard` | 流量概况 | `/traffic` | 流量概况 | 保留 KPI、双图、时间窗、出口图例 |
| `monitor/ipobj_main` | 在线用户 | `/clients` | 在线用户 | 保留表格 + 会话抽屉/筛选/分页 |
| `monitor/flow_topn` | Top连接 | `/connections` | 连接排行 | 真实 Top 连接 |
| `monitor/domain_topn` | Top域名 | `/domains` | 域名排行 | 真实 Top 域名 |
| `monitor/interface_list` | 网卡设置 | `/interfaces` | 网卡设置 | 角色配置、创建聚合、端口视图 |
| `network/proxy_main` | LAN/WAN | `/wan` | LAN/WAN | 三 Tab、LAN 接口/桥、WAN 六类型、PPPoE、线路日志 |
| `network/wangroup_manager` | WAN群组 | `/wan-groups` | WAN 群组 | 五种模式、成员/权重/连接数/下行 |
| `route/route_policy_main` | 策略路由 | `/routing` | 策略路由 | Any→Any 默认策略引导、优先级、条件/动作 |
| `route/portmap_list` | 端口映射 | `/port-mapping` | 端口映射 | 端口映射 + 静态 NAT |
| `route/dnspolicy_main` | DNS策略 | `/dns` | DNS 策略 | 策略 + 解析上游 |
| `network/dhcpsvr_main` | DHCP服务 | `/dhcp` | DHCP 服务 | 服务列表 + 静态分配 |
| `flowcontrol/flowct_main` | 流量控制 | `/qos` | 流量控制 | 限速/阻断 + Smart QoS 状态 |
| `object/urlgrp_list` | 域名管理 | `/objects/domains` | 域名管理 | 群组/成员/导入/导出 |
| `object/iptab_list` | IP管理 | `/objects/ips` | IP 管理 | 群组/成员/导入/导出 |
| `system/webuser_main` | 用户管理 | `/system/users` | 用户管理 | 管理员/只读、改密、删除保护 |
| `system/sys_config` | 配置管理 | `/system/runtime`、`/system/config`、`/system/firmware` | 运行态/配置/固件 | 拆分过长页面 |
| 无（后端已有） | 安全 | `/security` | 安全策略 | ACL / IP-MAC / 威胁情报 / 攻击防护 |
| 无（后端已有） | 代理 | `/proxy` | 代理线路 | 出口/节点/订阅/Xray 状态日志 |

旧 hash URL 通过 `LEGACY_ROUTES` 重定向到新路由。

## 3. 真实 API 使用

所有页面均直接请求 `/api/v1/*`，无 mock、无静态假数据。关键页面：

| 页面 | API |
| --- | --- |
| 登录/改密/用户 | `/auth/login`、`/auth/session`、`/auth/change-password`、`/auth/users` |
| 总览 | `/health`、`/dashboard/summary`、`/telemetry/dashboard`、`/telemetry/traffic-trend`、`/telemetry/online-users`、`/telemetry/top-sessions`、`/telemetry/top-domains`、`/telemetry/interfaces`、`/telemetry/audit-events`、`/runtime/status` |
| 在线用户/会话 | `/telemetry/online-users`、`/telemetry/sessions` |
| 网卡 | `/interfaces`、`/interface-bonds`、`/telemetry/interfaces` |
| LAN/WAN | `/interfaces`、`/gateway/wan-links`、`/proxy/egresses`、`/proxy/nodes`、`/proxy/subscriptions`、`/gateway/pppoe/status`、`/telemetry/audit-events` |
| WAN 群组 | `/gateway/wan-groups`、`/gateway/wan-links` |
| 策略路由 | `/gateway/policies/routes`、WAN/群组/代理/对象组 |
| 端口映射/静态 NAT | `/gateway/nat/port-maps`、`/gateway/nat/static` |
| DNS | `/dns/policies`、`/dns/upstreams`、`/objects/groups` |
| DHCP | `/dhcp/servers`、`/dhcp/static-bindings` |
| 流量控制 | `/gateway/traffic-control`、`/flow-control/smart-qos` |
| 对象 | `/objects/groups`、导入/导出 |
| 安全 | `/security/acls`、`/security/ip-mac-bindings`、`/security/threat-intel`、`/security/attack-rules` |
| 代理 | `/proxy/egresses`、`/proxy/nodes`、`/proxy/subscriptions`、`/proxy/xray/status|logs` |
| 运行态 | `/runtime/status`、`/runtime/preview`、`/runtime/apply` |
| 配置 | `/management/network`、`/config/export`、`/config/snapshots`、`/config/factory-reset` |
| 固件 | `/firmware/update/status|stage|install` |

写操作统一：保存 desired state → Toast“已保存，正在应用” → `POST /runtime/apply` → Apply Banner 显示事务号/已用时长/结果。`202 apply_in_progress` 不重复提交；`200/202` 均保留真实状态。

## 4. 保真与不杜撰

- 只显示后端返回字段；未知 `--`，不把不可用显示成 0。
- `desired_not_applied` 显示“待应用”；`degraded/unavailable` 显示原因；`apply_in_progress` 显示事务号，不显示假百分比。
- 路由策略无 `hit_count`：不显示“命中次数”列（Panabit 的命中列未照搬）。
- 会话无“策略/出口归属”：抽屉不展示推断结果；只显示真实源/目的/NAT/协议/时长/字节。
- WAN 无延迟/丢包/DPI/温度/磁盘/运营商：不显示。
- 系统时间只读：后端无写入 API，不保留旧 UI 的假“同步时间”提示。
- 页面明确标注 `request_id`（错误态/运行态），便于后端定位。

## 5. 构建与联调

### 开发

```sh
cd frontend/gateway
pnpm install
pnpm dev -- --port 5174    # 或 pnpm exec vite --host 0.0.0.0 --port 5174
# Vite 将 /api 代理到 http://127.0.0.1:18090，可用 LY_ROUTE_UI_API 覆盖
```

### 生产构建

```sh
cd frontend/gateway
pnpm build
# 产物：dist/index.html、dist/app.js、dist/styles.css、capabilities.json
```

### 打包链路

```sh
cd /root/work/ly-route-ui-redesign
LY_ROUTE_UI_DIST=$PWD/frontend/gateway/dist \
  ./scripts/build-gateway-ui.sh --out /tmp/gw-ui-bundle

./scripts/build-controller-shell.sh --product gateway --out /tmp/gw-ui-controller
```

`build-controller-shell.sh` 的 gateway 分支现在调用 `build-gateway-ui.sh`，不再拼接旧 `app.js`。输出保留 `app.js/styles.css/shell.js/commercial.css/capabilities.json`，以兼容现有升级包 checksum/rootfs 校验；`shell.js` 与 `commercial.css` 为兼容占位文件，不加载业务逻辑。

> 未改 `build-upgrade-package.sh` 的 checksum 列表，因为新构建继续产出同名文件；如后续切换为多 chunk/多 CSS，需要改为遍历 `ui-manifest.json`。

## 6. 验证证据

- 真实后端：独立 control API 沙箱（`127.0.0.1:18090`，临时 DB，`LY_ROUTE_ENABLE_VPP_INTERFACE_TELEMETRY=0`）。
- 真实 UI 写入：网卡角色、LAN、WAN、默认 Any→Any、端口映射、DNS、DHCP、域名组、只读用户均通过浏览器表单创建；API 回读确认。
- 只读账号：`ui-readonly` 登录后写按钮禁用。
- 路由/页面：21 个页面路由均可访问；旧 hash 映射可用。
- 1440×900：21 页截图；1920×1080：总览/流量/策略/网卡/配置截图。
- 控制台：除登录前 `/auth/session` 预期 401 外无错误；无失败请求。
- Lighthouse（登录页）：Accessibility 100、Best Practices 100；SEO 0.6（管理界面已加 `noindex`，不计入验收）。
- 截图路径（构建机）：`/root/work/ui-sandbox/final/`。
- Playwright 脚本：`/root/work/ui-sandbox/final-evidence.cjs`、`e2e-seed.cjs`、`e2e-visual.cjs`。

## 7. 已知缺口 / 后端限制

| 缺口 | 说明 | 处理 |
| --- | --- | --- |
| 系统时间写入 | 后端无 API；旧 UI 仅 toast，不真实生效 | 只读展示，明确说明 |
| 路由命中次数 | 路由 API 无 hit_count | 不显示该列 |
| 会话策略/出口归属 | 会话 API 无字段 | 不显示推断列 |
| VPP 遥测 | 沙箱无 VPP，图表为空/降级 | 真实 router 上应显示采样；空态/降级已覆盖 |
| `runtime/status` 慢 | 后端约 10s | 25s 超时 + 骨架 + 不阻塞首屏 |
| 配置导入确认 | 后端需 dry-run + confirm token；当前 UI 未暴露完整流程 | 保留导出/快照/初始化；导入待产品确认 |
| PPPoE 连接/断开 | 当前旧 UI 也仅状态；未加按钮 | 保持状态/peer 展示 |
| Xray 重启 | 旧 UI 未暴露；未加按钮 | 只读状态/日志 |
| 安全 ACL 默认规则 | 默认 WAN inbound deny 可能存在 | 删除按钮禁用默认规则 |
| LAN 桥 | 后端支持；新 UI 已加 LAN 桥成员选择 | 需真机回归桥接行为 |

## 8. 验收建议

1. 在真实 router（非沙箱）跑 `pnpm build` 产物 + nginx，验证真实 VPP 遥测/会话。
2. 按 `07-acceptance-and-gate.md` 逐页执行；重点看保存→apply→回读、只读权限、慢 status。
3. 确认 `build-controller-shell.sh` 输出可被现有 rootfs/升级包脚本消费（文件同名 + checksum）。
4. 通过后再合并 Pingora；nginx 保持默认与回退。

## 9. 提交

- 分支：`feat/ui-redesign`
- 基线：`fa3abd68`
- 提交：以最终 `git log -1` 为准（本文件随实现提交）。
