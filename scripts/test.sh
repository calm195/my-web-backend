#!/bin/sh
set -eu
. "$(dirname -- "$0")/common.sh"
# 数据库用例使用各自的 t.TempDir；race 检查覆盖认证与存储并发边界。
# race 由 Go 官方工具链按当前平台执行，不沿用发布构建的 CGO_ENABLED=0。
CGO_ENABLED=1 go test -race ./...
