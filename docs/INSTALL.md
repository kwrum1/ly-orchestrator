# 安装

## 要求

- x86_64 平台；
- 至少 2 张数据网卡（WAN-facing / LAN-facing），另需管理口；
- 4 GB 以上系统盘；
- BIOS 或 UEFI 启动；
- 数据口建议使用 VPP 支持的网卡（Intel igb/igc、部分 Broadcom/Mellanox 等）。

## ISO 安装

### 写入 U 盘

```bash
# Linux / macOS，确认 /dev/sdX 是 U 盘后再执行
sudo dd if=ly-route-orchestrator-1.0.3-amd64.iso of=/dev/sdX bs=4M status=progress conv=fsync
```

也可以使用 balenaEtcher、Rufus（DD 模式）等工具。

### 安装向导

1. 选择目标磁盘（会清空该磁盘）；
2. 选择管理网口；
3. 设置管理地址、网关、DNS 与管理员密码；
4. 确认后执行安装，等待写入与校验完成；
5. 重启，拔出 U 盘。

无 DHCP 时安装器回退到 `10.1.18.240/24`。首次启动后浏览器访问
`https://<管理地址>/`，使用安装时设置的管理员账号登录；首次登录强制修改密码。

### 安装后检查

```bash
systemctl status ly-route-control-api vpp
/usr/lib/ly-route/ly-route-control --version   # 期望 1.0.3
curl -sk https://127.0.0.1/api/v1/health
```

管理口应保留在管理面，数据口用于透明串接，不配置业务 IP。

## 网络接入

1. 把 WAN-facing 口接上游网关/防火墙，LAN-facing 口接下游业务网络；
2. 在控制台 **网卡设置** 中为两个口分配 WAN / LAN 角色；
3. 创建编排组，绑定这对方向端口；
4. 在 **流量编排** 中添加规则，选择直通、丢弃或经过服务链；
5. 可选：配置流量镜像、聚合、故障旁路。

透明串接不改变现有 IP 与路由，业务侧无需修改网关或客户端配置。

## 故障排查

| 现象 | 处理 |
| --- | --- |
| 安装器找不到磁盘 | 检查 BIOS/UEFI 模式与 RAID 设置，改用 AHCI/JBOD |
| 数据口未接管 | 查看 `ly-route-control` 日志与 VPP 接口状态 |
| Web 无法访问 | 确认管理口地址与 `ly-route-control-api` 状态 |
| 业务不通 | 检查编排组端口角色、策略顺序与旁路状态 |

更多问题请提交 GitHub Issue，并附上版本、网卡型号、拓扑与相关日志。
