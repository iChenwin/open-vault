# Linux 部署指南

以下示例适用于 Debian / Ubuntu 风格的 Linux 主机。服务使用非 root 用户，只能备份该用户可以读取的文件。部署路径必须与配置和服务文件一致；使用其他路径时同时更新相关限制。不要把仓库、状态目录或恢复目录放在备份源目录中。

## 二进制与 systemd

安装 Go 1.23+、restic、OpenSSL，然后构建并安装：

```sh
go build -o open-vault .
sudo install -m 0755 open-vault /usr/local/bin/open-vault
sudo useradd --system --home-dir /var/lib/open-vault --shell /usr/sbin/nologin open-vault
sudo install -d -m 0750 -o open-vault -g open-vault /etc/open-vault /var/lib/open-vault /backup/repository /restore
sudo install -d -m 0755 /data
sudo install -m 0640 -o root -g open-vault config.example.json /etc/open-vault/config.json
sudo -u open-vault sh -c 'umask 077; openssl rand -hex 32 > /etc/open-vault/api-token; openssl rand -hex 32 > /etc/open-vault/restic-password'
```

若用户已经存在，跳过 `useradd`。给 `open-vault` 用户授予备份源的读取及目录遍历权限；不要为了读取文件而将所有源数据改属该用户。密钥文件在 Linux 上必须为 `0600`，由运行服务的用户拥有。将 API 令牌复制到控制台登录输入框；不要把令牌写到 URL 或公开日志中。独立备份仓库密码文件。

```sh
sudo install -m 0644 deploy/open-vault.service /etc/systemd/system/open-vault.service
sudo systemctl daemon-reload
sudo systemctl enable --now open-vault
sudo systemctl status open-vault
sudo journalctl -u open-vault -n 100 --no-pager
curl --fail http://127.0.0.1:8080/healthz
```

示例服务对 `/data` 只读，对仓库和恢复目录可写。`ProtectHome=true` 阻止读取用户主目录；若需要备份主目录，应根据实际范围重新配置 systemd 限制并审核权限。NAS 仓库须先完成挂载并确认读写权限；服务文件中的 `RequiresMountsFor` 用来声明挂载依赖。避免挂载失败后误将备份写入底层本地目录。

## 配置

| 字段 | 含义 |
| --- | --- |
| `listen` | 监听地址，默认示例为 `127.0.0.1:8080` |
| `state_dir` | 任务与执行状态存放目录 |
| `token_file` | API 令牌文件，至少 32 个字符 |
| `repository` | restic 仓库的本地绝对路径，可指向已挂载 NAS |
| `password_file` | restic 仓库密码文件 |
| `restore_root` | 允许写入恢复结果的根目录 |
| `source_roots` | 允许选择的源目录绝对路径数组 |
| `restic_binary` | restic 可执行文件名称或绝对路径 |
| `operation_timeout_minutes` | 单次引擎操作超时分钟数 |

修改配置后重启服务。源目录、状态目录、仓库和恢复目录应相互独立。仓库密码必须与初始化仓库时使用的密码相同；替换密码文件不等于修改仓库密钥。轮换 API 令牌时更换令牌文件并重启服务，重新登录。

## Docker

镜像使用 Go 1.25 构建，并从 Debian Bookworm 安装 restic。基础镜像与 apt 包未锁定 digest / 精确补丁版本，重建可能得到不同版本；生产发布应固定镜像摘要并记录 `restic version`。

```sh
docker build -t open-vault:local .
```

镜像使用 UID/GID `10001:10001`。在宿主机准备 `/srv/open-vault/config`、`state`、`repository` 和 `restore`，将配置复制为 `/srv/open-vault/config/config.json`。配置中的 `listen` 保持 `127.0.0.1:8080`，其他路径也沿用示例，使用容器内路径。密钥由 UID 10001 拥有且权限为 0600，数据目录允许该 UID 写入，源目录允许读取。将示例中的宿主机 `/data` 换为实际源路径。服务只允许回环监听，以下命令使用 Linux 宿主机网络；不适用于 Docker Desktop 的默认网络配置。

```sh
docker run -d --name open-vault --restart unless-stopped \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,size=128m \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --network host \
  -v /srv/open-vault/config:/etc/open-vault:ro \
  -v /srv/open-vault/state:/var/lib/open-vault \
  -v /srv/open-vault/repository:/backup/repository \
  -v /srv/open-vault/restore:/restore \
  -v /data:/data:ro \
  open-vault:local
```

容器恢复结果位于宿主机 `/srv/open-vault/restore`。非 root 运行时恢复文件的属主、组和特殊属性可能无法完整还原，需要在恢复演练中验证业务所需属性。容器内存、空间和超时应根据备份规模调整。

## 运行与恢复

- `/healthz` 是公开活性探针；它不证明仓库可用或备份已完成。`GET /api/status` 需要 Bearer 认证，提供版本和忙碌状态。
- 一次只能运行一个仓库操作。备份、恢复、完整性检查和初始化可能长时间占用执行槽，应预留维护窗口。
- 默认不会自动删除快照，因此仓库持续增长。首版未提供保留策略，需关注空间容量。
- 首次使用先初始化空仓库。连接已有仓库时使用其原密码，不要清空仓库目录。
- 定期从旧快照恢复至新目录并校验文件内容。恢复目录由控制器约束，禁止直接覆盖业务源文件。
- 服务中断后先确认 restic 进程和仓库状态，再决定是否需要人工解锁。不要在活跃备份期间删除锁。

控制器状态目录不等于备份仓库。灾难恢复需要保留：仓库、密码、配置及必要的状态目录。仓库密码应另存于独立安全位置；安全复制状态文件最好先停止服务。已有 restic 仓库也可以使用对应版本的 restic 命令行恢复。

远程访问应使用 SSH 隧道，或部署 HTTPS 反向代理并限制访问来源。不要直接将 HTTP 管理端口暴露到公网。持有 API 令牌的人可以在允许的范围内读取备份、发起恢复并消耗磁盘空间。
