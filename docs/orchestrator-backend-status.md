# Orchestrator 后端能力矩阵 / Backend Capability Matrix

本文只记录本仓库 `orchestrator` 产品实际设计并实现的后端能力。仓库中的
Orchestrator 是**单机 VPP 流量编排器**（本地拓扑、策略、服务链与遥测），不是
多站点集中控制器；文档没有设计站点注册、跨网关心跳或集中下发，因此本表不会把
这些能力标记为“待实现”。

升级与转换接口见 [orchestrator-upgrade-conversion-api.md](orchestrator-upgrade-conversion-api.md)。

## 能力盘点

| 模块 | 已有 | 缺口 | 优先级 | 验证 |
|---|---|---|---|---|
| 进程启动/配置 | `cmd/orchestrator-control`；product profile、DB/config 路径、环境变量 | 无（ISO 不属于本仓库 Orchestrator 范围） | P1 | `go build ./cmd/orchestrator-control`；容器 API 测试 |
| DB/迁移 | SQLite schema v1；产品元数据不可变；Orchestrator 拒绝接管旧库 | 无版本化增量迁移（当前 schema 未变） | P2 | `internal/persistence` 单测 |
| 认证/会话 | 登录、登出、会话、改密、只读用户、Cookie/Bearer | 无 | P1 | `internal/httpapi` 认证测试 |
| 审计 | 成功/失败/拒绝事件；SQLite 持久化；敏感信息脱敏 | 服务链 intent 不进入配置包（运行态协调数据） | P1 | 审计单测；容器 API 测试 |
| 健康检查 | `/api/v1/health`、能力列表、服务状态 | 插件就绪目前由 `runtime-check.sh` 的启动门禁保证，HTTP 健康不单独探测 `.so` | P2 | 容器 API 测试；`runtime-check.sh` |
| 本地拓扑/分组 | topology、端口/Bond、编排组、管理口共享校验 | 无（站点/节点纳管不在产品设计内） | P1 | `internal/orchestrator` + API 测试 |
| 策略编译 | 类型化 IP 策略、路径编译、防回环、只读编译接口 | 无 | P1 | `test-orchestrator-policy-contract.sh` |
| 策略下发/回滚 | PUT 策略/拓扑前先下发 VPP，失败回滚；事务 journal；代际回读 | 无 | P1 | 容器 API + VPP 透明编排测试 |
| 配置包/备份 | **本次新增**：配置导出、快照、导入包含 `orchestrator_topology` 与 `orchestrator_policy`；导入前校验拓扑/策略一致性；旧快照恢复时保留现有拓扑/策略 | 服务链 intent 作为运行态协调数据排除 | P1 | 新增 `orchestrator_config_package_test.go`；容器 API 测试 |
| 运行时应用 | **本次新增**：`/api/v1/runtime/apply` 会先重放已持久化的拓扑/策略代际，再执行运行态计划 | 无 | P1 | 新增运行时应用单测 |
| 遥测 | 接口/策略命中/连接/邻居/流量趋势/dashboard | 依赖 VPP CLI 输出；不可用时显式 degraded | P1 | `internal/orchestrator/telemetry` + HTTP 测试 |
| VPP 插件 | `runtime/vpp-orchestrator` 真实 C 插件、CLI、生成器、服务链/透明模式 | 无（需在目标 VPP 版本上构建） | P1 | `scripts/build-vpp-orchestrator-plugin.sh`；`test-vpp-orchestrator-transparent.sh` |
| 升级/版本 | **本次新增**：Orchestrator 独立版本清单、升级任务、安装/回滚、失败态与审计；复用 `ly-route-upgrade` 载荷 | 真实镜像转换未启用（conversion `apply` 返回 501） | P1 | `test-orchestrator-upgrade-conversion-contract.sh` |
| 产品转换 | **本次新增**：conversion manifest 校验、任务持久化、预转换快照、服务 enable 状态捕获/恢复、回滚骨架；Gateway installer 显式识别 conversion 类型 | 不做真实镜像迁移/上传 | P1 | `internal/productupdate` + httpapi 测试 |
| 打包 | rootfs/upgrade/disk 支持 `--product orchestrator`；systemd unit 复用并注入产品 profile | ISO 只支持 Gateway（明确不支持 Orchestrator ISO） | P1 | `scripts/build-rootfs.sh`/`build-upgrade-package.sh`/`build-disk-image.sh` |
| 测试 | Go 单测、Shell 契约、容器 API/重启、VPP 数据面脚本 | 无统一 CI 入口 | P2 | 见下 |

