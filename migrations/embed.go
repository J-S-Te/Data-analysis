// Package migrations 提供数据分析系统不可变的架构迁移文件。
//
// 将 SQL 文件嵌入迁移二进制，确保代码和架构计划作为同一个版本化制品发布。
package migrations

import "embed"

// Files contains every numbered SQL migration in this directory.
//
//go:embed *.sql
var Files embed.FS
