// Package dbpool 为数析各进程提供独立、显式的数据库连接预算。
package dbpool

import (
	"database/sql"
	"fmt"
	"gorm.io/gorm"
	"os"
	"strconv"
	"time"
)

type Budget struct {
	MaxOpen, MaxIdle   int
	Lifetime, IdleTime time.Duration
}

// Load 读取指定进程预算并拒绝非法或无限连接配置。
// 参数：prefix 为进程环境变量前缀，getenv 为配置读取器。
// 返回值：有界连接预算；非法配置返回不含配置值的错误。
func Load(prefix string, getenv func(string) string) (Budget, error) {
	values := []int{8, 2, 300, 60}
	names := []string{"MAX_OPEN", "MAX_IDLE", "LIFETIME_SECONDS", "IDLE_SECONDS"}
	for i, name := range names {
		if raw := getenv(prefix + "_DB_" + name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > 3600 {
				return Budget{}, fmt.Errorf("invalid %s_DB_%s", prefix, name)
			}
			values[i] = value
		}
	}
	if values[0] < 1 || values[0] > 100 || values[1] > values[0] || values[2] < 1 || values[3] < 1 {
		return Budget{}, fmt.Errorf("invalid %s database connection budget", prefix)
	}
	return Budget{values[0], values[1], time.Duration(values[2]) * time.Second, time.Duration(values[3]) * time.Second}, nil
}

// Apply 将已验证预算安装到实际连接池。
// 参数：db 为数据库连接池，budget 为显式配置的上限与回收时间。
// 返回值：无。
func Apply(db *sql.DB, budget Budget) {
	db.SetMaxOpenConns(budget.MaxOpen)
	db.SetMaxIdleConns(budget.MaxIdle)
	db.SetConnMaxLifetime(budget.Lifetime)
	db.SetConnMaxIdleTime(budget.IdleTime)
}

// Configure 为指定进程配置环境预算。
// 参数：db 为已打开的GORM连接，prefix 为进程固定前缀。
// 返回值：配置或连接池访问失败返回错误。
func Configure(db *gorm.DB, prefix string) error {
	budget, err := Load(prefix, os.Getenv)
	if err != nil {
		return err
	}
	pool, err := db.DB()
	if err != nil {
		return err
	}
	Apply(pool, budget)
	return nil
}
