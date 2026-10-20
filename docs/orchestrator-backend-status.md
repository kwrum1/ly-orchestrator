# Orchestrator 后端能力矩阵 / Backend Capability Matrix

本文只记录本仓库 `orchestrator` 产品实际设计并实现的后端能力。仓库中的
Orchestrator 是**单机 VPP 流量编排器**（本地拓扑、策略、服务链与遥测），不是
多站点集中控制器；文档没有设计站点注册、跨网关心跳或集中下发，因此本表不会把
这些能力标记为“待实现”。

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
| 升级/版本 | 打包脚本可生成 Orchestrator upgrade 载荷；产品 profile 不暴露 Gateway 固件 API | Orchestrator 没有独立升级任务 API/页面（产品设计未包含） | P3 | 产物结构检查 |
| 打包 | rootfs/upgrade/disk 支持 `--product orchestrator`；systemd unit 复用并注入产品 profile | ISO 只支持 Gateway（明确不支持 Orchestrator ISO） | P1 | `scripts/build-rootfs.sh`/`build-upgrade-package.sh`/`build-disk-image.sh` |
| 测试 | Go 单测、Shell 契约、容器 API/重启、VPP 数据面脚本 | 无统一 CI 入口 | P2 | 见下 |

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

## 验证命令

```sh
cd backend
go test ./internal/orchestrator/... ./internal/orchestratorapi/... \
  ./internal/product ./internal/persistence -count=1
go test ./internal/httpapi -run 'TestProductConfig|TestProductSnapshot|TestOrchestrator' -count=1
cd ..
sh scripts/test-orchestrator-negative-contract.sh
sh scripts/test-orchestrator-policy-contract.sh
sh scripts/test-orchestrator-container-api.sh
```

已知基线问题（与本次改动无关）：`test-orchestrator-product-contract.sh` 会执行
完整 `internal/httpapi` 与 `internal/runtime/vpp`，当前基线在这两个包存在既有失败
（SmartDNS 测试夹具、VPP 快照/ABF 解析等）。本次修改涉及的 Orchestrator 包和
HTTP 子集均通过。
