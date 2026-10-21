# Open Vault

面向 Linux 的开源 CBT 数据卷备份项目：通过内核驱动取得时间点快照和变更块索引，将增量保存到独立的 AES-256-GCM 加密镜像仓库，并提供中文 Web 控制台。

**当前仅供开发验证：真实内核实验已发现增量镜像不一致，尚未修复验收。** 适配目标为 Ubuntu 24.04、x86_64 / amd64、GA 6.8 内核、普通 ext4/XFS 数据卷。实际结果见 [验证记录](docs/CBT-VALIDATION.md)，后续驱动改造边界见 [COW 后端说明](docs/CBT-COW-BACKEND.md)。本项目是独立实现，与 Synology / 群晖没有关联。

## 当前功能

- 首次全量，后续根据真实变更索引读取源快照；显示实际源读取量。
- 独立认证加密块仓库、完整镜像 SHA-256 校验。
- 当前及历史备份版本列表，选择历史版本校验或恢复。
- 恢复到新的普通 `.img` 文件，拒绝覆盖已有路径或写入真实设备。
- 非 root Web 控制台通过受限 Unix socket 调用 root Agent；作业串行、异步记录结果。

当前版本已移除 restic 和旧文件备份流程，不再提供按目录创建任务、文件快照或原有定时任务。旧仓库不会自动转换或删除，迁移说明见 [升级说明](docs/MIGRATION.md)。

## 构建与部署

需要 Go 1.23+。目标机器使用 **amd64** 工具链；已有合适的 Go 安装时直接构建：

```sh
go version
go build -o open-vault .
go build -o open-vault-cbt ./cmd/open-vault-cbt
go build -o open-vault-cbt-agent ./cmd/open-vault-cbt-agent
```

1. 先在可销毁的 QEMU 虚拟机运行 [真实内核实验](docs/CBT-LAB.md)，检查该提交的实验结果。
2. 按 [驱动说明](docs/CBT-DRIVER.md) 准备固定外部驱动构建。项目不会在业务主机自动安装或加载驱动。
3. 配置 [CBT CLI](config.cbt.example.json) 和 [特权 Agent](config.cbt-agent.example.json)，参考 [Agent 部署](docs/CBT-AGENT.md)。
4. 按 [部署指南](docs/DEPLOYMENT.md) 配置非 root Web 控制台，其配置只包含监听地址、API 令牌文件和 Agent socket。
5. 登录后执行备份、校验并恢复到新镜像，核对实际恢复内容。

本机访问 `http://127.0.0.1:8080`。局域网可使用反向代理提供 `http://服务器IP:8080`，示例见部署指南。

## 能力边界

当前支持范围仅为实验性数据卷镜像，不提供根卷/裸机恢复、ARM64 内核 CBT、LVM/RAID/LUKS、应用一致性、多机纳管、自动保留或链合并。整卷上限 256 GiB，最多 128 代；过度碎片化的增量也可能超过清单限制。重启或跟踪连续性丢失需要人工建立新基线。请独立保管加密密钥，并执行恢复演练。

## 开发与参与

```sh
go test ./...
go vet ./...
```

CI 验证用户态加密仓库、镜像恢复、驱动协议模拟、Unix socket 和控制台。真实驱动验收另由隔离 QEMU 实验执行；模拟测试不证明内核支持。

参见 [架构](docs/ARCHITECTURE.md)、[实验命令](docs/CBT-USAGE.md)、[贡献指南](CONTRIBUTING.md)和[安全策略](SECURITY.md)。

## 许可

本项目代码使用 [MIT 许可证](LICENSE)。独立安装的 dattobd 驱动及其外部修改保留上游 GPL 许可；未将内核实现复制或链接进 Go 程序。操作系统包保留各自许可。项目不包含 Synology 专有代码、协议或商标素材。
