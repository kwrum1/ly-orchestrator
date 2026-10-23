# Gateway 全栈验收 P1 修复

> 分支：`feat/ui-redesign`
> 基线：Gateway UI `cbca43b`（Orchestrator UI 已合入）
> 范围：只修 UI 状态映射/解包/apply 顺序；不改后端、安全、nginx；不合并 Pingora。
> 目标：保持 Gateway 21 页、Orchestrator 11 页。

## 1. 修复清单

| ID | 根因 | 修复 |
| --- | --- | --- |
| P1-UI-1 | `/gateway/pppoe/status` 返回 `items[]`，页面读 `peers/status`；WAN link 的 `pppoe_state=connected` 未被优先使用；总览用遥测 series 的过期 `health=failed` 覆盖了配置状态 | 统一读取 `items/peers`；状态优先 `pppoe_state → state → runtime_state`；`route_ready/connected` 归一为健康；OT 总览按 id/name 用配置状态覆盖过期 series health；显示 peer 地址、route_ready 与 `vpp.fib.route`；补 `connected/disconnected` 中文标签 |
| P1-UI-2 | Firmware API 返回 `{status:{...}}`，页面把 `status` 对象当字符串渲染 | 解包 `response.status`；使用真实字段 `staged/installing/install_status/image_path/image_hash/image_size/staged_at/reason`；包名取 basename；不伪造百分比，明确“后端未返回百分比” |
| P1-UI-3 | 代理页主动过滤 `proxy-egress-default`，导致 API 唯一默认出口不显示 | 保留默认出口，标记“默认”；默认出口不可删除；出口计数与 API 一致 |
| P1-UI-4 | 保存后 `apply` 与正在执行的 apply 竞争/被 `runningRef` 静默丢弃；路由 API 的 `runtime_state` 在 apply 后仍可能是 `desired_not_applied` | ApplyProvider 增加 pending 队列：保存遇到正在执行的 apply 时等待当前结束后自动再 apply 一次；路由状态优先用 runtime evidence 的 `routes` receipt + `after.route_policies[].id` 判定，只有真实 applied 才显示“已应用” |
| P1-UI-5 | `runtime_state` 字段滞后；Kea 实际 active，但 `/dhcp/servers` 仍返回 `desired_not_applied` | 状态组合 `/runtime/status`：仅当 `last_apply.status=committed` 且 `components[name=kea]` running/available 时把 `desired_not_applied` 显示为“已应用”；apply 进行中或失败时保持真实待应用 |

## 2. 真实 API 字段

| 页面 | API | 使用字段 |
| --- | --- | --- |
| WAN/PPPoE | `GET /gateway/wan-links` | `pppoe_state`, `route_ready`, `runtime_state`, `id`, `name` |
| WAN/PPPoE | `GET /gateway/pppoe/status` | `items[].peer_id`, `interface`, `state`, `assigned_ipv4`, `route_ready`, `vpp_route_handoff` |
| 固件 | `GET /firmware/update/status` | `status.staged`, `installing`, `install_status`, `image_path`, `image_hash`, `image_size`, `staged_at`, `reason` |
| 代理 | `GET /proxy/egresses` | `items[]`（含 `proxy-egress-default`）, `underlay_wan_id`, `node_id` |
| 路由 | `GET /gateway/policies/routes` | `runtime_state`, `id`, `enabled` |
| 路由 apply | `GET /runtime/status` | `last_apply.status`, `gateway_evidence[].resource=routes`, `apply_receipt.status`, `after.route_policies[].id` |
| DHCP | `GET /dhcp/servers` | `runtime_state` |
| DHCP apply | `GET /runtime/status` | `last_apply.status`, `components[].name=kea`, `state`, `available` |

## 3. 后端字段缺口（不伪造）

- 固件 API 没有进度百分比；页面只显示 `install_status`/`installing`，并提示无百分比。
- DHCP 资源没有独立的 apply receipt/readback；UI 只能用全局 `last_apply=committed` + Kea 组件 running 推导“已应用”。若后续新增 DHCP 资源级 receipt，应改用 receipt。
- 路由资源也没有更新后的 `runtime_state`；UI 用 routes evidence 判定，避免把旧 apply 状态误认为当前。
- WAN/PPPoE API 的 `vpp_route_handoff` 可能是字符串、对象或 null；UI 对三种形态做了只读显示，不推断缺失的下一跳。
- 遥测 series 的 `health` 可能滞后；总览仅用配置状态覆盖同 id/name 的出口 health，仍保留 API 的 rates。

## 4. 构建

