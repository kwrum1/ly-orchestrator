# 01 · 真实功能清单与页面地图

> 目标：把“现有 UI 能看到什么”“后端真正提供什么”“哪些数据根本不存在”分开，
> 作为后续实现和验收的唯一功能基线。

## 1. 核对方法

1. 读 `backend/internal/httpapi/product_routes.go`，枚举全部 `/api/v1/*` 路由。
2. 读 `frontend/gateway/app.js`、`gateway-overview.js`、`shell.js`，枚举现有页面、Tab、弹窗、抽屉和实际请求。
3. 读 `backend/internal/product/profile.go`，确认 Gateway 的 22 项 capability 全开。
4. 读 `docs/acceptance/functional-vm-lab.md`，确认 UI 阶段范围与验收顺序。
5. 对照 `evidence/*.png`、`evidence/ui-pages.json`、`evidence/ui-diag.json` 核实当前页面真实状态。
6. 对任何后端没有返回的字段，一律标记“不可用”，不进入设计。

## 2. 后端能力总览

Gateway profile 的 capability（`profile.go`）：

`product, health, auth, management, interfaces, gateway_mode, object_groups, gateway_wan, gateway_pppoe, gateway_routing, gateway_nat, proxy, dns, dhcp, security, traffic_control, runtime, config, firmware, dashboard, telemetry, top_domains`

> 注意：**能力存在 ≠ 当前 UI 已暴露 ≠ 当前数据可用**。例如 `security` 有 API，
> 但当前菜单没有独立安全页；`top_domains` 有 API，但需要 telemetry 采集器可用。

## 3. API 清单（按用途）

### 3.1 会话 / 产品 / 健康

| 方法 | 路径 | 用途 | 当前 UI 使用 |
| --- | --- | --- | --- |
| POST | `/api/v1/auth/login` | 登录，设置 `ly_route_session` Cookie | 登录页 |
| POST | `/api/v1/auth/logout` | 登出 | 顶栏退出 |
| GET | `/api/v1/auth/session` | 当前会话 | 启动校验 |
| POST | `/api/v1/auth/change-password` | 修改密码 | 首次登录/用户菜单 |
| GET/POST | `/api/v1/auth/users` | 用户列表/新增 | 用户管理 |
| PATCH/DELETE | `/api/v1/auth/users/{username}` | 改角色/改密/删除 | 用户管理 |
| GET | `/api/v1/product` | 产品标识 | 未见独立使用 |
| GET | `/api/v1/health` | 依赖健康 | 首页/配置页 |
| GET | `/api/v1/capabilities` | 能力状态 | 全局 |
| GET | `/api/v1/mode` | 网关模式 | 未见独立使用 |
| POST | `/api/v1/mode/initialize` | 模式初始化 | 未见独立使用 |

### 3.2 概览 / 遥测

| 方法 | 路径 | 返回要点 | 当前 UI 使用 |
| --- | --- | --- | --- |
| GET | `/api/v1/dashboard/summary` | `status, degraded, dependencies, system{cpu_busy_percent, memory_*, load_average, uptime_seconds, system_time, platform}, capabilities` | 系统概况 |
| GET | `/api/v1/telemetry/dashboard` | `sessions, throughput_bps, runtime_state, degraded, components, last_apply` | 流量概况 KPI |
| GET | `/api/v1/telemetry/interfaces` | `items[]`：接口运行态、地址、link、speed、rx/tx bps/pps、sessions、capability | 网卡设置/首页 |
| GET | `/api/v1/telemetry/traffic-trend?window=&points=` | `totals{download_bps,upload_bps}`, `series.logical_egresses[]`（id/name/kind/health/state/samples） | 首页/流量概况 |
| GET | `/api/v1/telemetry/top-sessions` | `data.items[]`，连接数/字节等 | Top 连接 |
| GET | `/api/v1/telemetry/sessions` | `data.items[]`：源/目的/NAT/协议/端口/字节/时长/状态 | 在线用户抽屉 |
| GET | `/api/v1/telemetry/top-domains` | 域名、最后访问、命中次数 | Top 域名 |
| GET | `/api/v1/telemetry/online-users` | DHCP 租约 + 邻居合成的在线用户 | 在线用户/首页 |
| GET | `/api/v1/telemetry/policy-hits` | 策略命中（后端存在；当前无独立页面） | 未见独立使用 |
| GET | `/api/v1/telemetry/audit-events` | 审计事件列表 | 线路日志/未独立展示 |

