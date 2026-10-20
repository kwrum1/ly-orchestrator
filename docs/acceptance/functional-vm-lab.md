# Ly Route 功能验收（VM 实验室）

## 1. 目标与边界

本阶段只做**功能验收**，不把 VM 吞吐数字当作生产性能结论。

顺序固定为：

1. 三 VM 家用局域网拓扑功能验收；
2. 基于真实功能页面做 UI 重构；
3. UI 逐页验收；
4. 最后才进入 J1900/i211 真机与性能/长稳验收。

**配置写入硬约束：所有 WAN/LAN/PPPoE/DHCP/DNS/NAT/QoS/安全配置必须通过真实 UI 操作完成，禁止用 API POST/PATCH 绕过 UI。API 只允许用于登录后的只读回读和证据采集。**

数据面接管原则：

- **物理机场景优先**：RDMA → AF_XDP zero-copy → AF_XDP copy → DPDK/VFIO → DPDK/UIO。
- **VMnet 只做显式回退**：通过 `LY_ROUTE_VMNET_AF_PACKET_ACCEPTANCE=true` 允许 e1000/vmxnet3 使用 AF_PACKET host-interface。
- 该开关默认关闭，不由物理机镜像设置，不改变物理机选择顺序。
- VM 验收只验证功能路径，不代表生产性能。

## 2. 三 VM 拓扑

```text
                    QEMU user net (mgmt)
                     10.0.2.0/24
        +----------------+----------------+
        |                |                |
   [pppoe-server]     [router]        [client]
      eth0 WAN <-----> eth1 WAN
                         eth2 LAN <-----> eth0
```

网络：

| 网络 | 用途 | 成员 |
|---|---|---|
| `br-wan` | PPPoE 接入侧 | `tap-pppoe-wan`, `tap-router-wan` |
| `br-lan` | 家庭 LAN | `tap-router-lan`, `tap-client-lan` |
| QEMU user net | 管理/取证 | 每台 VM 一个 mgmt NIC + hostfwd |

VM：

| VM | 角色 | 系统 | 接口 |
|---|---|---|---|
| `ly-lab-pppoe` | PPPoE Server / 模拟上游 | Debian 12 cloud image | eth0=WAN，eth1=mgmt |
| `ly-lab-router` | Ly Route Gateway | final3 x86 disk | eth0=mgmt，eth1=WAN，eth2=LAN |
| `ly-lab-client` | 家庭客户端 | Debian 12 cloud image | eth0=LAN，eth1=mgmt |

地址规划：

| 项目 | 值 |
|---|---|
| Router mgmt | `10.0.2.15/24`，hostfwd `10443` |
| PPPoE Server mgmt | `10.0.2.16/24`，hostfwd `10023/10080` |
| Client mgmt | `10.0.2.17/24`，hostfwd `10024` |
| PPPoE Server WAN | `100.64.0.1` |
| PPPoE 地址池 | `100.64.0.100-110` |
| Router LAN | `192.168.88.1/24` |
| Client LAN | DHCP `192.168.88.100-200` |
| PPPoE 账号 | `labuser` / `labpass` |
| 上游测试服务 | `http://100.64.0.1:8080/health` |
| 上游 DNS 名 | `pppoe.test -> 100.64.0.1` |

## 3. 数据面路径

VMnet 验收路径：

```text
router WAN eth1 -> VPP AF_PACKET host-interface
router LAN eth2 -> VPP AF_PACKET host-interface
PPPoE -> VPP native PPPoE client plugin
NAT44 -> VPP
LAN 客户端 -> 路由器 DHCP/DNS/NAT -> PPPoE -> pppoe-server
```

要求：

- `LY_ROUTE_VMNET_AF_PACKET_ACCEPTANCE=true` 仅在 VM 实验室设置。
- `runtime-check.sh` 对 e1000/vmxnet3 生成 `af_packet/linux_packet_socket` 单候选 proof。
- `SelectNativePath` 仍优先 native/DPDK；AF_PACKET 分数最低，只作为回退。
- 真实物理机不设置该变量，不会走 AF_PACKET。

