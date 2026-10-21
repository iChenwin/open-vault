# Linux CBT 控制台部署

目标为 Ubuntu 24.04 amd64。先按 [CBT-AGENT.md](CBT-AGENT.md) 配置数据卷、独立仓库、密钥及 root Agent。实际内核支持状态见 [验证记录](CBT-VALIDATION.md)。

## 非 root Web 服务

在已有 Go 1.23+ 的主机上构建：

```sh
go build -o open-vault .
sudo install -m 0755 -o root -g root open-vault /usr/local/bin/open-vault
```

创建专用系统用户/组 `open-vault`（已存在则保留）。配置目录由 root 持有，Web 用户只需读取自己的 API 令牌，并可遍历 Agent socket 目录：

```sh
sudo install -d -m 0750 -o root -g open-vault /etc/open-vault
sudo install -m 0640 -o root -g open-vault config.example.json /etc/open-vault/config.json
sudo sh -c 'umask 077; set -C; openssl rand -hex 32 > /etc/open-vault/api-token'
sudo chown open-vault:open-vault /etc/open-vault/api-token
sudo chmod 0600 /etc/open-vault/api-token
sudo install -m 0644 deploy/open-vault.service /etc/systemd/system/open-vault.service
sudo systemctl daemon-reload
sudo systemctl enable --now open-vault
curl --fail http://127.0.0.1:8080/healthz
```

已有令牌必须保留，生成命令使用禁止覆盖模式。Web 配置只有三个字段：

| 字段 | 含义 |
| --- | --- |
| `listen` | 本机监听地址，默认 `127.0.0.1:8080` |
| `token_file` | 至少 32 字符的私有 API 令牌文件 |
| `cbt_socket` | 固定 Agent Unix socket 绝对路径 |

Web 不需要访问仓库、源盘或恢复目录。后台超时、状态及恢复目录由 Agent 配置控制。更改配置或令牌后重启相应服务。

## 局域网 IP:8080

将 Web 的 `listen` 改为 `127.0.0.1:8081` 并重启，使用 [Nginx 配置样例](../deploy/nginx-lan.conf) 在 8080 提供前端。把防火墙来源限定为自己的局域网；使用 HTTPS 时在反向代理配置证书。浏览器打开 `http://服务器IP:8080`，输入原 API 令牌。

也可保留默认监听，通过 SSH 隧道访问：

```sh
ssh -L 8080:127.0.0.1:8080 user@backup-host
```

## Docker

Dockerfile 只构建并运行非 root Web 控制器，不安装备份引擎或内核驱动，也不运行 root Agent。优先使用 Linux 主机的 systemd 部署；容器方式需要管理员另外配置宿主 socket 挂载和组身份。容器用户必须具备 socket 对应的数值 GID，且令牌可由该 UID 读取。不要使用 `--privileged` 启动 Web。

```sh
docker build -t open-vault:local .
```

此处没有声称已验证 Docker 到宿主 Agent 的完整部署。

## 首次演练

登录控制台后运行首次全量，确认作业最终成功；修改实验数据后再备份，检查父代和实际源读取量。选择历史版本恢复至新镜像，独立比对镜像和文件内容。任务已受理、健康探针或哈希字段的存在都不能替代恢复验证。