### 3.3 接口 / LAN / WAN / PPPoE / 代理

| 方法 | 路径 | 关键字段 | 当前 UI |
| --- | --- | --- | --- |
| GET/POST | `/api/v1/interfaces` | `id, name, system_name, gateway_role, work_mode, active_path, addresses, cidr, display_name, mtu, link_state, speed, stats, capability` | 网卡设置 |
| GET/PATCH/DELETE | `/api/v1/interfaces/{id}` | 角色、地址、bridge 等 | 角色配置 |
| GET/POST | `/api/v1/interface-bonds` | 聚合组与成员 | 创建聚合 |
| GET/POST | `/api/v1/gateway/wan-links` | `id, name, interface_id, type/wan_type, cidr, gateway, mtu, username/password, ipv4/ipv6, smart_qos_*_kbps, description` | LAN/WAN |
| GET/POST | `/api/v1/gateway/wan-groups` | `name, load_balance_mode/mode, hash_fields, members/wan_members, weights, connection_limits, downlink_mbps, primary_member, backup_member` | WAN 群组 |
| GET | `/api/v1/gateway/pppoe/status` | `status, peers[]`（peer/interface/state/route_handoff） | 配置页 |
| POST | `/api/v1/gateway/pppoe/connect|disconnect` | PPPoE 生命周期 | 未见独立按钮 |
| GET/POST | `/api/v1/proxy/egresses` | 代理出口、承载 WAN、节点/订阅绑定 | 代理 WAN |
| GET/POST | `/api/v1/proxy/nodes` | 节点 URI/协议 | 代理 WAN |
| GET/POST | `/api/v1/proxy/subscriptions` | 订阅 URL、选择策略、top_n、node_refs | 代理 WAN |
| GET | `/api/v1/proxy/xray/status|logs` | Xray 运行态/日志 | 配置页 |
| POST | `/api/v1/proxy/xray/restart` | 重启 Xray | 未见独立按钮 |

### 3.4 策略 / NAT / DNS / DHCP

| 方法 | 路径 | 关键字段 | 当前 UI |
| --- | --- | --- | --- |
| GET/POST | `/api/v1/gateway/policies/routes` | `priority(1-65535), action(nat|route), match{sources,destinations,src_port,dst_port,domain_group}, egress, next_hop, full_cone, enabled, name` | 策略路由 |
| GET/POST | `/api/v1/gateway/nat/port-maps` | `name, wan_link, external_address, external_port, internal_host, internal_port, protocol, hairpin, full_cone, enabled, description` | 端口映射 |
| GET/POST | `/api/v1/gateway/nat/static` | 静态 NAT | **未暴露** |
| GET/POST | `/api/v1/dns/policies` | `priority, name, policy{miss,rules[]}`；rule 含 source_prefixes/domain_set_ids/outcome | DNS 策略 |
| GET/POST | `/api/v1/dns/upstreams` | `servers[], bootstrap_*, wan_egress_id, cache, ttl` | DNS 策略内部 |
| GET/POST | `/api/v1/dns/domain-ip-sets` | 域名 IP 集 | **未暴露** |
| GET/POST | `/api/v1/dhcp/servers` | `interface_id, subnet, pools[], routers[], name_servers[], lease_time_seconds` | DHCP 服务 |
| GET/POST | `/api/v1/dhcp/static-bindings` | MAC/IP/备注 | DHCP 静态分配 |
| GET | `/api/v1/dhcp/leases` | Kea 租约 | 间接用于在线用户 |
| GET | `/api/v1/dhcp/leases/{...}` | 单租约 | 未见独立使用 |

