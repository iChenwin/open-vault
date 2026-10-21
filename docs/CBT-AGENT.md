# 在控制台接入特权 CBT Agent

CBT 备份需要访问批准的数据块设备和源卷内的 COW 文件。`open-vault-cbt-agent` 是独立 root 服务；普通 Web 控制器继续以非 root 用户运行，通过同组 Unix socket 请求固定的备份、校验及恢复操作。控制台不取得通用 shell、任意磁盘路径选择或仓库密钥读取权限。

这是针对 Ubuntu 24.04 GA 6.8、普通 ext4 / XFS **数据卷**的实验能力。支持状态应以当前提交对应的真实内核实验结果为准，参见 [实验流程](CBT-LAB.md)。恢复输出为新的普通镜像文件，不执行裸机回写、分区修改或系统引导修复。先在一次性实验卷验证，再按实际数据卷范围配置。

## 身份和路径

| 对象 | 运行身份 / 权限 | 样例路径 |
| --- | --- | --- |
| Web 控制器 | 用户和组 `open-vault` | 原有 `open-vault.service` |
| 特权 Agent | `User=root`、`Group=open-vault` | `open-vault-cbt-agent.service` |
| Agent socket | `root:open-vault`、`0660` | `/run/open-vault-cbt/agent.sock` |
| socket 父目录 | `root:open-vault`、`0750` | `/run/open-vault-cbt` |
| Agent 私有配置 | `root:root`、`0600` | `/etc/open-vault/cbt-agent.json` |
| CLI 私有配置及密钥 | `root:root`、`0600` | `/etc/open-vault/cbt.json`、`cbt-key` |
| CLI 跟踪状态 | root 专用、`0700` | `/var/lib/open-vault-cbt` |
| Agent 服务状态 | root 专用、`0700`，与 CLI 状态独立 | `/var/lib/open-vault-cbt-service` |
| 加密仓库 | root 专用 | `/backup/open-vault-cbt` |
| 源卷及 COW 目录 | COW 目录 root 持有、`0700` | `/srv/business-data/.open-vault-cbt` |
| 恢复镜像目录 | root 专用 | `/restore/cbt-images` |

仅将可信的 Web 控制器身份加入 `open-vault` 组；同组成员能够发起管理员范围的 CBT 操作。源目录、COW 父路径、配置父目录及可执行文件不能由控制器或不可信用户替换。Agent 的私有配置不需要 Web 控制器读取。

## 准备配置和可执行文件

以下命令是供管理员在目标 Linux 实验主机使用的部署样例，不会由项目自动格式化任何磁盘。先完成 dattobd 的安装、内核匹配和模块加载。CLI 的 `diagnose` 会验证数据卷及驱动条件；对于启用 discard 的卷应停止并由管理员审核其挂载和队列配置，不能跳过检查。不要将实验脚本对一次性虚拟盘执行的格式化步骤用于业务卷。

```sh
go build -o open-vault-cbt ./cmd/open-vault-cbt
go build -o open-vault-cbt-agent ./cmd/open-vault-cbt-agent
sudo install -m 0755 -o root -g root open-vault-cbt /usr/local/bin/open-vault-cbt
sudo install -m 0755 -o root -g root open-vault-cbt-agent /usr/local/bin/open-vault-cbt-agent
```

确保已存在专用用户和组 `open-vault`。配置目录必须由 root 持有且不可由控制器写入，即使其中其他 Web 凭据文件仍由控制器用户持有：

```sh
sudo install -d -m 0750 -o root -g open-vault /etc/open-vault
sudo chown root:open-vault /etc/open-vault
sudo chmod 0750 /etc/open-vault
sudo install -d -m 0700 -o root -g root \
  /var/lib/open-vault-cbt /var/lib/open-vault-cbt-service \
  /backup/open-vault-cbt /restore/cbt-images
```

确认 `/srv/business-data` 已挂载为目标数据卷后，再创建 COW 目录，避免挂载缺失时误写底层目录：

```sh
mountpoint /srv/business-data && sudo install -d -m 0700 -o root -g root /srv/business-data/.open-vault-cbt
sudo install -m 0600 -o root -g root config.cbt.example.json /etc/open-vault/cbt.json
sudo install -m 0600 -o root -g root config.cbt-agent.example.json /etc/open-vault/cbt-agent.json
```

将 `/etc/open-vault/cbt.json` 中的设备 UUID 占位符替换为管理员核实的数据卷 UUID，并核对 `mount_point`、`cow_dir`、仓库及状态目录。`dbdctl` 使用实际安装的绝对路径，可通过 `command -v dbdctl` 核对。样例默认 `/usr/local/bin/dbdctl`。

