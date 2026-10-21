# CBT 数据卷备份：实验命令

本模式与 V1 文件备份控制台独立，使用外部安装的 dattobd 驱动取得真实时间点块快照与变更索引。增量数据保存于独立的 AES-256-GCM 块仓库，未通过 restic 写入。V1 的 restic 仓库和恢复流程不变。

目标环境为 Ubuntu 24.04 amd64、GA `6.8.0-*-generic` 内核及普通 ext4/XFS 数据盘。该范围是软件的能力门槛，实际已验证的内核与场景以 [实验记录](CBT-LAB.md) 为准。不要将 WSL 单元测试当成内核驱动验收。

## 准备

先在可销毁虚拟机使用 [内核实验脚本](../scripts/cbt-lab/host.sh) 验证。源盘必须是独立挂载的数据盘，不能是根卷、boot、LVM、loop、MD 或其他堆叠块设备。外部驱动应由管理员根据固定版本说明安装，模块签名、Secure Boot 和内核升级兼容性另行管理。项目不自动安装或加载内核模块。

源设备 discard 队列能力必须为 0；原驱动过滤 discard，本实现发现该能力启用时拒绝备份。对测试虚拟盘的设置由实验脚本完成；真实设备需管理员明确评估并配置，命令不会修改其 sysfs 队列。

```sh
go build -o open-vault-cbt ./cmd/open-vault-cbt
```

以 [config.cbt.example.json](../config.cbt.example.json) 为基础，填写批准的数据盘、挂载点、驱动 minor 和绝对 dbdctl 路径。创建源盘内的私有 COW 目录（0700）；仓库、密钥和状态目录必须位于源卷之外且彼此分离。密钥是 64 个十六进制字符表示的 32 字节随机密钥，文件权限 0600。例如在已配置的密钥路径生成：

```sh
umask 077
openssl rand -hex 32 > /etc/open-vault/cbt-key
```

镜像仓库密钥与 V1 restic 密码不同，必须独立保管。丢失密钥不能恢复。命令还会检查仓库、密钥、状态和实验 oracle 的底层文件系统，不能通过 bind mount 或路径别名把它们放回源卷。

## 运行

除恢复、校验和仓库状态外，驱动操作需要能够读取批准块设备并控制已加载驱动的管理员身份。所有参数放在操作名称之前。

```sh
sudo ./open-vault-cbt -config /etc/open-vault/cbt.json diagnose
sudo ./open-vault-cbt -config /etc/open-vault/cbt.json backup
sudo ./open-vault-cbt -config /etc/open-vault/cbt.json status
sudo ./open-vault-cbt -config /etc/open-vault/cbt.json verify
sudo ./open-vault-cbt -config /etc/open-vault/cbt.json -target /restore/data-new.img restore
```

首次 backup 建立全量基线；之后读取上次到本次快照边界的真实变更块。输出 `generation.source_read_bytes` 是块仓库从源快照实际读取的数据量；`tracking_index_read_bytes` 是另行读取的 COW 索引元数据量。生成全镜像校验摘要还会读取仓库父链，所以源端少读取不代表仓库端没有全镜像校验工作。

`-generation ID` 可恢复或校验旧版本。恢复只创建全新的普通镜像文件，不写入任何已有路径或真实块设备，不执行引导修复。恢复与校验不要求原始源盘、原驱动或原内核运行；保留 key_file/repository/state_dir 的独立路径配置即可，在恢复主机上可移除 volume 配置。

## 中断、失败与边界

- 每个仓库及代理状态目录只允许一个备份进程。锁目录在进程异常终止后可能保留；确认没有存活写入者、检查驱动与仓库后才可人工移除锁。
- 先持久提交镜像，再切换驱动进入下一阶段。镜像已提交而调用驱动前中断时，可识别相同跟踪代际并完成后续切换，不重复创建镜像。
- 重试释放快照前，会完整校验已提交镜像并重新确认 HEAD 持久性。若提交仅完成了发布、未确认目录同步，错误 JSON 会包含 generation 和 `durability: uncertain`，不会释放快照。
- 若驱动命令执行结果不确定，journal 保留 pending 并拒绝自动继续。不能简单删 journal 重跑；需要人工检查，必要时建立新仓库和新跟踪基线。
- 跟踪状态丢失、主机重启、卷大小或 UUID/序列不符都会拒绝继续增量。当前不提供 initramfs 跟踪自动恢复。
- 最多支持 128 代链，尚无合并与保留策略，也不会自动清理旧 COW 文件。需要容量监控及人工维护。
- 当前实验仓库整卷上限为 256 GiB，达到链长上限会在下一次快照旋转前拒绝。过度碎片化的增量也可能超过清单容量；读取关闭索引后会在任何源数据或 oracle 读取前拒绝，并保留快照供人工处理。驱动索引解析的地址范围不等于仓库容量承诺。
- 本模式提供崩溃一致性层面的块镜像能力；应用一致性、数据库事务协调、多机控制台接入和裸机恢复尚未实现。
- 仓库格式是实验格式，未接受外部安全审计，不承诺未来版本兼容。

`generation.source_read_bytes` 是该代创建时的历史读取量；`this_run_source_read_bytes` 表示本次命令的读取量。仅完成已有提交的后续切换时，后者为 0。

测试专用的 `-oracle-target` 只在 `OPEN_VAULT_CBT_LAB=1` 时允许，额外完整读取快照来形成独立比对镜像，读取量单列为 `oracle_read_bytes`。生产增量命令不使用该参数。
