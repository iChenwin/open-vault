# 第一次使用 Open Vault

适用于 Ubuntu 24.04、x86_64 / amd64。当前发布为**控制台开发预览**：可以启动页面、从局域网登录；CBT 增量恢复仍有已知一致性问题，不能作为正式备份使用。

## 1. 下载启动包

打开 [v0.2.0-alpha.3 下载页面](https://github.com/iChenwin/open-vault/releases/tag/v0.2.0-alpha.3)，下载 `open-vault-linux-amd64.tar.gz`。无需安装 Go、Nginx 或 Docker。

服务器没有浏览器时，可以逐行复制：

```sh
mkdir -p ~/open-vault-preview
cd ~/open-vault-preview
curl -fLO https://github.com/iChenwin/open-vault/releases/download/v0.2.0-alpha.3/open-vault-linux-amd64.tar.gz
curl -fLO https://github.com/iChenwin/open-vault/releases/download/v0.2.0-alpha.3/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf open-vault-linux-amd64.tar.gz
```

校验应显示启动包 `OK`。如果下载或校验失败，先解决错误，再继续。

## 2. 启动

在解压后的目录运行：

```sh
bash start.sh
```

不需要 sudo。程序自动创建配置和登录口令，默认目录为 `~/.config/open-vault`（设置了 `XDG_CONFIG_HOME` 时使用该目录）。再次启动会沿用配置和口令。启动窗口会显示类似：

```text
本机访问：http://127.0.0.1:8080
局域网访问：http://192.168.1.50:8080
登录口令：（这里会显示你的实际口令）
```

保留这个窗口。按 Ctrl+C 会停止服务；再次运行同一命令即可启动。需要开机自动运行时参见 [服务部署](DEPLOYMENT.md)。

## 3. 用电脑或手机打开

让设备连接同一局域网，在浏览器输入启动窗口中的地址，例如 `http://192.168.1.50:8080`。复制启动窗口的登录口令，粘贴到网页并进入控制台。

不需要反向代理。访问口令仍然必填；直接访问模式仅接受本机及私有网络来源。请在可信局域网使用 HTTP，跨网访问应配置 HTTPS 反向代理。

若显示多个地址，优先选择与电脑或手机处于同一网段的服务器地址；Docker、VPN、WSL 的虚拟网卡地址不一定能从其他设备访问。

## 4. 页面打开后

「未配置」表示控制台已经能访问，但备份数据卷服务还没准备好。快速启动不会安装内核模块、选择磁盘或自动进行备份。

研究备份功能时，再按 [数据卷服务配置](CBT-AGENT.md) 在可销毁的 Ubuntu 测试机准备驱动、仓库、加密密钥和 Agent。启动包包含三个程序，但只有 `open-vault` 是网页服务；另外两个由测试管理员配置。准备源卷前请先读 [真实验证结果](CBT-VALIDATION.md)。

## 打不开页面时

| 现象 | 处理方式 |
| --- | --- |
| 服务器上也打不开 | 确认启动窗口还在运行，尝试 `http://127.0.0.1:8080` |
| 提示端口占用 | 改用 `bash start.sh -port 8081`，浏览器也改为 8081 |
| 本机能开，其他电脑不能 | 确认同一局域网、IP 正确、防火墙允许该端口、路由器未启用客户端隔离 |
| 使用虚拟机或 WSL | 配置桥接网络或宿主端口映射；虚拟机自身 IP 不一定能被局域网访问 |
| 网页提示口令无效 | 使用当前启动窗口显示的完整口令；不要用仓库加密密钥代替 |
| 退出后再次启动 | 回到解压目录运行 `bash start.sh`；口令不会重新生成 |
| 忘记口令 | 重新运行启动命令，或仅在服务器本机读取配置目录的 `api-token`；不要发到公开 issue |

如果使用 UFW，先执行 `sudo ufw status` 查看状态。仅在需要时允许自己的网段。例如下面的 `192.168.1.0/24` **要替换为你自己的局域网网段**：

```sh
sudo ufw allow from 192.168.1.0/24 to any port 8080 proto tcp
```

## 已有源码时

机器已有 Go 1.23+，可直接构建并快速启动：

```sh
go build -o open-vault .
bash start.sh
```

也可以运行 `./open-vault -quickstart -lan`。仅本机体验则运行 `./open-vault -quickstart`；已有配置可使用 `./open-vault -config /绝对路径/config.json -lan`。`-lan`、`-port` 是本次启动选项，不会改写已有配置。