首次初始化仓库时生成独立 CBT 密钥；已有仓库必须使用原密钥，不可覆盖：

```sh
sudo sh -c 'umask 077; set -C; openssl rand -hex 32 > /etc/open-vault/cbt-key'
sudo chmod 0600 /etc/open-vault/cbt-key
sudo /usr/local/bin/open-vault-cbt -config /etc/open-vault/cbt.json diagnose
```

密钥内容为 64 位十六进制文本，解码后是 32 字节。将其另存于独立安全位置；不把它写入控制台、Git、日志或支持工单。配置父目录、密钥和二进制文件应保持 root 持有。

## 安装 Agent 服务

先按批准的实际卷修改服务文件的 `RequiresMountsFor` 和 `ReadWritePaths`。以下路径与样例 CLI 配置一致：源 COW `/srv/business-data/.open-vault-cbt`、仓库 `/backup/open-vault-cbt`、CLI 状态 `/var/lib/open-vault-cbt`、恢复目录 `/restore/cbt-images`。Agent 自身状态由 `StateDirectory=open-vault-cbt-service` 创建，socket 父目录由 `RuntimeDirectory=open-vault-cbt` 创建。

```sh
sudo install -m 0644 -o root -g root deploy/open-vault-cbt-agent.service /etc/systemd/system/open-vault-cbt-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now open-vault-cbt-agent
sudo systemctl status open-vault-cbt-agent
sudo journalctl -u open-vault-cbt-agent -n 100 --no-pager
sudo stat -c '%U:%G %a %n' /run/open-vault-cbt /run/open-vault-cbt/agent.sock
```

socket 的预期结果分别是 `root:open-vault 750` 和 `root:open-vault 660`。不应通过设置为 `0777` 来解决访问问题。确认 Web 控制器实际进程属于 `open-vault` 组；组成员变更需要重启相关服务才会生效。

样例使用 `ProtectSystem=full`，保护系统目录。`ReadWritePaths` 在该模式下不是系统范围的完整写入白名单，也不替代 Agent 的固定操作及路径检查。不要直接改成 `ProtectSystem=strict` 而遗漏批准卷的 COW 写入路径；也不要启用 `PrivateDevices=true` 隐藏所需数据块设备。需要进一步约束时应针对实际卷测试所有挂载命名空间及内核驱动行为。

## 将控制台连接到 socket

在普通 Web 控制器的现有配置中增加以下字段，保留其他文件备份配置：

```json
{
  "cbt_socket": "/run/open-vault-cbt/agent.sock"
}
```

这是配置片段，不是替代整个 Web 配置的文件。路径必须与 Agent 配置的 `socket_path` 相同。重启 Web 控制器后，输入原有 API 令牌，通过 CBT 控制台查看状态并发起操作：

```sh
sudo systemctl restart open-vault
```

Web 控制器无需 root、sudo 权限、仓库密钥、源块设备访问权限或 Agent 私有配置读取权限。它只通过 socket 调用预设数据卷的 `backup` / `verify` / `restore`。恢复目标受 Agent 的 `restore_root` 约束，只创建新的普通镜像；后续只读挂载、业务切换或人工回写是管理员另行操作的流程。

Web 文件备份服务样例的 `ProtectSystem=strict` 仅适用于原有非 root 文件备份控制器。为控制器增加 Unix socket 客户端访问不需要授予其源块设备读写权限。

## 验证与排查

完成第一次全量备份后，在实验数据上制造少量修改并创建下一代，查看 generation 的父代、序列和源读取量。恢复两代至不同的新镜像，按 [真实内核实验](CBT-LAB.md) 的流程比对文件系统及文件内容。备份 API 返回任务启动不等于备份完成，应在执行结果中确认最终状态。

无法连接时依次检查：Agent 是否运行、两端配置是否为同一个绝对 socket 路径、socket 和父目录组权限、控制器实际组成员。Agent 作业失败时查看服务日志、真实驱动状态和 CLI `diagnose` 结果。长任务同时受到 Agent 的超时和仓库互斥控制，不应在运行过程中手工删除锁、清空 COW 目录或卸载模块。

私有配置修改后重启 Agent；控制台 socket 配置修改后重启 Web 控制器。备份密钥、仓库、跟踪状态和 Agent 服务状态具有不同用途，不能通过复制或删除其中一个目录来重置整个链。发生跟踪缺口或驱动代际不匹配时，应保留现场、检查现有可恢复备份，并按新基线流程处理。
