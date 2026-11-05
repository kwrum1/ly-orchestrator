# 从网关真机踩坑同步到编排器（2026-11）

来源：Ly Route 1.2 在 J1900 真机上的验收记录
（`ly-route/docs/HW-ACCEPTANCE-PITFALLS-J1900-1.2.md`）。

编排器与网关是独立仓库，但两侧的 `internal/httpapi`、`internal/runtime/*`、
`applystate` 等包在拆分时是同一套模型的副本。网关在真机上踩到的“事务/回执/
重启后状态”问题，编排器里存在同型代码。本文记录已同步的修复、核对结论，以及
明确不适用的部分，避免下一次在编排器上重复排查。

---

## 1. 已同步到编排器的修复

### 1.1 desired source hash 加入构建版本

**网关经验**

只 hash 配置文档、不 hash 构建版本时，升级后旧 committed generation 会被
误判为“配置没变”，apply 返回 `unchanged`，新二进制的编程逻辑没有真正跑一遍。

**编排器同型问题**

`backend/internal/httpapi/runtime_apply_authority.go` 的
`runtimeDesiredSource` 只有 schema/product/documents，没有 build。

**已改**

```go
type runtimeDesiredSource struct {
    Schema    string
    Product   string
    Build     string `json:"build"`
    Documents []...
}
...
Build: server.version,
```

升级后 source hash 必变，强制重建 plan。

### 1.2 VPP 重启后不要相信 “unchanged”

**网关经验**

VPP 重启会丢掉运行时编程（LAN/NAT/ABF 等），但回执、artifact 都还是匹配的；
no-op 路径会错误地返回 `unchanged`，数据面实际是空的。必须在 no-op 前比较
“VPP 进程启动时间”和“上次提交时间”，VPP 更年轻就重建 plan。

**编排器同型问题**

`tryNoopRuntimeApply` 只校验 source hash 与 live artifact/readback，没有检查
VPP 是否在提交后重启。

**已改**

在 no-op 校验通过后增加：

```go
if vppUptime, ok := liveVPPUptimeSeconds(ctx); ok &&
   vppRestartedAfterCommit(vppUptime, result.AppliedAt, server.now().UTC()) {
    log.Printf("runtime apply: VPP restarted after the committed generation ...")
    return nil, false
}
```

并移植 `liveVPPUptimeSeconds` / `unitUptimeSeconds` / `vppRestartedAfterCommit`
（读 `/proc/<pid>/stat` + `/proc/uptime`，不依赖 systemd 时间格式）。

### 1.3 空 VPP operations 不等于数据面失败

**网关经验**

route-only / DNS-only / policy-only 的 generation 可以没有任何 VPP operations；
旧逻辑把 `operations[] == 0` 直接判成 degraded，导致 apply 被标成 partial。

**编排器同型问题**

`server.go` 的 `vppRuntimeComponent` 同样无条件：

```go
if len(receipt.Operations) == 0 {
    return degraded "VPP apply receipt has no operations"
}
```

**已改**

前面的 live VPP health probe 已经通过；当回执 `status=applied` 且
`operations[]` 为空时，返回 running，并明确原因：

```text
VPP service is healthy; this generation recorded no VPP operations
```

### 1.4 旧回执缺少 applied_at/readback_at

**网关经验**

`vpp-apply` 按契约只写 transaction_id/generation/hash/steps，不写
`applied_at`/`readback_at`；控制面读取器要求这两个字段，导致“永远降级”。

**编排器同型问题**

`readVPPApplyReceipt` 直接解析后返回，缺字段时上层报
`VPP apply receipt identity or timestamps are incomplete`。

**已改**

解析后用回执文件 mtime 兜底：

```go
if receipt.AppliedAt.IsZero() || receipt.ReadbackAt.IsZero() {
    if info, err := os.Stat(path); err == nil {
        committed := info.ModTime().UTC()
        ...
    }
}
```

回执是原子写入的，mtime 就是提交时间。

---

## 2. 核对后“已经是对的”部分（不要再改坏）

| 网关坑 | 编排器现状 | 结论 |
|---|---|---|
| 策略变更后旧 flow 继续走旧路径 | `ly_orch_commit()` 创建全新 worker，flow 表清零，旧 workers 释放 | 已经天然失效，不需要额外 generation 字段 |
| 策略顺序依赖 map/数据库顺序 | `ORCHESTRATOR-POLICY-ORDER.md` 明确以 JSON 数组位置为唯一顺序，`ParsePolicy/CompilePolicy` 使用派生序号 + ID tie-breaker | 保持 |
| 删除策略后旧规则残留 | 透明 VPP 命令从完整 desired policy 重建；candidate→active 整体替换 | 保持；不要在 delete 路径复用旧 payload |
| 故障旁路把 committed generation 改坏 | bypass 只翻 VPP 插件标志，不改 candidate/active/generation | 保持 |
| 升级后控制面没起来 | `orchestrator_upgrade_install.go` 的 `health_check()` 最多等 120 秒，确认 control-api + /health | 保持；不要在安装未完成时重启 |
| 网关专属 DNS/PPPoE 单元泄漏到编排器 | `product.Orchestrator()` + `profile.AllowsService` 过滤 | 保持；不要重新引入 |

---

## 3. 明确不适用（不要照搬）

- **DNS UDP 回复长度**：编排器没有 VPP DNS 适配器，不需要 `socklen` 修复。
- **SmartDNS 单默认 nameserver**：编排器不跑 SmartDNS。
- **PPPoE 幂等/RA 有限生命周期**：编排器不拨号、不发 RA。
- **delegated prefix / link-local ND**：编排器是 L2 透明串接，不持有 LAN IPv6 网关。
- **domain_ip_set route-only**：编排器没有 DNS 观测资源。

这些坑只属于网关路径；在编排器里引入同名机制反而会增加无关复杂度。

---

## 4. 真机验收方法（同样适用）

- 客户端测试必须绑定到被测 LAN 口；笔记本/测试机的 VPN 可能抢走默认路由，
  导致“网关看起来不通”，实际请求没经过设备。
- 策略/编排变更后使用新的 5-tuple 验收；不要用一条已经建立的连接判断策略没生效
  （编排器自身会在 commit 时清 flow，但上游设备/浏览器可能复用连接）。
- 升级/回滚后先确认 `systemctl is-active ly-route-control-api` 与 `/health`，
  再看数据面。
- 接口身份不要假设 `ethN` 固定；按 MAC/PCI/系统名解析后再写入拓扑/策略。

---

## 5. 验证

```bash
cd backend
GOFLAGS=-mod=mod go build ./...
go test ./internal/httpapi -run 'TestRuntimeApply|TestVPP|TestOrchestrator' -count=1
```

重点观察：

- 升级后第一次 apply 不是 `unchanged`（build 进入 hash）。
- 手动 `systemctl restart vpp` 后 apply 触发完整 replay（VPP uptime 检查）。
- 只有策略/镜像变更、没有 VPP operations 的 generation 不会显示 degraded。
- 旧回执（无 applied_at/readback_at）不会让 vpp 组件降级。