> 依赖说明：本分支 cherry-pick 了 `9867b65`（`fix: make firmware upgrade
> verifiable and reversible`，来自 `fix/p0-ui-af-xdp`），提供 Gateway
> 升级的备份/回滚/健康检查与 `includes_dataplane` 校验；本分支在其之上增加了
> conversion 类型识别。后续合并主分支时不要重复应用该提交。

## 本次修改的根因

Orchestrator 的配置导出/快照只遍历 `desiredResourceDefs`，而拓扑和策略存放在
`orchestrator_topology` / `orchestrator_policy` 配置文档中，不在该集合内。结果是
用户“导出/创建快照”得到的包不包含编排器最核心的状态；恢复也只能恢复对象组、
ACL 等外围配置。配置导入完成后 UI 又提示“需执行运行时应用”，但原
`/api/v1/runtime/apply` 对 Orchestrator 只编译安全/流意图，不会重放拓扑和策略。

修复后：

- 导出和快照始终携带拓扑/策略（无状态时为空数组，便于恢复时真正删除）；
- 导入/恢复前校验拓扑可解析、策略可引用新拓扑，并且只改拓扑时不会让现有策略失效；
- 旧版本快照（没有两个新键）恢复时保留当前编排状态，避免升级后误清空；
- `runtime/apply` 在 Orchestrator 上先按持久化拓扑/策略执行透明编排事务，再执行
  原有运行态计划；失败返回 `apply_failed` 并写审计；
- 服务链 intent 明确列入 `excluded_domains`，因为它是运行态协调缓存，不是可移植
  的期望配置。

## 自研/平替原则（长期）

自研组件若存在难以稳定修复的缺陷，不允许无限期硬修。处理顺序：

1. 先确认根因、影响面、修复成本和回归历史；风险高、周期长或反复回归时进入平替评估。
2. 候选优先级：**VPP 原生能力/API → 官方上游实现 → 成熟第三方实现 → 继续自研**。
3. 平替必须保留现有产品 API、配置和数据契约；记录版本 pin、许可证、来源和构建方式；
   提供适配层与回退开关；补充单元/集成测试；旧实现保留到新路径验收通过。
4. 不得以“自研可控”为由拒绝成熟实现，也不得为省事直接替换而破坏契约。
5. 每次评估记录候选、对比、选择理由、风险和回退方式。
6. 节点纳管、策略下发、遥测、升级等模块若已有成熟上游组件，先评估集成。
7. **外部软件导致修复困难时允许平替**：SmartDNS、Kea、Xray 或其它外部组件在
   Orchestrator 集成中若难以稳定修复，允许使用 VPP 原生能力或成熟第三方实现；
   前提是功能等价，不能降低策略、遥测、配置恢复、升级回滚等产品行为。
8. **选择最快、最容易修复、最成熟且功能等价的实现**：顺序为 VPP 原生 →
   成熟第三方 → 继续自研/原组件；保留 API/配置/数据契约，提供 adapter/feature
   flag/回退，旧路径至少保留一个发布周期；记录版本 pin、许可证、provenance、
   迁移步骤；必须有功能等价和故障测试；不得静默删功能或降低可观测性。
9. **反复修复仍不稳定时停止打补丁，允许换一种方式重新实现**。触发条件：同一问题
   多次回归、补丁互相影响、根因跨模块、修复成本高于重写、依赖脆弱外部组件。要求：
   - 记录已尝试补丁、失败原因和可观察契约；
   - 明确功能等价定义；
   - 选择更简单/成熟的替代实现（VPP 原生/第三方/重新设计）；
   - 保留 API/配置/数据契约，提供 adapter/feature flag/回退；
   - 旧实现至少保留一个发布周期；
   - 记录版本 pin、许可证、provenance 和迁移步骤；
   - 补功能等价、故障、重启/升级测试；
   - 不得借重写删除功能或降低可观测性。

   **本轮案例（P1-O1）**：升级安装脚本在 control-api 自身 cgroup 内重启服务，
   多次导致 HTTP 连接被切断。修复不是继续调整等待时间，而是改为“先持久化并返回
   202 + task_id，再用 systemd-run 独立 unit 异步执行，结果文件由 GET 任务轮询
   reconcile”，保留原 API/状态机并新增 `operations/current` 可观测性。

当前 P1/P2 修复属于升级/错误/可观测契约，不涉及外部组件替换。若后续
SmartDNS/Kea/Xray 进入 Orchestrator 运行时，先按上述原则评估 VPP 原生或上游
实现，并保留现有 HTTP/配置/遥测契约。

