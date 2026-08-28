// alert-worker：预警规则计算（聚合库上执行，幂等 upsert alert_item）。
// 对齐 customer_and_opportunity/cmd/*-worker；阈值参数读取平台 cfg 命名空间 data_analysis.alert_rules（设计方案 §3.6/§6）。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/alertworker"
)

func main() {
	once := flag.Bool("once", envBool("ALERT_WORKER_ONCE"), "run one alert evaluation pass and exit")
	interval := flag.Duration("interval", envDuration("ALERT_WORKER_INTERVAL", 5*time.Minute), "alert evaluation interval")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if envBool("ALERT_WORKER_DISABLED") {
		logger.Info("alert-worker disabled")
		return
	}
	dsn := os.Getenv("DASHBOARD_MYSQL_DSN")
	if dsn == "" {
		logger.Error("DASHBOARD_MYSQL_DSN is required")
		os.Exit(1)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		logger.Error("open dashboard database failed", "error", err)
		os.Exit(1)
	}
	tenantID := os.Getenv("OIDC_TENANT_ID")
	if tenantID == "" {
		logger.Error("OIDC_TENANT_ID is required for alert rule isolation")
		os.Exit(1)
	}
	worker := alertworker.New(alertworker.NewGormStore(db, tenantID))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		count, runErr := worker.RunOnce(ctx)
		if runErr != nil {
			logger.Error("alert evaluation failed", "error", runErr, "processed", count)
			os.Exit(1)
		}
		logger.Info("alert evaluation completed", "processed", count)
		return
	}
	if err := alertworker.RunLoop(ctx, *interval, worker, logger); err != nil {
		logger.Error("alert-worker stopped with error", "error", err)
		os.Exit(1)
	}
}

func envBool(key string) bool {
	value, _ := strconv.ParseBool(os.Getenv(key))
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