### 3.5 对象 / 流量控制 / 安全

| 方法 | 路径 | 关键字段 | 当前 UI |
| --- | --- | --- | --- |
| GET/POST | `/api/v1/objects/groups` | `kind(ip|domain), name, members[], source{format,category}`, `enabled` | 域名/IP 管理 |
| POST | `/api/v1/objects/groups/{id}/import` | multipart 文件、format、category、mode | 对象组导入 |
| GET | `/api/v1/objects/groups/{id}/export` | 导出成员 | 导出按钮 |
| GET/POST | `/api/v1/gateway/traffic-control` | `rules[]`：match、direction、policer/drop | 流量控制 |
| GET | `/api/v1/flow-control/smart-qos` | `runtime_state, selected_dataplane_tier, reason` | 流量控制页顶部 |
| GET/POST | `/api/v1/security/acls` | ACL | **未暴露** |
| GET/POST | `/api/v1/security/ip-mac-bindings` | IP/MAC 绑定 | **未暴露** |
| GET/POST | `/api/v1/security/threat-intel` | 威胁情报 | **未暴露** |
| GET/POST | `/api/v1/security/attack-rules` | 攻击规则 | **未暴露** |
| GET | `/api/v1/security/runtime` | 安全运行态 | **未暴露** |

### 3.6 运行态 / 配置 / 固件

| 方法 | 路径 | 关键字段 | 当前 UI |
| --- | --- | --- | --- |
| GET | `/api/v1/runtime/status` | `status, components[], last_apply, request_id` | 配置页/首页 |
| GET | `/api/v1/runtime/preview` | `plan`：dns_policies、service_artifacts、vpp_operations、dhcp_servers、pppoe_peers、nftables_tproxy_plan、linux_policy_routing_plan、warnings | 配置页 |
| POST | `/api/v1/runtime/apply` | 200/202；`status, runtime_state, reason, transaction_id, components, rollback*` | 保存后自动触发 |
| POST | `/api/v1/config/apply` | 配置事务 | 配置页（可能） |
| GET | `/api/v1/config/export` | 配置导出 | 未见独立入口 |
| POST | `/api/v1/config/import` | `dry_run, payload` | 未见独立入口 |
| GET | `/api/v1/config/snapshots` | 快照列表 | **未暴露** |
| POST | `/api/v1/config/snapshots/{id}` | 恢复快照 | **未暴露** |
| POST | `/api/v1/config/factory-reset` | 配置初始化 | 配置页 |
| GET | `/api/v1/firmware/update/status` | staged/状态 | 配置页 |
| POST | `/api/v1/firmware/update/stage` | 上传升级包 | 配置页 |
| POST | `/api/v1/firmware/update/install` | 安装/重启 | 配置页 |
| GET/POST | `/api/v1/management/network` | 管理口接口/CIDR/网关 | 配置页 |

## 4. 当前页面地图（现状）

