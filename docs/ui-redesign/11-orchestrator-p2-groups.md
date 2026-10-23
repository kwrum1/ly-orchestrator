# Orchestrator 编排组 P2 修复

> 分支：`feat/ui-redesign`
> 提交：以最终 `git log -1` 为准（本 P2 修复）
> 测试实例：真实 Orchestrator QEMU `orch-e2-81b974f`，mgmt `10032/10451`，4 个数据口
> 约束：只改前端；不改后端、安全、nginx、Pingora。测试期间的临时策略/编排组变更已恢复。

## 1. 根因

真实 Orchestrator 拓扑使用系统接口名：

```text
eth1 = enp0s3
eth2 = enp0s4
eth3 = enp0s5
eth4 = enp0s6
```

旧编排组页面把 `/interfaces` 的 `name`（`eth3`/`eth4`）作为 Select 的 value，但拓扑/编排组 API 使用 `system_name`（`enp0s5`/`enp0s6`）。因此：

- 编辑现有组时，表单值 `enp0s5/enp0s6` 不在选项 value 中，下拉显示为空。
- 新增组时，即使 POST 发出，提交的也是 `eth3/eth4`，与真实数据面接口不匹配；失败信息没有稳定留在弹窗内。
- 编辑/新增共用 `null | undefined` 编辑器状态，保存分支与关闭行为容易混乱。
- 后端 `PUT /orchestration-groups/{name}` 明确要求 body `name` 与路径一致，重命名无法复用该接口。

## 2. 修复

- 端口选项 value 改为 `system_name || name || id`；显示标签为 `eth3 · enp0s5`。
- 编辑时保留原组端口，并把现有组端口加入候选，确保预选。
- 新增/编辑状态拆分为 `open` + `editing`，提交/关闭/错误处理明确。
- 保存前做本地校验；API 错误保持在弹窗内显示（`role=alert`），不再静默保持或假成功。
- 编辑时组名称只读，提示“名称是 API 主键，编辑时不可修改；端口会保留”。保存时发送原名称和预选端口，`PUT` 可成功。
- 新增使用 `POST /orchestration-groups`；编辑使用 `PUT /orchestration-groups/{name}`；删除使用 `DELETE /orchestration-groups/{name}`。
- 说明后端缺口：当前没有原子“重命名编排组 + 更新策略引用”接口，因此 UI 不提供改名。

## 3. 真实 API CRUD 证据

测试环境：真实 Orchestrator VM `10451`；临时将策略中的 via 组改为 direct 以释放 `inline-a` 端口，测试后已恢复原策略。

```text
POST   /api/v1/orchestrator/orchestration-groups
       {"name":"inline-b","ports":[{"interface":"enp0s5","direction":"lan_facing"},{"interface":"enp0s6","direction":"wan_facing"}]}
       -> 201 Created

PUT    /api/v1/orchestrator/orchestration-groups/inline-a
       {"name":"inline-a","ports":[{"interface":"enp0s5","direction":"lan_facing"},{"interface":"enp0s6","direction":"wan_facing"}]}
       -> 200 OK

DELETE /api/v1/orchestrator/orchestration-groups/inline-b
       -> 204 No Content

POST   /api/v1/orchestrator/orchestration-groups   (restore)
       {"name":"inline-a","ports":[{"interface":"enp0s5","direction":"lan_facing"},{"interface":"enp0s6","direction":"wan_facing"}]}
       -> 201 Created

PUT    /api/v1/orchestrator/policy (restore original via-inline-a policy)
       -> 200 OK
```

最终拓扑恢复为：

```json
{"management_interface":"enp0s2","interfaces":[{"name":"lan","role":"lan","port":"enp0s3"},{"name":"wan","role":"wan","port":"enp0s4"}],"orchestration_groups":[{"name":"inline-a","ports":[{"interface":"enp0s5","direction":"lan_facing"},{"interface":"enp0s6","direction":"wan_facing"}]}]}
```

## 4. 浏览器验证

- 编辑弹窗：LAN-facing 显示 `eth3 · enp0s5`，WAN-facing 显示 `eth4 · enp0s6`；值分别为 `enp0s5`/`enp0s6`。
- 保存：PUT 200，弹窗关闭，列表回读 `inline-a`，Toast“编排组已保存”。
- 删除：DELETE 204，列表清空。
- 新增：POST 201，列表回读 `inline-b`。
- 恢复：删除 `inline-b` 后 POST 201 恢复 `inline-a`。
- 1440×900、1920×1080 截图见证据目录。
- Gateway 21 页、Orchestrator 11 页导航未变化。

## 5. 构建

```sh
cd /root/work/ly-route-ui-redesign
./scripts/build-product-ui.sh --product gateway --out /tmp/gw-ui-p2
./scripts/build-product-ui.sh --product orchestrator --out /tmp/orch-ui-p2
./scripts/build-controller-shell.sh --product gateway --out /tmp/gw-ui-p2-controller
./scripts/build-controller-shell.sh --product orchestrator --out /tmp/orch-ui-p2-controller
```

产物 hash：

```text
Gateway      app.js  5f30ed4db99e061bb847c8a642f6da3d1464ca9a7f7fb3851aed9f2a5cb06227
Gateway      styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
Orchestrator app.js  dba17962b33a27dd5c7c617e3b7ff978daa93bb459423c75230763e931c3aad8
Orchestrator styles  6b58f3314c216019af7c721a6de29af7e193d52595a2ea3f10d8abaa6641cbfe
```

## 6. 证据

`docs/ui-redesign/evidence/orchestrator-p2-groups/`

- `groups-list-1440.png` / `groups-list-1920.png`
- `groups-edit-preselect-1440.png` / `groups-edit-preselect-1920.png`
- `crud-created-1440.png`（inline-b，201 后回读）
- `crud-deleted-1440.png`（DELETE 204 后空列表）
- `crud-restored-1440.png`（inline-a 恢复）

## 7. 后端缺口

- 编排组重命名没有独立 API；`PUT /orchestration-groups/{name}` 拒绝 body name 与 path 不一致。若产品要求改名，需要后端提供原子 rename + policy reference migration，UI 不自行拼多请求。
- 编排组端口使用系统接口名；`/interfaces.name` 是逻辑别名。UI 已按 `system_name` 提交，避免真实数据面 422。