### 当前平替评估

| 模块 | 现状/触发条件 | 优先候选 | 结论与回退 |
|---|---|---|---|
| VPP 转发/策略/服务链 | 自研 `ly_route_orchestrator_plugin`，已基于 VPP 原生接口 | VPP 原生 ACL/ABF/接口与 stats API | 继续使用插件；新增能力前先确认 VPP 原生是否已覆盖。插件 CLI/代际不变，可回退到 direct/disable |
| 遥测采集 | Go 解析 `vppctl` 文本（`show interface`、`show ly-route orchestrator`） | VPP stats segment / 二进制 API / 上游 JSON 输出 | `show ly-route orchestrator` 由自有插件定义，保持；若通用 VPP 文本解析继续反复失败，评估 stats segment，HTTP 契约不变 |
| 节点/网关纳管 | 当前产品未设计多站点纳管 | NetBox/Nautobot 等成熟 inventory（需核实许可证与版本） | 需求出现时先做只读 inventory 适配层，不直接替换本地拓扑模型 |
| SmartDNS | Orchestrator profile 不包含该服务 | VPP 原生 DNS/上游 resolver；成熟 DNS 组件 | 若未来集成且难以稳定修复，保留 API/配置契约并加 adapter/flag |
| Kea DHCP | Orchestrator profile 不包含该服务 | VPP 原生/上游 DHCP；成熟 DHCP 组件 | 同上；不得降低配置恢复和遥测 |
| Xray 代理 | Orchestrator profile 不包含该服务 | VPP service-chain/上游代理 | 同上；保留策略/遥测/升级回滚契约 |
| 升级/回滚 | 现有 upgrade 包 + 自定义安装脚本 | RAUC / SWUpdate / Mender（需核实许可证、版本与 A/B 分区支持） | 仅在现有脚本无法满足回滚/原子性时评估；保留现有包格式与 manifest |
| 认证/会话/审计 | 自研 Go session + SQLite 审计 | 上游 OIDC/Keycloak（如未来需要集中身份） | 当前内部离线场景保持自研；适配层需保留现有 Cookie/Bearer 与审计字段 |
| 数据库/HTTP | `modernc.org/sqlite` + `net/http` | 已是成熟上游 | 不替换 |

评估记录模板：`模块 / 失败现象 / 根因 / 自研成本 / 候选与版本 / 许可证 / 适配层 / 回退开关 / 测试证据 / 决策`。

## 透明策略方向与 VPP 重启恢复（P0/P1）

- 策略匹配统一使用 **client → server** 方向。LAN 入向保持原 tuple；WAN 入向在匹配前交换源/目的地址和端口，因此 `destination_ports` 规则会同时命中请求与返回流。`via`、`drop`、端口范围、`source_ports` 和非端口规则共用同一归一化路径。
- 回归必须同时检查 VPP `policy ... packets > 0`、flow `groups inline-a`、API `policy_hits > 0` 和真实 TCP 200/超时，不能只检查 apply committed。
- VPP 重启后插件处于 `state locked` 时，control 的透明恢复循环按 `LY_ROUTE_ORCHESTRATOR_RECONCILE_INTERVAL`（默认 2s）读取持久化 topology/policy 并重放；恢复失败会保留错误并继续重试。
- `/api/v1/health` 在存在 topology 时必须包含 `orchestrator_runtime` 依赖；插件 locked、generation 漂移或回读失败时返回 `degraded=true`，不再只依据 `vpp.service` 报假绿。无 topology 时不要求该依赖。
- 相关证据：`.sisyphus/full-acceptance/evidence/orchestrator-p0p1/`。

## 验证命令

```sh
cd backend
go test ./internal/productupdate ./internal/persistence ./internal/orchestrator/... \
  ./internal/orchestratorapi -count=1
go test ./internal/httpapi -run 'TestOrchestratorUpgrade|TestGatewayConversion|TestFirmwareUpgradeInstaller' -count=1
cd ..
sh scripts/test-orchestrator-upgrade-conversion-contract.sh
sh scripts/test-orchestrator-negative-contract.sh
sh scripts/test-orchestrator-policy-contract.sh
sh scripts/test-orchestrator-container-api.sh
```

已知基线问题（与本次改动无关）：`test-orchestrator-product-contract.sh` 会执行
完整 `internal/httpapi` 与 `internal/runtime/vpp`，当前基线在这两个包存在既有失败
（SmartDNS 测试夹具、VPP 快照/ABF 解析等）。本次修改涉及的 Orchestrator 包和
HTTP 子集均通过。