## 4. 功能验收矩阵

| ID | 验收项 | 验收方法 | 通过标准 |
|---|---|---|---|
| F01 | 三 VM 启动 | QEMU 进程/串口/SSH | 三台 VM 均可达 mgmt |
| F02 | Router API | `/api/v1/health` | 200，degraded=false |
| F03 | VPP 基线 | `vppctl show version` | 26.06 release |
| F04 | VMnet 数据面 | `/api/v1/interfaces` / `runtime/status` | eth1/eth2 进入 VPP/AF_PACKET，非 kernel_stack |
| F05 | PPPoE 建链 | `vppctl show pppoe session` + server ppp if | client-ip 在 100.64.0.100-110 |
| F06 | WAN 连通 | Router ping 100.64.0.1 | 成功 |
| F07 | LAN 地址 | Client DHCP | 192.168.88.100-200，网关/DNS=192.168.88.1 |
| F08 | LAN→WAN 转发 | Client ping 100.64.0.1 | 成功 |
| F09 | NAT44 | `vppctl show nat44 sessions` | 存在 client 到 WAN 的会话 |
| F10 | DNS | Client `nslookup pppoe.test` | 解析到 100.64.0.1 |
| F11 | HTTP | Client `curl http://100.64.0.1:8080/health` | 200 |
| F12 | 断线重连 | 重启 PPPoE server 或 router WAN | 会话恢复，F06/F08 再通过 |
| F13 | 并发 | 20-50 个并发 ping/HTTP | 无持续丢包/进程崩溃 |
| F14 | 配置回读 | API/UI 读取 WAN/LAN/DHCP/NAT | 与提交一致，密码不回显 |
| F15 | 证据归档 | 日志、VPP 输出、API JSON | 每项有可复现证据 |

性能、长稳、J1900 真机不在此阶段判定。

## 5. UI 阶段（功能验收通过后）

UI 重构只基于已通过功能：

- 系统概览、接口、WAN/LAN、PPPoE、DHCP、DNS、NAT、策略路由、代理、QoS、安全；
- 逐页视觉/交互/表单/回读；
- 登录、改密、错误态、空态、加载态；
- 1440×900 + 紧凑分辨率；
- 形成截图基线和 UI 验收清单。

## 6. 编排器职责

编排器负责：

0. 用 Playwright/真实浏览器驱动 UI 完成所有配置写入；API 只做回读；
1. 下载/校验 Debian cloud image；
2. 生成 cloud-init seed；
3. 创建 bridge/tap；
4. 启动三台 QEMU VM；
5. 准备 router 磁盘的 `installed-network.json` / VMnet 环境；
6. 调用 router API 完成 WAN/LAN/PPPoE 配置；
7. 执行 F01-F15 并保存证据；
8. 清理 VM/网络，不残留 tap/bridge。

建议目录：

```text
scripts/acceptance/vm-lab/
  README.md
  topology.env
  orchestrate.py
  cloud-init/
    pppoe-server.yaml
    client.yaml
  ui-configure.mjs   # Playwright real UI writes
  ui-evidence.mjs    # API readback only
  test-functional.py
```

## 7. 当前状态

- [x] VMnet AF_PACKET 显式回退代码与选择测试。
- [x] Gateway final3 产物。
- [ ] 三 VM 编排脚本首版。
- [ ] PPPoE Server / Client cloud-init。
- [ ] Router 磁盘 VMnet 映射。
- [ ] F01-F15 执行与证据。
- [ ] UI 重构与验收。
- [ ] Orchestrator 产物构建（进行中）。

## 8. 重要约束

- 功能验收以“独立客户端观察到结果”为准，不以 mock/fixture/单元测试代替。
- VM 数字不进入生产性能报告。
- 任何 VMnet 回退都必须由显式环境变量启用，并在证据中记录；物理机默认路径不得改变。
- 不修改真实物理机接管脚本的优先级；只增加 VM 专用回退分支。
