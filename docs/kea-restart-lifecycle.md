# Kea 重启生命周期 / Kea Restart Lifecycle

状态 / Status: 2026-10-22, P0-3 修复，待主开发在共享 lab 做受控 reboot 验收。

## 根因 / Root cause

0ce 镜像在共享 lab 的一次重启日志（`journalctl -b -1`）中复现：

- `02:20:42` `ly-route-vpp-apply.service` 完成，但 LAN tap 尚未创建。
- `02:22:12` `kea-dhcp4-server.service: start-pre operation timed out`。
  旧 `ExecStartPre` 用 `seq 1 60` 的计数循环匹配任意 `lylan-*`，在 TCG
  虚拟机上单次 `ip` 调用很慢，整个 start-pre 超过 systemd 默认
  `TimeoutStartSec=90s`。
- `02:23:54` Kea 在 tap 出现前运行：
  `Failed to select interface: interface 'lylan-eth2' doesn't exist`。
- `02:24:10` VPP 创建 `lylan-eth2`，`02:24:16` Kea 才成功监听。
  DHCP 在重启后约 3.5 分钟内不可用，并且期间反复失败/重启。

另外两个独立问题：

1. 空租约库：Kea 在第一条租约前会创建 0 字节 memfile。旧解析器把
   0 字节文件当成错误，`GET /api/v1/dhcp/leases` 返回 500。
2. 默认作用域：工厂 Kea 配置必须始终是 LAN `192.168.88.0/24`，不能
   绑定管理网（lab 管理网为 `10.0.2.0/24`）。

## 修复 / Fix

### 1. 精确、限时的 LAN 等待

新增 `packaging/rootfs-overlay/usr/lib/ly-route/kea-wait-lan.sh`：

- 从 `/etc/kea/kea-dhcp4.conf` 读取**唯一**的配置接口名，而不是匹配任意
  `lylan-*`。
- 等待该接口存在且带 `UP` 标志。
- 等待一个非 `169.254/16` 的 IPv4 地址（即 VPP 已装好 LAN 网关地址）。
- 使用墙钟 deadline（默认 45 秒），在慢速虚拟化平台上也不会因为单次
  `ip` 调用耗时而突破 systemd 超时。
- 配置缺失/非法时 5 秒内以 exit 2 快速失败，不进入长等待。

### 2. systemd 与 udev

`20-ly-route-lan-retry.conf`：

- `ExecStartPre=/usr/lib/ly-route/kea-wait-lan.sh`
- `TimeoutStartSec=120`
- `Restart=on-failure`，`RestartSec=3`
- `StartLimitIntervalSec=600`，`StartLimitBurst=10`（总重试有界）
- `BindsTo=vpp.service` + `After=vpp.service`（VPP 重启时 Kea 一起停止）
- 保留 `Wants=ly-route-vpp-apply.service`，但不用 `Requires=`，避免
  apply 失败时 Kea 被永久阻塞。

新增 `/etc/udev/rules.d/90-ly-route-kea-lan.rules`：
`lylan-*` 网卡 add 事件通过 `SYSTEMD_WANTS` 拉起 Kea。这样 tap 晚于
Kea 出现、或 VPP 重启后重建 tap 时，Kea 会立即重新启动。

`firstboot.sh` 删除早期的 `systemctl restart --no-block kea`，避免与
dataplane 竞争并打断正在进行的等待。

### 3. 空租约库

`KeaMemfileLeaseCollector` 把 0 字节、仅换行、仅表头的 memfile 视为
合法空库，返回 `[]`（HTTP 200）。文件缺失或格式损坏仍然显式失败，
不会返回缓存/陈旧租约。

### 4. 默认作用域

工厂 `/etc/kea/kea-dhcp4.conf` 保持：

- 接口：`lylan-eth2`（VPP LAN control host interface，`lylan-` 前缀）
- 子网：`192.168.88.0/24`
- 池：`192.168.88.100 - 192.168.88.199`
- 网关/DNS：`192.168.88.1`
- 不包含 `10.0.2.0/24`

`dhcpLANControlInterface` 只接受 `Role=lan` 的 assignment；管理/WAN
assignment 不会被当成 DHCP LAN（新增回归测试）。

## 测试 / Tests

已通过：

```sh
cd backend
GOMODCACHE=/root/go/pkg/mod-alt GOSUMDB=off GOPROXY=off \
  go test ./internal/runtime/service ./internal/httpapi -count=1

./scripts/test-kea-lifecycle.sh
GOMODCACHE=/root/go/pkg/mod-alt GOSUMDB=off GOPROXY=off \
  ./scripts/test-kea-dhcp-netns.sh
GOMODCACHE=/root/go/pkg/mod-alt GOSUMDB=off GOPROXY=off \
  ./scripts/validate-rootfs-scaffold.sh
```

