// Command production-migrate 应用嵌入且受校验和保护的数据库
// 历史。发布编排器必须在此二进制启动前完成
// 备份门禁。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	applied, err := migration.Run(ctx, os.Getenv("DASHBOARD_MYSQL_DSN"), migrations.Files)
	if err != nil {
		logger.Error("data_analysis production migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("data_analysis production migrations are current", "newly_applied", len(applied))
}
