package alertworker

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RunLoop 立即执行一轮，之后按固定间隔串行执行，避免同一 Worker 重叠计算。
func RunLoop(ctx context.Context, interval time.Duration, worker *Worker, logger *slog.Logger) error {
	if interval <= 0 {
		return errors.New("alert worker interval must be positive")
	}
	runOnce := func() {
		count, err := worker.RunOnce(ctx)
		if err != nil {
			logger.Error("alert evaluation failed", "error", err, "processed", count)
			return
		}
		logger.Info("alert evaluation completed", "processed", count)
	}

	runOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runOnce()
		}
	}
}
