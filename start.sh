#!/usr/bin/env bash
# Run the bundled console as the current user; never install/load a driver.
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
if [[ ! -x ./open-vault ]]; then
  echo '未找到 open-vault 程序。请先解压完整的 Linux 启动包。' >&2
  echo '源码用户请先运行：go build -o open-vault .' >&2
  exit 1
fi
exec ./open-vault -quickstart -lan "$@"
