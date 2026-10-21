# 真实内核 CBT 实验

此实验在一次性 QEMU Ubuntu 24.04 guest 中编译并加载真实 dattobd 模块，验证普通 ext4、XFS 数据卷的全量及变更块增量备份。单元测试或模拟驱动不作为本实验验收依据。脚本存在不代表支持已验证，必须检查对应提交的真实运行结果。

## 隔离范围

宿主机只下载镜像、创建普通 qcow2 / raw 文件并启动 QEMU，不格式化或挂载任何宿主机块设备。没有将宿主机 `/dev`、业务盘或共享文件系统传入 guest。guest 仅格式化同时满足以下条件的两块盘：virtio serial 分别为 `OPENVAULT_CBT_EXT4` / `OPENVAULT_CBT_XFS`、大小恰为 512 MiB、没有分区、挂载点或已有文件系统。任何检查失败即停止。

Linux 宿主机需要 x86_64、Go 1.23+、QEMU、cloud-image-utils、OpenSSH 客户端、curl 和 Python 3。GitHub 工作流安装所需宿主工具；特权的文件系统操作和驱动加载仅发生在 guest。`auto` 默认使用 TCG，避免仅凭 `/dev/kvm` 存在就假设嵌套虚拟化可用；在已知可用的主机上可显式选择 KVM。二者均启动真实 Linux 内核，TCG 速度较慢。

## 运行

当前 `feature/linux-cbt` 分支每次 push 会触发独立的 **CBT real kernel lab**。工作流同时支持 `workflow_dispatch`；合并到默认分支后，可在 GitHub Actions 中选择 **CBT real kernel lab → Run workflow** 手动运行。此流程不改变普通测试和发布流程。也可以在隔离 Linux 实验主机从仓库根目录运行：

```sh
CBT_LAB_ACCEL=auto bash scripts/cbt-lab/host.sh
```

脚本打印一次性工作目录及结果目录。默认结果保存在 `artifacts/cbt-kernel-lab`。工作目录中的虚拟磁盘保留以便调查，本地运行完后由部署者确认路径再自行删除；GitHub runner 会随任务结束回收。请预留约 10 GiB 可用空间，允许下载官方云镜像以及 guest 安装依赖。没有将私钥或备份密钥上传到工作流 artifact。

`CBT_LAB_ACCEL` 可选 `auto`、`kvm` 或 `tcg`；`CBT_LAB_RESULTS` 指定结果目录。默认云镜像目录为 `https://cloud-images.ubuntu.com/noble/current`。`CBT_LAB_IMAGE_BASE` 可指定 Canonical 的历史镜像目录，以复现实验；镜像及 `SHA256SUMS` 必须来自同一可信来源。使用同一镜像目录 `unpacked` 中校验过的 kernel/initrd 直接启动，强制输出串口日志，绕过固件及 GRUB；这不验证启动恢复能力。当前镜像随时间更新，实际下载 URL、磁盘及 kernel/initrd 校验值写入结果。SHA256 校验用于检测下载内容与发布清单的一致性，没有替代镜像来源认证。

## 固定条件

- guest 必须报告 Ubuntu `24.04`、`6.8.*-generic` GA 内核；其他发行版和 HWE 内核直接失败，不扩展为已支持。
- dattobd 固定为 `v0.11.10` 的 commit `229ca998ce570ab1b74172d55dda62783d5b2d8d`；构建前验证 tag 解析结果。
- 两个文件系统使用独立备份仓库和 agent 状态，minor 分别为 0、1，期间不卸载驱动。
- virtio 磁盘配置不向宿主传递 discard，guest 对新建实验盘设置并确认 `discard_max_bytes=0`，使用 `nodiscard` 挂载。
- 模块自身在创建快照与状态切换时协调冻结 / 解冻，测试不在外部先执行 `fsfreeze`。
- 仓库密钥使用 `openssl rand -hex 32` 生成 64 位十六进制文本（解码为 32 字节），权限 `0600`，不会出现在日志和 artifact 中。

## 验收流程

每种文件系统均执行：

1. 写入固定测试内容，包括大文件、100 KiB 文件、待删除文件和待重命名文件；记录 payload 文件 SHA256 清单。
2. 调用真实 `backup` 创建首次全量，同时通过专用于实验的 `-oracle-target` 从**同一时间点的快照**复制全盘镜像作为独立 oracle。
3. 使用 `verify` 和 `restore` 重建全量镜像，要求与 oracle 字节完全相同且 SHA256 相同。
4. 对重建镜像执行只读 `e2fsck -fn` 或 `xfs_repair -n -f`，随后只读挂载并比对 payload 文件清单。
5. 修改 100 KiB 文件，创建、删除、重命名文件，并对大文件局部覆盖；执行第二次真实变更块备份。
6. 要求增量 `parent` 对应全量代，并且 `0 < source_read_bytes < size`。再次执行重建、oracle 字节对比、文件系统检查及文件清单比对。
7. 再次校验旧全量代，确认旧代仍可校验；收集驱动状态、内核日志、配置及每次 CLI JSON。

`-oracle-target` 完整扫描仅用于实验验证，必须显式设置 `OPEN_VAULT_CBT_LAB=1` 才可使用；测试脚本已设置。不属于生产增量路径，oracle 读取量应与 `source_read_bytes` 分开记录。增量成功同时需要**正确的镜像结果和减少的源读取量**，不能只凭“incremental”标签判断。

本实验预期驱动冻结产生可检查的文件系统快照。如文件系统检查失败，任务直接失败，不自动修复后宣布通过。失败恢复镜像及虚拟盘留在实验 workspace 供排查。

## 结果与边界

成功必须有 `SUCCESS`、ext4/XFS 两套 full / incremental JSON、四份镜像 hash 对比、四份文件系统检查日志和文件清单对比；任一步失败工作流即失败。工作流 artifact 还包含实际 guest 内核版本、模块信息、固定源码 commit、驱动状态和内核日志。

这只能验证指定 Ubuntu GA 内核、虚拟盘和测试工作负载。不能据此声明已支持宿主业务盘、其他内核、LVM、RAID、LUKS、根卷启动恢复、并发数据库一致性或所有断电场景。应继续扩展故障注入、COW 空间耗尽、链中断和长时间负载验证。

参考：[dattobd v0.11.10](https://github.com/datto/dattobd/releases/tag/v0.11.10)、[驱动操作手册](https://github.com/datto/dattobd/blob/v0.11.10/doc/dbdctl.8.md)、[Canonical 本地 cloud-init + QEMU](https://ubuntu.com/docs/public-images/public-images-how-to/use-local-cloud-init-ds/)。