新增测试覆盖：

- 0 字节/空行/仅表头 memfile → 0 条租约、HTTP 200。
- 缺文件/坏格式仍然失败（不返回陈旧租约）。
- 精确接口：相似前缀 `lylan-other0` 不能满足 `lylan-target1` 的等待。
- 接口存在但无 IPv4 地址时继续等待；`--no-address` 可显式跳过。
- 晚到 tap 在等待期间出现 → 等待成功。
- 缺失配置快速 exit 2。
- 默认 Kea 配置只落在 LAN `192.168.88.0/24`，不含管理网。

## 隔离 live 验证 / Isolated live evidence

在独立克隆盘 `router-dhcp.qcow2`（0ce 盘副本，独立 bridge/tap，未改共享
lab、未改 0ce VM）上验证：

- 受控 reboot 后：`kea-dhcp4-server.service` active，`lylan-eth2`
  有 `192.168.88.1/24`，`systemctl --failed` 为 0。
- 晚到 tap：接口不存在时 `ActiveState=activating`、
  `SubState=start-pre`；创建 `lylan-late0` 并配置地址后 Kea 立即
  `DHCPSRV_CFGMGR_ADD_IFACE listening on interface lylan-late0`。
  `udevadm test /sys/class/net/lylan-late0` 输出
  `SYSTEMD_WANTS=kea-dhcp4-server.service`。
- 客户端（host netns, veth on LAN bridge）：
  - 地址 `192.168.88.101/24`
  - 默认路由 `default via 192.168.88.1`
  - lease 中 `option routers 192.168.88.1;`
  - lease 中 `option domain-name-servers 192.168.88.1;`
- 空租约库：把 `/var/lib/kea/kea-leases4.csv` 截为 0 字节后
  `GET /api/v1/dhcp/leases` 返回 HTTP 200，body 含
  `"items":[]`、`"total":0`、`"capabilities":[...available...]`。

## 提交 / Commits

分支：`feat/p0-dhcp-restart`（基于 `0ce76c0`）

```text
40af8a52bc20894d784779d80a4a8f60241c1ab1  fix(gateway): read an empty Kea lease database as zero leases
841953eb9d8fd9da9459db44d5249438228c5511  fix(gateway): gate Kea on the exact LAN tap with bounded retries
```

Cherry-pick 顺序：

```sh
git cherry-pick 40af8a52bc20894d784779d80a4a8f60241c1ab1
git cherry-pick 841953eb9d8fd9da9459db44d5249438228c5511
```

可选：同时 cherry-pick 本文件所在提交（`docs(gateway): record the Kea
restart lifecycle and P0-3 evidence`）以保留证据说明。

已在主工作树当前 HEAD `9d663b5` 的临时 worktree 上试 cherry-pick 两个
代码提交，**无冲突**；最终以主开发合并时的 HEAD 为准。

不涉及前端、安全、P0-1/P0-5 的代码路径；主要文件：
`backend/internal/runtime/service/kea_leases.go`、
`backend/internal/httpapi/kea_*_test.go`、
`packaging/rootfs-overlay/etc/systemd/system/kea-dhcp4-server.service.d/`、
`packaging/rootfs-overlay/usr/lib/ly-route/kea-wait-lan.sh`、
`packaging/rootfs-overlay/etc/udev/rules.d/`、
`scripts/test-kea-*`、`scripts/build-*`（仅 Kea/udev 相关行）。

## 共享 lab 受控 reboot 验收 / Controlled reboot checklist

主开发合并后，在共享 lab 上执行：

```sh
# router VM
systemctl is-active kea-dhcp4-server
systemctl --failed
ip -br addr show lylan-eth2
journalctl -b -u kea-dhcp4-server | grep -E 'ADD_IFACE|STARTED|doesn.t exist|timeout'
cat /var/lib/kea/kea-leases4.csv

# client VM / LAN test host
ip -4 addr show
ip -4 route
# 期望：192.168.88.x/24，default via 192.168.88.1，DNS 192.168.88.1
```

通过标准：

1. reboot 后 Kea 最终 active，且启动时目标接口已存在；
2. 无 `interface '...' doesn't exist` / `start-pre operation timed out`；
3. 客户端自动获得 `192.168.88.0/24` 地址，网关和 DNS 为 `192.168.88.1`；
4. 空租约库时 `/api/v1/dhcp/leases` 为 200 而不是 500；
5. `systemctl --failed` 无 Kea 相关失败单元。
