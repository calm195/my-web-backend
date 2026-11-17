// Package migrations 提供编译进运维工具的版本化 SQL，不允许 API 启动时自动迁移。
package migrations

import "embed"

// Files 包含已评审的 SQL；新增迁移必须使用递增编号，不能改写已应用文件。
//
//go:embed *.sql
var Files embed.FS
