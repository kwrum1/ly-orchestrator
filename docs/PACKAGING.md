# 打包与 CI

本仓库的 CI **不编译产品源码**，也不包含产品源码。它把已发布的二进制输入组装成
安装 ISO，并附加到 GitHub Release。

## 输入

| 输入 | 说明 |
| --- | --- |
| 升级包 | `ly-route-orchestrator-<version>-amd64.tar.zst` |
| 基础 ISO | 上一正式版的 `ly-route-orchestrator-<base>-amd64.iso`，提供 Live 启动骨架与 squashfs |
| 版本号 | 目标版本，例如 `1.0.4` |

## 本地组装

```bash
sudo apt-get install -y xorriso zstd squashfs-tools

packaging/build-orchestrator-iso-from-base.sh \
  --version 1.0.4 \
  --package ly-route-orchestrator-1.0.4-amd64.tar.zst \
  --base-iso ly-route-orchestrator-1.0.3-amd64.iso \
  --out dist
```

需要 root 或免密 sudo（用于 loop mount 修改磁盘镜像）。

输出：

```text
dist/ly-route-orchestrator-<version>-amd64.iso
dist/ly-route-orchestrator-<version>-amd64.iso.sha256
```

脚本会：

1. 从基础 ISO 提取 `/live/filesystem.squashfs` 并解包；
2. 从 squashfs 取出基础磁盘镜像；
3. 挂载镜像第 3 分区，覆盖控制面、`vpp-apply`、Web UI 与 `version.env`；
4. 重新压缩磁盘镜像，更新 squashfs 内的镜像与校验文件；
5. 重新打包 squashfs；
6. 用 xorriso 替换 ISO 内的 squashfs，保留原有 BIOS/UEFI 启动结构。

## GitHub Actions

工作流：`.github/workflows/release.yml`

触发方式：

- 手动 `workflow_dispatch`：私有流水线上传升级包后触发；
- 不使用 tag push 自动触发，避免 Release 资产尚未上传时误跑。

手动参数：

| 参数 | 说明 |
| --- | --- |
| `version` | 目标版本，例如 `1.0.4` |
| `base_version` | 基础 ISO 的 Release 标签，例如 `v1.0.3` |
| `package_url` | 升级包直链；留空则从目标版本 Release 下载 |

流程：

1. 下载基础 ISO；
2. 下载目标版本的升级包（或 `package_url`）；
3. 运行 `packaging/build-orchestrator-iso-from-base.sh`；
4. 校验 ISO 内的镜像与版本信息；
5. 生成 `SHA256SUMS`；
6. 创建或更新 `v<version>` Release，上传 ISO 与校验文件。

## 发布约定

- 私有构建流水线先创建目标版本 Release，并上传升级包；
- GitHub Actions 随后补齐 x86_64 ISO；
- 所有发布资产都应附带 `SHA256SUMS`；
- 不要在公开仓库提交源码、密钥、内部构建日志或私有补丁。

## 资源需求

GitHub 托管 runner（`ubuntu-latest`）实测资源需求：

- 磁盘：约 8–10 GB（基础 ISO + squashfs + 4 GB 磁盘镜像 + 新 ISO）；
- 时间：约 15–25 分钟；
- 内存：2 GB 以上。

如果 runner 磁盘不足，可改用自托管的 x86_64 runner；工作流本身不依赖特定主机。
