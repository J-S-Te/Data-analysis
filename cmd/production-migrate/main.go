// Command production-migrate applies the embedded, checksum-protected schema
// history. The release orchestrator must complete its backup gate before this
// binary is started.
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
