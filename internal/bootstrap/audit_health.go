package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

func reportAuditOutboxHealth(ctx context.Context, store *auditoutbox.GORMStore, reporter platformaudit.Reporter, logger *slog.Logger) {
	if store == nil || reporter == nil {
		return
	}
	health, ok := reporter.(platformaudit.OutboxHealthReporter)
	if !ok {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		counts, err := store.Counts(ctx)
		if err == nil {
			reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if reportErr := health.ReportOutboxHealth(reportCtx, counts.Pending, counts.Dead, time.Time{}); reportErr != nil {
				logger.Warn("report audit outbox health failed", "error", reportErr)
			}
			cancel()
		} else {
			logger.Warn("read audit outbox health failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
