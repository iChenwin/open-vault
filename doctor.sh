#!/usr/bin/env bash
# Read-only diagnosis; never reads credentials or changes disks/services.
set -uo pipefail
echo 'Open Vault 环境诊断（只读，不显示登录口令或仓库密钥）'
echo '体系结构与内核：'
uname -m -r
echo '发行版：'
if [[ -r /etc/os-release ]]; then
  awk -F= '$1 == "PRETTY_NAME" || $1 == "ID" || $1 == "VERSION_ID" { print }' /etc/os-release
else
  echo '未找到 /etc/os-release'
fi
echo '磁盘与挂载点：'
if command -v lsblk >/dev/null; then
  # MOUNTPOINT is compatible with older util-linux, including Ubuntu 20.04.
  lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT || true
else
  echo '未找到 lsblk'
fi
echo '后台服务：'
if command -v systemctl >/dev/null; then
  systemctl is-active open-vault-cbt-agent 2>/dev/null || true
else
  echo '此环境未提供 systemctl'
fi
if [[ -S /run/open-vault-cbt/agent.sock ]]; then
  echo 'Agent 默认 socket 存在；这不等于已通过备份恢复验收。'
else
  echo 'Agent 默认 socket 不存在；start.sh 只启动网页，尚未部署或启动后台。'
fi
echo '适配目标：Ubuntu 24.04、x86_64/amd64、GA 6.8 generic、普通 ext4/XFS 数据卷。'
echo '当前 CBT 增量恢复仍有已知一致性问题。即使环境匹配，也不代表备份已可可靠使用。'
