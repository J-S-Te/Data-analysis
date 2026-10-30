package aggregation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Pass 组合一次聚合任务。未配置的同步通道保持为 nil，不会产生误报警。
type Pass struct {
	LicenseCheck          func(context.Context) error
	SyncSources           func(context.Context) error
	SyncImportReport      func(context.Context) error
	SyncJobs              func(context.Context) error
	SyncContractDashboard func(context.Context) error
	SyncProjectDashboard  func(context.Context) error
}

// Configured 判定当前聚合链路是否含可执行同步入口。
// 参数：无。
// 返回值：至少一个来源或历史摘要通道已配置时为true。
func (p Pass) Configured() bool {
	return p.SyncImportReport != nil || p.SyncSources != nil || p.SyncContractDashboard != nil || p.SyncProjectDashboard != nil
}

// RunOnce 串行执行已配置通道，并汇总各通道错误。
// 参数：ctx 为许可和取消上下文。
// 返回值：全部成功为nil；单源失败不会阻断其余已配置通道。
func (p Pass) RunOnce(ctx context.Context) error {
	tasks := []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "database sources", run: p.SyncSources},
		{name: "contract import report", run: p.SyncImportReport},
		{name: "queued sync jobs", run: p.SyncJobs},
		{name: "contract dashboard API", run: p.SyncContractDashboard},
		{name: "project dashboard API", run: p.SyncProjectDashboard},
	}
	var runErrors []error
	for _, task := range tasks {
		if task.run == nil {
			continue
		}
		if p.LicenseCheck != nil {
			if err := p.LicenseCheck(ctx); err != nil {
				return err
			}
		}
		if err := task.run(ctx); err != nil {
			runErrors = append(runErrors, fmt.Errorf("sync %s: %w", task.name, err))
		}
	}
	return errors.Join(runErrors...)
}

// RunLoop 立即执行一轮，之后按固定间隔串行执行，直至收到关闭信号。
func RunLoop(ctx context.Context, interval time.Duration, pass Pass, logger *slog.Logger) error {
	if interval <= 0 {
		return errors.New("aggregation interval must be positive")
	}
	runOnce := func() {
		startedAt := time.Now()
		if err := pass.RunOnce(ctx); err != nil {
			logger.Error("aggregation pass failed", "error", err, "elapsed", time.Since(startedAt))
			return
		}
		logger.Info("aggregation pass completed", "elapsed", time.Since(startedAt))
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
