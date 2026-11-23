#!/bin/sh
# 由入口脚本 source；仅固定当前命令的工具链，不修改全局环境。
set -eu
backend_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$backend_root"
export GOTOOLCHAIN=local
if ! command -v go >/dev/null 2>&1; then
  echo '未找到 Go，请将 Go 1.27.4 的 bin 目录加入 PATH。' >&2
  exit 1
fi
backend_go_version=$(go env GOVERSION)
if [ "$backend_go_version" != 'go1.27.4' ]; then
  echo "Go 版本不符合锁定要求：需要 go1.27.4，实际为 ${backend_go_version}。" >&2
  exit 1
fi
