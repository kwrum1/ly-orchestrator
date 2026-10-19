# 后续工作计划 / Work Plan

## 中文

1. 修复实际使用中发现的缺陷，并只回归受影响功能和发行门禁。
2. 在 VPP 26.06 基线上完成插件、CLI、运行态和 AF_XDP copy 的定向回归，
   并以 J1900/i211 真机结果作为最终准入。
3. 实现管理口虚拟化，使管理地址可与 LAN 数据口共享，并提供配置回滚、失联保护和
   恢复入口。
4. 优化 Smart QoS，覆盖多带宽档位、公平性、长期稳定性、代理 WAN 交互和遥测。
5. 在真实物理 PCI 网卡上验证 VPP 原生和 DPDK/VFIO 路径，并验证真实 IPv6
   PPPoE/DHCPv6-PD、前缀派发和 RA。

工作顺序以用户可用性为准：先修复、再定向回归、最后构建发行物。已验收功能不因
夹具重启或拓扑变化重复验收；性能结论只来自目标物理硬件。

## English

1. Fix field defects with focused regression and release gates.
2. Harden the VPP 26.06 baseline with focused plugin, CLI, runtime and AF_XDP
   copy regression, then gate it on real J1900/i211 hardware.
3. Virtualize management so it can share a LAN data port with rollback and
   management-loss protection.
4. Improve Smart QoS across bandwidth tiers, fairness, long-run stability,
   proxy-WAN interaction and telemetry.
5. Qualify VPP-native and DPDK/VFIO paths on physical PCI NICs, then validate
   real IPv6 PPPoE/DHCPv6-PD, prefix delegation and RA.
