# 实验性 Linux 真实 CBT 驱动适配

该适配器通过独立安装的 `dbdctl` 命令管理 dattobd，不链接 libdattobd，不包含、复制或修改其内核实现。Open Vault 的 Go 适配器和公开 COW 格式解析器为本项目独立实现；外部驱动及其工具仍按上游许可证分发。上游代码不应被复制进本项目后套用 MIT 许可证。

## 目前限定的环境

- Ubuntu 24.04，amd64，GA `6.8.0-数字-generic` 内核。
- dattobd `v0.11.10`，固定提交 [`229ca998ce570ab1b74172d55dda62783d5b2d8d`](https://github.com/datto/dattobd/tree/229ca998ce570ab1b74172d55dda62783d5b2d8d)。后续上游版本需要重新验证。
- 普通物理盘或虚拟机直接呈现的数据盘及其分区，完整 ext4/XFS 挂载。禁止系统根、boot、bind 子目录、LVM/device-mapper、md、loop、zram、其他堆叠或虚拟块设备。
- COW 目录必须已存在、权限私有（0700），位于被跟踪的数据挂载内，且不能跨入嵌套挂载。持久化 journal、仓库和密钥放在源挂载之外。
- 管理员配置精确的数据卷 allowlist。块设备、挂载点和 COW 目录在启动时解析符号链接；每次操作重新校验。
- 从源卷挂载根到 COW 目录的整个目录链必须由可信身份持有，且不能对组或其他用户开放写权限；仅给 COW 末级目录设 0700 无法防止上级目录将它重命名或替换。

这些是程序的实验性准入条件，不等于全部内核补丁版本已经通过生产认证。`Diagnose` 可以在不支持的环境读取诊断，跟踪与状态转换会拒绝执行。Windows 和 WSL 不属于此版本的真实内核验证环境。

### ARM64 与用户态功能的区别

固定 `v0.11.10` 提交中的 [bio_request_callback.h](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/src/bio_request_callback.h) 在较新内核的提交路径中仅为 `CONFIG_X86` 定义跳过 fentry 指令的长度，其他架构明确标为 `Unsupported architecture`；[bio_helper.h](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/src/bio_helper.h) 标明 Linux 5.9 之后使用该路径，[submit_bio.c](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/src/submit_bio.c) 依赖这一指令处理。[同提交的安装文档](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/INSTALL.md) 也限定发行包为 x86-64，Ubuntu 软件源示例指定 `arch=amd64`。因此不能将这个固定驱动视为支持 Ubuntu 6.8 ARM64，也不能仅移除 Go 架构检查来启用它。Go 用户态程序可针对 ARM64 构建；在具备相应 ARM64 restic 和运行环境时，文件备份及已有镜像的用户态校验、恢复属于独立功能，需要分别验证，不代表 ARM64 内核级 CBT 已获支持。本项目保留 amd64 CBT 门槛。

## 必须禁用 discard/TRIM

固定上游版本的 [`tracer_should_trace_bio`](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/src/tracer_helper.c) 过滤 discard 请求。若源盘允许 discard，仅复制普通写入对应的变更块不能证明镜像与新快照一致。

因此适配器沿 `/sys/dev/block/major:minor` 找到源设备；对分区检查其父盘队列，要求 `queue/discard_max_bytes` 为 `0`，同时拒绝带 slaves 的设备。该检查在每次跟踪操作、Current 和 Verify 时执行。程序不会自行更改 sysfs，也不会在本机安装驱动。管理员必须在独立测试机确认并配置该条件，避免挂载选项、fstrim 或其他管理程序重新启用 discard。重启后设置可能丢失。

## 跟踪与备份顺序

1. 新卷调用 `setup-snapshot DEVICE NEW_COW MINOR`，获得 sequence 1 的 `/dev/dattoMINOR` 只读数据视图；全量备份读取该快照。
2. 镜像持久化成功并再次 Verify 后，调用 `transition-to-incremental MINOR`。此阶段仅跟踪后续变更地址。
3. 下次调用 `transition-to-snapshot NEW_COW MINOR`。UUID 保持不变，sequence 必须增加 1，旧 COW 被关闭并成为稳定变更索引，新快照提供相应变更块的数据。
4. 旧 COW 的 UUID 必须与当前快照相同，旧 sequence 必须与已提交父备份相同，且恰好为当前 sequence 减 1。读取旧索引，只从新快照读取被标记的块。
5. 提交新镜像，Verify，再进入 incremental。重复使用 COW 文件名一律拒绝。

`Generation` JSON 字段为 `volume_id`、`uuid`、`cow_file`、`snapshot_device`、`sequence`、`minor`、`mode` 和 `size`。UUID 是 32 位小写十六进制，无连字符。mode 仅为 `snapshot` 或 `incremental`。`Transition` 包含 `current`、`previous_cow` 和 `previous_sequence`。

## 崩溃与代际保护

驱动命令前先将 journal 写成 pending，并同步文件和父目录；命令后重新读取 `/proc/datto-info`，核验结果后才保存新的已提交跟踪代际。journal 修改和读取使用独占目录锁，防止多个进程同时推进代际。进程崩溃留下的锁必须由管理员检查后处理。

仅当卷在 journal 中没有记录时才返回 `ErrNoGeneration`。只有这一错误能触发首次 Setup。pending、journal 损坏、版本变化、UUID/sequence/COW/大小或模式不一致、驱动 error 字段出现、状态为 dormant/unverified，均失败关闭，不自动新建跟踪、不自动销毁驱动、不静默全量回退。

journal 保留旧 COW 与旧 sequence，所以镜像提交前失败时可以使用同一快照和旧索引重试；镜像已提交、尚未开始驱动 finalization 的失败可由协调器辨认已提交代际并继续 finalization。如果驱动命令已经开始并留下 pending，程序保守拒绝自动恢复，必须人工检查并建立新基线。

journal 还记录 Linux boot ID。重启后拒绝沿用原链，即使相同 UUID 被重新载入：此版本不声称已经证明 initramfs 提前恢复跟踪，没有这项保证时可能存在漏写窗口。管理员不应在同一 boot 内手工卸载、重载或更改驱动；此类越过管理器的操作破坏连续性假设。

## COW 格式解析边界

格式依据上游公开的 [STRUCTURE.md](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/doc/STRUCTURE.md)、[头部格式声明](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/src/dattobd.h)及 [update-img 协议使用方式](https://github.com/datto/dattobd/blob/229ca998ce570ab1b74172d55dda62783d5b2d8d/utils/update-img.c)。解析规则独立实现，未移植其函数。

amd64 原生格式为 little-endian：4096 字节头部，magic 4776；sequence 位于字节 24–31，16 字节 UUID 位于 32–47，格式版本位于 48–55，变更计数位于 56–63。索引从字节 4096 开始，每个源 4096 字节块对应一个 64 位项，非零即发生变更。

只接受 clean 位和 index-only 位均设置的已关闭旧 COW，拒绝未知 flags、版本、错误 UUID/sequence、截断索引及版本 1 的变更计数不一致。支持源盘 512 字节对齐的尾块并裁剪输出范围。相邻变更块合并成 extent；读取缓存固定为 64 KiB，源盘上限 64 TiB，输出最多 1,048,576 个不连续 extent，超过限制会报错而非遗漏范围。

## 验证要求

单元测试使用临时普通文件和私有测试替身，验证命令参数、增量代际、旧索引重试、pending、boot 改变、越权卷、错误状态、COW 文件不复用、journal 锁和格式损坏，不加载驱动、不写本机块设备。

真实验证必须在可丢弃虚拟机内完成：固定 Ubuntu 24.04 GA 6.8 内核，独立 ext4 和 XFS 数据盘，禁用源队列 discard，安装固定上游提交，对多轮在线写入执行全量和真实 CBT 增量。每轮从相同快照另做完整 oracle 镜像；恢复所得镜像必须逐字节哈希一致，同时记录源快照实际读取字节，证明增量没有扫描全盘。恢复镜像仅在独立文件或测试盘上检查，不覆盖源盘。

GitHub [官方 runner 文档](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners)说明嵌套虚拟机没有稳定性保证。QEMU 测试应能使用 TCG 软件仿真，以免依赖 `/dev/kvm`；若 runner 提供 KVM，可作为可选加速。真实内核实验完成前，不能将单元测试成功描述为已验证内核级 CBT。