| 分组 | 页面 ID | 标题 | Tab / 主要交互 | 真实数据源 |
| --- | --- | --- | --- | --- |
| 系统概况 | `system/system_overview` | 系统概况 | 资源卡、设备信息、WAN 趋势 | `/dashboard/summary`, `/telemetry/traffic-trend` |
| 系统概况 | `dashboard/dashboard` | 流量概况 | KPI、上下行双图、时间窗、出口图例 | `/telemetry/dashboard`, `/telemetry/traffic-trend`, `/telemetry/online-users` |
| 系统概况 | `monitor/ipobj_main` | 在线用户 | 表格、点击 IP 打开会话抽屉（筛选/分页） | `/telemetry/online-users`, `/telemetry/sessions`, `/telemetry/top-sessions` |
| 系统概况 | `monitor/flow_topn` | Top连接 | 表格 | `/telemetry/top-sessions` |
| 系统概况 | `monitor/domain_topn` | Top域名 | 表格 | `/telemetry/top-domains` |
| 网络设置 | `monitor/interface_list` | 网卡设置 | 接口表、角色配置弹窗、创建聚合 | `/interfaces`, `/telemetry/interfaces`, `/interface-bonds` |
| 网络设置 | `network/proxy_main` | LAN/WAN | Tab：LAN接口 / WAN接口 / 线路日志；新增/编辑/删除 | `/interfaces`, `/gateway/wan-links`, `/proxy/egresses`, `/proxy/nodes`, `/proxy/subscriptions`, `/telemetry/audit-events` |
| 网络设置 | `network/wangroup_manager` | WAN群组 | 群组表、成员/权重/主备 | `/gateway/wan-groups`, `/gateway/wan-links` |
| 网络设置 | `route/route_policy_main` | 策略路由 | IPv4 表、优先级、匹配条件、NAT/路由 | `/gateway/policies/routes` |
| 网络设置 | `route/portmap_list` | 端口映射 | 映射表、新增/编辑/删除 | `/gateway/nat/port-maps` |
| 网络设置 | `route/dnspolicy_main` | DNS策略 | 域名组、解析线路、上游、拉黑 | `/dns/policies`, `/dns/upstreams`, `/objects/groups` |
| 网络设置 | `network/dhcpsvr_main` | DHCP服务 | Tab：服务列表 / 静态分配 | `/dhcp/servers`, `/dhcp/static-bindings` |
| 行为管理 | `flowcontrol/flowct_main` | 流量控制 | 限速/阻断、方向、速率、Smart QoS 状态 | `/gateway/traffic-control`, `/flow-control/smart-qos` |
| 对象管理 | `object/urlgrp_list` | 域名管理 | Tab：域名群组 / 域名展示；导入/导出 | `/objects/groups` |
| 对象管理 | `object/iptab_list` | IP管理 | Tab：IP群组 / IP展示；导入/导出 | `/objects/groups` |
| 系统维护 | `system/webuser_main` | 用户管理 | 用户表、新增、改密、删除 | `/auth/users` |
| 系统维护 | `system/sys_config` | 配置管理 | 管理口、运行态预览/应用、Xray、PPPoE、配置初始化、系统时间、固件升级 | 见 3.6 |

现有 UI 的明确定位：

- 顶栏：产品标识、退出；左侧：菜单搜索 + 分组菜单；无全局搜索、无健康总览、无面包屑。
- 首页：系统概况与流量概况分成两页，信息重复；WAN 趋势占满大面积，但缺少接口/健康/待处理事项。
- 表格：固定高度、空表也显示分页；无列设置、无排序、无行密度切换；批量按钮在部分页存在但无实际批量接口。
- 弹窗：统一单栏，复杂策略表单拥挤；无抽屉（除在线用户会话抽屉）。
- 状态：`desired_not_applied`、`degraded`、`apply_in_progress` 有部分文字，但缺少统一的“待应用/降级/执行中”全局提示。
- 当前截图基线：`evidence/01-login.png`、`evidence/d01-overview-after-load.png`、`evidence/d03-interfaces.png`、`evidence/e03-wan.png`、`evidence/page-route_route_policy_main.png`、`evidence/page-flowcontrol_flowct_main.png`、`evidence/page-system_sys_config.png` 等。

## 5. 目标页面地图（IA 建议）

> 只重排分组与视觉，**不删除现有页面，不新增后端功能**。旧 URL 保留 301/别名，
> 避免书签和外部脚本失效。