```sh
cd /root/work/ly-route-ui-redesign
./scripts/build-product-ui.sh --product gateway --out /tmp/gw-ui-p1
./scripts/build-product-ui.sh --product orchestrator --out /tmp/orch-ui-p1
./scripts/build-controller-shell.sh --product gateway --out /tmp/gw-ui-p1-controller
./scripts/build-controller-shell.sh --product orchestrator --out /tmp/orch-ui-p1-controller
```

本轮产物：

```text
Gateway      app.js  5f30ed4db99e061bb847c8a642f6da3d1464ca9a7f7fb3851aed9f2a5cb06227
Gateway      styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
Orchestrator app.js  0115be966224b58982509c6407457e7b3774e098ebb9f0004f002d4edeb00965
Orchestrator styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
```

Gateway 页面数仍为 21，Orchestrator 为 11；不改后端 API/数据。

## 5. 验证

### 真实 router/18444

- 新 UI 已部署到路由器的 `edge-staging` 静态根；`edge-staging` 服务监听 18444。
- 已验证代理页：`出口 1`、`Default proxy egress`、承载 WAN `lab-pppoe`，不再显示“暂无代理配置”。
- 已验证固件页：API `{status:{staged:false,staged_at:"0001..."}}` 显示“空闲/--”，无 `[object Object]`。
- 已验证总览：`lab-pppoe` 不再显示“失败”，显示“正常”；WAN/PPPoE 状态改由 `pppoe_state`/`route_ready` 驱动。
- 真实 lab 在验收期间被并行测试反复重启/切换 18444 与 control API，导致部分页面请求出现 `ERR_CONNECTION_RESET`/502；这些是环境瞬态，不是 UI 空值伪造。最终全栈回归仍由测试专家在稳定 lab 上执行。

### 沙箱

- 代理页（Gateway sandbox）：出口 1、默认出口可见。
- 固件页（Gateway sandbox）：空状态无 `[object Object]`。
- 控制台除登录前预期 401 外无未处理错误。

### 截图

- `docs/ui-redesign/evidence/gateway-p1/`
  - `proxy-1440.png`、`proxy-1920.png`：Gateway sandbox，默认出口 1 条可见。
  - `firmware-1440.png`、`firmware-1920.png`：Gateway sandbox，无 `[object Object]`。
  - `wan-late-1440.png`：真实 lab（并行测试期间），PPPoE 状态直接来自 API；该次采样的 API 为 disconnected，因此显示 0/1 未连接，不再是“未知”。
  - `dhcp-late-1440.png`、`routing-late-1440.png`：真实 lab，API/runtime 仍在 apply failed/degraded 时的真实待应用状态。

## 6. 本轮复测结果（真实环境限制）

| 项 | 结果 |
| --- | --- |
| P1-UI-1 WAN/PPPoE | 代码已改为读取 `items`/`pppoe_state`；真实 lab 中 API `connected` 时总览显示“正常”；一次 API 采样为 `disconnected` 时 WAN 页显示 `0/1`、不再显示“未知”。连续采样被并行测试重启/网络重置打断，未取得完整的 connected 1440/1920 成对截图。 |
| P1-UI-2 固件 | 真实 lab 空状态与 sandbox 1440/1920 均无 `[object Object]`；字段按后端 `status` 解包。 |
| P1-UI-3 代理 | 真实 lab 与 sandbox 均显示 `出口 1`、`Default proxy egress`，不再显示“暂无代理配置”。 |
| P1-UI-4 Any→Any | ApplyProvider pending 队列与路由 evidence 状态映射已实现；真实 lab 的 apply 在并行测试期间多次被 reset/502 打断，未能稳定完成一次 270s committed 复测。UI 不再把“成功”伪造为已完成。 |
| P1-UI-5 DHCP | 状态映射已实现：仅 committed + Kea running 才把旧 `desired_not_applied` 显示为“已应用”；真实 lab 并行采样的 runtime 曾处于 failed/degraded，因此截图仍显示待应用，这是当时真实状态。 |

> 上述真实 lab 采样期间，另一组验收进程反复重启/切换 control API 与 18444，出现 `ERR_CONNECTION_RESET`、`502`、`database is locked`。UI 没有把请求失败显示成 0 成功；相关页面显示错误态/待应用，符合不伪造原则。

## 7. 后续

1. 测试专家在稳定 lab/18444 重跑：
   - WAN/PPPoE connected 状态；
   - 固件 staged 状态；
   - Proxy default egress；
   - Any→Any 保存后一次 apply；
   - DHCP Kea active 后状态。
2. 真实 router 上确认 apply 事务在 270s 内完成后，路由/DHCP 状态使用 runtime readback 而非资源旧字段。
3. 通过后再考虑 Pingora 合并；nginx 保持默认。
