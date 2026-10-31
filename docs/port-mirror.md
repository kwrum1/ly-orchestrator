# 端口镜像（镜像口）

编排器可以把任意物理数据口的流量镜像到一个专用“镜像口”，由 VPP SPAN 实现。
镜像口是编排器功能：它随编排器仓库发布，网关产品不再提供该能力。

## 配置模型

接口资源（`/api/v1/interfaces`）有两个字段，都写在**镜像目标口**的文档上：

```json
{
  "id": "eth2",
  "mirror": true,
  "mirror_sources": [
    {"interface": "eth1", "direction": "both"}
  ]
}
```

- `mirror`：该物理口是镜像目标口；同一时刻只能有一个。
- `mirror_sources[].interface`：镜像源，必须是已接管的物理数据口，可多个。
- `mirror_sources[].direction`：`rx` / `tx` / `both`，缺省 `both`。
- `mirror_sources[].l2`：缺省 `false`（见“已知限制”）。

取消镜像：`{"mirror": false, "mirror_sources": []}`。

## 校验规则

- 只能有一个 `mirror=true` 的接口。
- 镜像口不能是管理口，不能有 LAN/WAN 角色，不能属于编排组。
- 镜像源必须是本机物理口、已接管，不能是目标口自身，不能是管理口。
- 方向只能是 `rx`、`tx`、`both`。
- 写入时校验（422 `invalid_desired_resource`）；apply 时再次校验（防止旧版本写入的坏配置）。

## VPP 命令与清理顺序

每个包含 VPP 附着的 generation 都会生成 `vpp.interface-span` 操作（属于
interfaces supplemental owner），因此：

- 新配置：先对所有候选源（以及目标口）执行 `l2 disable` 和 `disable`，再下发目标，最后读回；
- 删除/改绑：同一操作内先 disable，再重建，不会残留 `DELETED (idx)` 会话。

```
?set interface span <if> l2 disable
?set interface span <if> disable
...
set interface span <src> destination <dst> both
show interface span
```

（只有 `l2=true` 时使用 `set interface span <src> l2 destination ...`。）

`disable` 必须两种模式都发：实测 `l2 disable` 只清 L2 会话，普通 `disable` 只清非 L2 会话。

apply 结束后用 `show interface span` 精确比对 desired（多一个、少一个、方向不符都失败）。
读回缓存会在这个操作前失效，避免状态轮询刚缓存的空表把已收敛的 apply 判失败。

## 重启回放

镜像操作写入 `/var/lib/ly-route/vpp/operations.json`，由 `ly-route-vpp-apply.service`
（`PartOf=vpp.service`）在 VPP 重启后重放，而不是写 startup.conf。

## 健康

运行状态新增 `mirror` 组件：

- 每个会话输出 `源 -> 目标 (方向, l2/l3)`；
- 会话缺失/方向不符 → `degraded`；
- 目标口无载波 → 仍为 `running`，reason 中告警（源口转发不受影响）。

## UI

编排器控制台 “拓扑与编排 → 镜像口”（`/mirror`）：

- 选择一个已接管的物理数据口作为镜像目标口；
- 勾选其它物理口作为镜像源并选择方向（双向/仅接收/仅发送）；
- “取消镜像”清除目标口标记与全部会话；
- “当前状态”按会话列出 `源 → 目标（方向）` 和“已生效 / 未生效”，数据来自运行时
  `mirror` 组件的 `show interface span` 读回；目标口无载波时仅显示告警。

页面只编辑接口资源的 `mirror` / `mirror_sources` 字段，不直接操作 VPP；
保存和取消都会立即触发 apply 事务，由 apply 下发并读回校验。镜像源状态在
“系统状态”页的 `mirror` 组件里同样可见。

## 已知限制

- **l2 默认关闭**：VPP 26.06 只在 L2 特性弧（`l2-input`/`l2-output`）注册 `span-l2-*`，
  路由模式的数据口不经过该弧，`l2` 会话不会复制任何帧（实验室实测）。
  因此产品默认 `l2=false`；只有桥接/交换场景才应打开 `l2`，此时非 IP 帧也会镜像。
- 目标口无载波时镜像帧在目标口 output 被丢弃：实验室 VPP 版本计到
  `show errors` 的 `lyroute-<if>-output interface is down`，而不一定增加接口 `tx-error`；
  源口转发与会话本身不受影响。
- 镜像不保证线速/不保证不丢帧（J1900 性能不在本期目标内）。
- 一个源可以镜像到多个目标由 VPP 支持，但产品模型只允许一个镜像目标口。
- 删除镜像源所属端口（解绑）时，旧会话随 VPP 重启消失；正常 apply 会先 disable 所有
  仍存在的候选口。