```text
概览
  /                         总览（新首页，合并系统概况 + 流量概况的核心信息）

监控
  /traffic                  流量概况（保留原双图/KPI/时间窗）
  /clients                  在线用户（表格 + 会话抽屉）
  /connections              Top 连接（原 Top连接）
  /domains                  Top 域名（原 Top域名）

网络
  /interfaces               网卡设置（端口视图 + 表格 + 角色/聚合）
  /wan                      LAN/WAN（LAN接口 / WAN接口 / 线路日志）
  /wan-groups               WAN 群组
  /dhcp                     DHCP 服务（服务列表 / 静态分配）

策略与对象
  /routing                  策略路由（IPv4）
  /port-mapping             端口映射
  /dns                      DNS 策略
  /qos                      流量控制
  /objects/domains          域名管理（域名群组 / 域名展示）
  /objects/ips              IP 管理（IP 群组 / IP 展示）

系统
  /system/runtime           运行态（status / preview / apply / last apply）
  /system/config            配置管理（管理口 / 配置初始化 / 系统时间）
  /system/firmware          固件升级
  /system/users             用户管理
```

IA 变更说明：

- “系统概况”与“流量概况”不再作为两个首页；首页是新总览，`/traffic` 保留完整流量分析。
- “策略路由 / 端口映射 / DNS / 流量控制 / 对象组”归入“策略与对象”，符合 iKuai 4.0 的“网络配置 + 路由对象”思路，也避免“网络设置”过载。
- “运行态 / 配置 / 固件 / 用户”拆成系统子页；当前配置页内容过多，必须拆分。
- 所有旧页面仍可通过别名访问；实现时先做重定向，不删除代码路径。

## 6. 功能矩阵（页面 → 操作 → API → 证据）

| ID | 页面 | 操作 | API | 成功证据 |
| --- | --- | --- | --- | --- |
| F-01 | 登录 | 输入用户名/密码 | POST `/auth/login` | 200 + Set-Cookie + 跳转首页 |
| F-02 | 登录 | 首次登录改密 | POST `/auth/change-password` | 200 + 新会话 + 新密码可登录 |
| F-03 | 首页 | 读取健康/资源/运行态 | GET `/health`, `/dashboard/summary`, `/runtime/status` | 页面数值与 API 一致 |
| F-04 | 首页 | 切换流量时间窗 | GET `/telemetry/traffic-trend` | 图表时间轴/采样点变化 |
| F-05 | 首页 | 打开待处理应用 | GET `/runtime/status` | 显示 `last_apply`/降级原因 |
| F-06 | 在线用户 | 查看列表 | GET `/telemetry/online-users` | 行数与 API 一致 |
| F-07 | 在线用户 | 打开会话抽屉 | GET `/telemetry/sessions` | 抽屉内会话与 API 一致 |
| F-08 | 在线用户 | 筛选协议/关键字/分页 | 前端过滤（数据来自 F-07） | 结果可复现 |
| F-09 | Top连接 | 查看排行 | GET `/telemetry/top-sessions` | 行数与 API 一致 |
| F-10 | Top域名 | 查看排行 | GET `/telemetry/top-domains` | 行数与 API 一致 |
| F-11 | 网卡设置 | 修改接口角色 | PATCH `/interfaces/{id}` + POST `/runtime/apply` | 回读角色 + apply 成功 |
| F-12 | 网卡设置 | 创建聚合 | POST `/interface-bonds` | 回读聚合组/成员 |
| F-13 | LAN/WAN | 新增 LAN | POST `/interfaces` | 回读 LAN CIDR/角色 |
| F-14 | LAN/WAN | 新增 WAN（static/DHCP/PPPoE/代理） | POST `/gateway/wan-links` 或 `/proxy/egresses` | 回读线路 + PPPoE/代理状态 |
| F-15 | LAN/WAN | 删除线路 | DELETE + apply | 回读不存在 + 依赖清理 |
| F-16 | WAN群组 | 新建/编辑群组 | POST/PATCH `/gateway/wan-groups` | 回读成员/模式/权重 |
| F-17 | 策略路由 | 新建 Any→Any NAT | POST `/gateway/policies/routes` | 回读 priority/action/egress |
| F-18 | 策略路由 | 调整优先级/禁用 | PATCH | 回读 priority/enabled |
| F-19 | 端口映射 | 新建/编辑映射 | POST/PATCH `/gateway/nat/port-maps` | 回读外部/内部端口、协议 |
| F-20 | DNS策略 | 新建解析/拉黑 | POST `/dns/policies` + `/dns/upstreams` | 回读 policy/upstream |
| F-21 | DHCP | 新建服务 | POST `/dhcp/servers` | 回读 interface/subnet/pools |
| F-22 | DHCP | 新建静态分配 | POST `/dhcp/static-bindings` | 回读 MAC/IP |
| F-23 | 流量控制 | 新建限速/阻断 | POST `/gateway/traffic-control` | 回读 rules/action |
| F-24 | 对象管理 | 新建域名/IP 组 | POST `/objects/groups` | 回读 kind/members |
| F-25 | 对象管理 | 文件导入 | POST `/objects/groups/{id}/import` | `invalid_lines` 为空 + 回读成员 |
| F-26 | 用户管理 | 新增/改密/删除 | `/auth/users` | 新账号可登录/旧账号失效 |
| F-27 | 运行态 | 预览 | GET `/runtime/preview` | 计划计数/警告与 API 一致 |
| F-28 | 运行态 | 应用 | POST `/runtime/apply` | 200/202 + 状态/事务号 |
| F-29 | 配置 | 保存管理口 | POST `/management/network` | 回读 CIDR/网关；管理面仍可达 |
| F-30 | 配置 | 配置初始化 | POST `/config/factory-reset` | 需二次确认；回读默认态 |
| F-31 | 配置 | 同步系统时间 | 通过配置/管理接口实际支持 | 回读系统时间 |
| F-32 | 固件 | 上传并安装 | POST `/firmware/update/stage`, `/install` | staged + install 状态 |

