#!/bin/sh
set -eu
. "$(dirname -- "$0")/common.sh"
mkdir -p bin
# API 与维护 CLI 独立生成；SQL 迁移编译时嵌入，不读取任何前端产物。
for backend_target in api site-admin; do
  CGO_ENABLED=0 go build -trimpath -o "bin/$backend_target" "./cmd/$backend_target"
done
