# 最终 UI 产物收口

> 产物源提交：Gateway UI `6fee555`；Orchestrator UI `84b1c2d`（当前工作树 HEAD）。
> 产物路径：`/root/work/ly-route-ui-final/`。
> 未改后端/安全/nginx/Pingora；未删除 VM。

## 1. 最终产物路径

```text
/root/work/ly-route-ui-final/
  gateway/dist/
  gateway/controller-shell/
  orchestrator/dist/
  orchestrator/controller-shell/
  SHA256SUMS
  MANIFEST.txt
  evidence/gateway/
  evidence/orchestrator/
  evidence/api/
```

`dist` 与 `controller-shell` 内容一致，均包含：

```text
app.js
styles.css
shell.js
commercial.css
index.html
capabilities.json
```

## 2. 核心 hash

| 产品 | 文件 | SHA-256 |
| --- | --- | --- |
| Gateway | `app.js` | `5f30ed4db99e061bb847c8a642f6da3d1464ca9a7f7fb3851aed9f2a5cb06227` |
| Gateway | `styles.css` | `6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe` |
| Gateway | `index.html` | `95bd19e5cd48b071e26a6b8606208e7659ec67ea09a48879924acfa2547b4b80` |
| Gateway | `capabilities.json` | `12e67b55278788e22472a2e54e23744abdd53664a319fc5d8a1ddeacb74fb75f` |
| Orchestrator | `app.js` | `dba17962b33a27dd5c7c617e3b7ff978daa93bb459423c75230763e931c3aad8` |
| Orchestrator | `styles.css` | `6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe` |
| Orchestrator | `index.html` | `b64fb9a1278409234494ba2ce5413c51ae5b000d720df2b989586f9792782130` |
| Orchestrator | `capabilities.json` | `7e0a88f454c2750b44d36a717d4a66bca6f7e63a27ba488f8d82ce06ffaba91d` |

完整文件清单见 `SHA256SUMS`。

## 3. capabilities / index / title 校验

```text
Gateway:
  capabilities.json .product = gateway
  <title>Ly Route 出口网关</title>

Orchestrator:
  capabilities.json .product = orchestrator
  <title>Ly Route 流量编排器</title>
```

`dist` 与 `controller-shell` 两套均一致。

## 4. Gateway 真实 API 证据

截图：`evidence/gateway/`

- WAN/PPPoE 1440/1920：`1/1 已连接`，`100.64.0.100`，`vpp.fib.route · ready`。
- 代理线路 1440/1920：`出口 1`，`Default proxy egress`，承载 `lab-pppoe`。
- 固件升级 1440/1920：`空闲`/`--`，无 `[object Object]`。
- DHCP 1440/1920：最终 5f30 采样仍显示“待应用”。
- 策略路由 1440/1920：最终采样在 API 超过 15s 时显示读取超时。

API 原始响应：`evidence/api/`

```text
gw-final-wan-links.json   pppoe_state=connected, route_ready=true
gw-final-pppoe.json       state=connected, assigned_ipv4=100.64.0.100
gw-final-proxy.json       proxy-egress-default
gw-final-firmware.json    {"status":{"staged":false,...}}
gw-final-runtime.json     last_apply.status=committed, kea=running/available
gw-final-dhcp.json        runtime_state=desired_not_applied
gw-final-routes.json      R3 desired_not_applied; default applied
```

说明：

- 当前 lab 的 DHCP 资源字段仍是 `desired_not_applied`，5f30 页面按运行时 readback 的显示逻辑未在该采样中显示“已应用”；已保留真实截图，不伪造成通过。
- 路由页在最终采样时 `GET /gateway/policies/routes` 耗时超过前端 15s 请求预算，页面显示读取超时；`PATCH` 保存请求已发出，但未在本次采样内进入 apply 完成态。
- 这两项已作为最终回归重点交给全栈测试；没有回改代码或伪造状态。

## 5. Orchestrator 组 CRUD 证据

真实 Orchestrator VM `orch-e2-81b974f`（`10451`，4 数据口）：

```text
PUT    /orchestration-groups/inline-a      200
POST   /orchestration-groups (inline-b)    201
DELETE /orchestration-groups/inline-b      204
POST   /orchestration-groups (inline-a)    201
PUT    /orchestrator/policy (restore)      200
```

截图：`evidence/orchestrator/`

- 组列表 1440/1920。
- 编辑弹窗预选 1440/1920：`eth3 · enp0s5`、`eth4 · enp0s6`。
- 创建 201、删除 204、恢复 201 后截图。
- 测试 VM 拓扑/策略已恢复到原 `inline-a` + via inline-a。

## 6. 重建命令（主开发）

```sh
cd /root/work/ly-route-ui-redesign
./scripts/build-product-ui.sh --product gateway --out /root/work/ly-route-ui-final/gateway/dist
./scripts/build-product-ui.sh --product orchestrator --out /root/work/ly-route-ui-final/orchestrator/dist
./scripts/build-controller-shell.sh --product gateway --out /root/work/ly-route-ui-final/gateway/controller-shell
./scripts/build-controller-shell.sh --product orchestrator --out /root/work/ly-route-ui-final/orchestrator/controller-shell
```

用 `/root/work/ly-route-ui-final/SHA256SUMS` 对 rootfs/upgrade/disk 做输入校验。
