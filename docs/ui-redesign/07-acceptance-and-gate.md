# UI 全栈验收与合并闸门

> 适用于 Gateway 与 Orchestrator 两个产品。
> 当前状态：代码与构建完成；真实 router/VPP 验收未完成；Pingora 未合并，nginx 保持默认。

## 1. 闸门

必须同时满足以下条件，才允许合并 Pingora 或切换默认 nginx：

1. Gateway **21 页**全部可访问，旧 hash URL 可重定向；核心写操作保存 → apply → 回读一致。
2. Orchestrator **11 页**全部可访问，拓扑/策略/编排组/透明编排/运行态/配置/升级/转换使用真实 API。
3. `202 apply_in_progress` 不重复提交；`422` 显示后端原因；转换 `apply` 501 明确显示，不伪造成功。
4. 只读账号写按钮禁用，后端权限仍校验。
5. `scripts/build-controller-shell.sh --product gateway|orchestrator` 产物可被现有 rootfs/升级包 checksum 消费。
6. 真实 router 上验证 VPP 遥测、会话、PPPoE、拓扑/策略保存、透明编排、升级任务。
7. nginx 默认入口与回退可用；Pingora 仅在独立开关/回退验证后合并。

## 2. Gateway 验收

- 页面集合：侧栏 21 项，路由见 `docs/ui-redesign/08-implementation-report.md`。
- 关键路径：
  - 登录/首次改密/退出/只读。
  - 网卡角色、LAN/WAN、默认 Any→Any、端口映射、DNS、DHCP、对象组、用户。
  - 会话筛选/分页/抽屉；策略弹窗；配置导出/快照。
- 证据：`docs/ui-redesign/evidence/`（1440/1920 关键页）。
- 本轮无 Gateway 页面删除；Gateway bundle 因双产品共享层版本更新，hash 记录见 `09-orchestrator-product.md`。

## 3. Orchestrator 验收

- 页面集合：侧栏 11 项：
  `/`、`/topology`、`/groups`、`/policy`、`/transparent`、`/telemetry`、`/runtime`、`/config`、`/upgrades`、`/conversions`、`/system/users`。
- 关键路径：
  - 首次无拓扑：总览/拓扑显示“未配置”，不伪造端口。
  - 保存管理口 + LAN/WAN + 编排组；422 显示后端原因。
  - 策略组/规则/IP 对象保存；策略编译返回真实 path。
  - 透明编排 operation 状态；独立端点 404 有明确说明。
  - 遥测降级原因；不显示不存在的延迟/丢包/DPI/温度。
  - 升级包上传/暂存/安装/回滚 202 轮询。
  - 转换 manifest 校验 → 创建任务 → Apply 501 → 回滚。
  - 配置导出 → 导入预检 → 确认替换。
- 证据：`docs/ui-redesign/evidence/orchestrator/`。

## 4. 构建验收命令

```sh
cd /root/work/ly-route-ui-redesign
./scripts/build-product-ui.sh --product gateway --out /tmp/gw-ui-final
./scripts/build-product-ui.sh --product orchestrator --out /tmp/orch-ui-final
./scripts/build-controller-shell.sh --product gateway --out /tmp/gw-ui-controller
./scripts/build-controller-shell.sh --product orchestrator --out /tmp/orch-ui-controller
```

预期固定文件：

```text
app.js
styles.css
shell.js
commercial.css
capabilities.json
index.html
```

`capabilities.json` 的 `product` 必须分别为 `gateway` / `orchestrator`；`index.html` 标题分别为 `Ly Route 出口网关` / `Ly Route 流量编排器`。

## 5. 当前证据与限制

- 沙箱：Gateway `127.0.0.1:18090`，Orchestrator `127.0.0.1:18095`；均未接真实 VPP。
- 浏览器：1440×900 全页面；1920×1080 关键页；控制台无未处理错误/404。
- 已有真实 API 验证：策略编译、转换 validate/create/apply 501、配置导入预检、Gateway 核心写操作（前次验收）。
- 待补：真实 router 上 VPP 遥测/会话、拓扑/策略保存与透明编排、升级/回滚、rootfs/升级包 checksum 全链路。
- 阻塞：`scripts/validate-rootfs-scaffold.sh` 全量在 `go test` 阶段可能被模块下载阻塞；后端未改。
- 未 push：构建机无 GitHub 凭据。

## 6. 回退

- 前端：保留 Gateway `dist/` 与 Orchestrator `dist-orchestrator/` 两套静态产物；nginx 默认继续指向 Gateway 产物。
- Pingora：不合并；现有 nginx 配置与回退路径保持。
- 转换：不执行真实镜像转换；只保留 manifest/task/rollback 骨架。