> 表中“apply”均指真实 `POST /api/v1/runtime/apply`。所有写操作都先落 desired state，
> 再显式应用；UI 必须显示“已保存，待应用”，不得把保存成功当成已生效。

## 7. 后端已具备、当前 UI 未独立暴露（本轮默认不做）

| 能力 | API | 说明 | 处理 |
| --- | --- | --- | --- |
| 静态 NAT | `/gateway/nat/static` | 有 API，无独立页面 | 不新增页面；如产品批准，后续以“NAT”Tab 加入 |
| 安全 ACL / IP-MAC / 威胁情报 / 攻击规则 | `/security/*` | 有 API，无菜单 | 不新增页面；白皮书边界内但本轮不扩功能 |
| 代理群组 | `/proxy/groups` | 有 API | 不新增 |
| 配置快照/导入导出 | `/config/snapshots`, `/config/export|import` | 有 API，当前仅少量入口 | 不扩功能；可在系统页后续迭代 |
| PPPoE 连接/断开 | `/gateway/pppoe/connect|disconnect` | 有 API | 当前只读状态；是否加按钮需产品确认 |
| Xray 重启 | `/proxy/xray/restart` | 有 API | 当前只读状态/日志；不新增按钮 |
| 策略命中 | `/telemetry/policy-hits` | 有 API | 当前无页面；不在首页伪造命中 |
| DHCP 租约直读 | `/dhcp/leases` | 有 API | 当前经在线用户间接使用 |

## 8. 数据边界（严禁杜撰）

以下字段/指标**不在当前后端契约内**，不得出现在设计稿或实现中，除非后端先补 API：

