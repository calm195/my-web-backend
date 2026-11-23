#!/bin/sh
set -eu
. "$(dirname -- "$0")/common.sh"
# 使用同一 Go 发行物的 gofmt；发现格式问题只报告，不悄悄修改源码。
backend_goroot=$(go env GOROOT)
backend_unformatted=$("$backend_goroot/bin/gofmt" -l cmd internal migrations)
if [ -n "$backend_unformatted" ]; then
  echo "Go 格式不符合约定：
$backend_unformatted" >&2
  exit 1
fi
go vet ./...
