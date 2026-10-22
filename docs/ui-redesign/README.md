# Ly Route UI 生产化设计与工程准备

> 状态：**实现完成，待测试专家全栈验收。**
>
> 分支：`feat/ui-redesign`；基线：Gateway `fa3abd68`（API 契约冻结）。
> 未合并 Pingora，nginx 仍为默认；未修改后端 API/数据。

## 0. 一句话结论

在不新增后端功能、不虚构数据的前提下，把现有 Ly Route 网关 UI 重构为
“iKuai 4.0 的清晰信息层级 + UniFi 的克制数据密度 + Panabit 的可运维会话视图”
的浅色专业控制台。首页重新设计；其余页面保留现有功能与真实 API，只重做显示与交互。

## 1. 文档索引

| 文档 | 内容 | 读者 |
| --- | --- | --- |
| [01-functional-inventory.md](./01-functional-inventory.md) | 真实功能清单、API 清单、页面地图、数据边界、已知限制 | 产品 / 前端 / 后端 |
| [02-architecture.md](./02-architecture.md) | 技术选型、目录结构、API client、状态管理、表格/图表、构建与打包集成 | 前端 / 构建 |
| [03-design-system.md](./03-design-system.md) | 设计 tokens、字体、颜色、间距、组件、表格密度、响应式规则 | 设计 / 前端 |
| [04-home-and-wireframes.md](./04-home-and-wireframes.md) | 首页布局、关键页面线框、1440/1920 尺寸、弹窗/抽屉规格 | 设计 / 产品 |
| [05-interaction-spec.md](./05-interaction-spec.md) | 操作流、Apply 事务、会话筛选/详情、错误/空态、交互差异表 | 产品 / 前端 |
| [06-panabit-lab-plan.md](./06-panabit-lab-plan.md) | Panabit 安装、学习场景、证据模板、清理与风险 | 测试 / 设计 |
| [07-acceptance-and-gate.md](./07-acceptance-and-gate.md) | UI 验收标准、证据包、门禁条件、上线与回退 | 产品 / 测试 / 发布 |
| [08-implementation-report.md](./08-implementation-report.md) | 实际实现、构建命令、页面映射、未实现/后端缺口、证据 | 产品 / 测试 / 前端 |
| [09-orchestrator-product.md](./09-orchestrator-product.md) | Orchestrator 单机产品边界、11 页路由、API 映射、双产品构建、验收证据 | 产品 / 测试 / 前端 |

## 2. 基线（本次阅读与核对）

| 项目 | 基线 |
| --- | --- |
| 产品源码 | `/root/work/ly-route-ui-redesign`，分支 `feat/ui-redesign`，基线 `fa3abd68` |
| 当前 UI | `frontend/gateway/`：React 19 + TypeScript + Vite；设计文档与本报告同目录 |
| 后端 API | `backend/internal/httpapi/product_routes.go`，约 95 条 `/api/v1/*` 路由 |
| 产品能力 | `product.Profile` 22 项 capability；Gateway 全开 |
| 验收矩阵 | `docs/acceptance/functional-vm-lab.md` F01–F15；UI 阶段在第 5 节 |
| 当前 UI 证据 | 本仓库 `evidence/*.png`、`evidence/ui-pages.json`、`evidence/ui-diag.json` |
| 现有测试报告 | `ly-route-firmware-functional-test-report.md`、`orchestrator-functional-test-report.md` |
| Pingora | `docs/pingora-migration.md`；前端完成后才切换，nginx 保留回退 |

## 3. 硬边界

1. **不新增后端能力**：页面只使用已冻结 API；安全/代理/静态 NAT 等是后端已有能力，本轮只补 UI 暴露。
2. **不虚构数据**：页面只显示 API 返回的字段；未知显示 `--`；不估算延迟、丢包、DPI、温度等不存在的数据。
3. **不伪造状态**：`desired_not_applied`、`degraded`、`apply_in_progress`、`unavailable` 必须原样可见。
4. **不绕过 UI 写配置**：验收阶段所有配置写入必须通过真实 UI；API 只用于登录后只读回读与证据。
5. **不替换 nginx 默认入口**：Pingora 在前端验收完成后再合并；nginx 保留回退。
6. **不复制商标/素材**：只学习 iKuai、UniFi、Panabit 的设计语言与交互模式，不使用其 logo、图标、截图或专有 CSS。

## 4. 当前门禁

| 门禁 | 状态 | 说明 |
| --- | --- | --- |
| 后端功能冻结 | **已满足（用户确认）** | Gateway P0 清零，HEAD 约 `fa3abd68`，API 契约冻结 |
| UI 实现 | **已完成，待验收** | `feat/ui-redesign` 已实现 Gateway 21 页 + Orchestrator 11 页；见 08/09 报告 |
| 测试专家全栈验收 | **待启动** | 真实 router + 真实 VPP 遥测/会话回归 |
| Pingora 合并 | **暂缓** | 前端验收通过后再合并；nginx 保持默认 |
| 发布门禁 | 独立管理 | 仍以测试/发布团队结论为准 |

## 5. 下一步

1. 测试专家按 `07-acceptance-and-gate.md` 与 `08-implementation-report.md` 全栈验收。
2. 在真实 router 上验证 VPP 遥测、会话抽屉、PPPoE、策略/NAT 的 apply 与回读。
3. 确认 rootfs/升级包可消费新构建产物（`app.js/styles.css/capabilities.json` 同名 + checksum）。
4. 验收通过后再合并 Pingora；nginx 保持默认，按 runbook 灰度与回退。