| 不存在/不可得 | 说明 | 替代展示 |
| --- | --- | --- |
| WAN 延迟、抖动、丢包率 | 只有 bps/bytes、link state、health；无 RTT/loss | `--` 或“未采集”；不画延迟曲线 |
| DPI / 应用识别 / Top 应用 | 产品明确不含 DPI/OAF；无应用协议字段 | 只做 IP/域名/接口维度 |
| CPU 温度、磁盘、License、风扇 | 系统摘要只有 CPU/内存/负载/uptime/platform | 不显示 |
| 无线/AP/终端类型 | 不是本产品范围 | 不显示 |
| WAN 运营商、套餐、本月用量 | 无字段 | 不显示 |
| 每会话重传、最大包长 | Panabit 有，Ly Route 无 | 不显示 |
| 路由策略命中次数 | 路由 API 只返回 `runtime_state`，无 hit_count | 显示 `--`/“未采集”；端口映射可显示 `session_count/last_hit_at`（若有） |
| 会话命中的策略/出口 | 后端会话项没有策略/出口归属；当前 UI 为前端推断 | 默认不显示；如保留，必须标注“推断”并允许关闭 |
| Apply 百分比进度 | API 不返回阶段百分比 | 不确定进度条 + 已用时长 + 事务号 |
| 配置变更审计的完整历史 | 只有 `/telemetry/audit-events` 当前可读窗口 | 只显示 API 返回的事件 |

## 9. 现有后端约束（设计必须吸收）

| 约束 | 证据 | UI 设计应对 |
| --- | --- | --- |
| `runtime/status` 约 10.4–10.9s | 固件报告 P1-A | 首页/运行态用独立慢查询 + 骨架；不阻塞首屏；超时显示“状态读取较慢”，不误报故障 |
| 单次 apply 约 183s | 固件报告 P1-B | 不确定进度 + 事务号 + 轮询；>300s 显示“仍在执行” |
| 并发 apply 返回 202 `apply_in_progress` | 固件报告 §4 | UI 禁止重复提交；显示已有事务；提供“查看进度” |
| 保存只落 desired state | `handleDesiredMutation` 返回 `desired_not_applied` | 保存后统一提示 + 自动 apply + 全局待应用徽标 |
| `health.version` 可能是 `dev` | 固件报告 P2-1 | 显示 `--` 或原样 `dev`，不伪造版本号 |
| 接口地址可能含 `169.254.x/16` | 固件报告 P2-2 | 原样显示，标“链路本地”；不隐藏为正常地址 |
| 组件可能 degraded/timeout | 固件报告 §5 | 状态色 + reason + 采样时间；0 值与不可用严格区分 |
| 后端仍有 28 项测试失败 | P0-6 | UI 不启动实现；验收不以 mock 代替 |

## 10. 页面保留清单（实现时逐项对照）

实现前必须确认以下**现有功能**一个不少：

- [ ] 登录、登出、首次登录改密、改密、用户增删改
- [ ] 系统概况的资源/设备信息/WAN 趋势
- [ ] 流量概况的 5 个 KPI、上下行双图、实时/5分钟/1小时/24小时、出口图例开关
- [ ] 在线用户表、会话抽屉、会话筛选、分页
- [ ] Top 连接、Top 域名
- [ ] 网卡表、角色配置、创建聚合
- [ ] LAN/WAN 三 Tab、LAN 接口/桥、WAN 六种类型、线路日志
- [ ] WAN 群组五种模式、成员/权重/连接数/下行带宽/主备
- [ ] 策略路由优先级/匹配条件/动作/NAT线路/下一跳/全锥
- [ ] 端口映射协议/外部端口/内网主机/内网端口/备注
- [ ] DNS 策略优先级/源 IP/域名组/解析线路/上游/Bootstrap/拉黑
- [ ] DHCP 服务列表/静态分配
- [ ] 流量控制限速/阻断/方向/速率、Smart QoS 状态
- [ ] 域名组/IP 组的列表、成员展示、新增/编辑、文件导入、导出
- [ ] 配置页管理口、运行态预览/应用、Xray 状态/日志、PPPoE 状态、配置初始化、系统时间、固件升级

> 本清单是 UI 验收的“功能不回归”底线；任何一项缺失都判 UI 不通过。
