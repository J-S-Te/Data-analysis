// Package auditoutbox 投递事务内写下的业务审计事件（批次 2）。
//
// 业务事务只负责把事件写入 bi_audit_outbox；真正投递由本包的后台循环完成。
// 平台不可用时事件留在本地，按退避重试；进程崩溃后租约到期可被重新领取。
package auditoutbox

import (
	"context"
	"log/slog"
	"time"

	platformaudit "github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

// Store 负责领取与回写；实现必须用租约保证同一事件只被一个 worker 投递。
type Store interface {
	Claim(ctx context.Context, workerID string, limit int, now time.Time) ([]Entry, error)
	MarkDelivered(ctx context.Context, id string, now time.Time) error
	MarkFailed(ctx context.Context, id string, code string, retryAt time.Time, now time.Time) error
}

// Dispatcher 依配置的周期投递，达到上限后放弃并保留错误码供告警。
type Dispatcher struct {
	Store       Store
	Reporter    platformaudit.Reporter
	WorkerID    string
	Poll        time.Duration
	BatchSize   int
	MaxAttempts uint
	MaxBackoff  time.Duration
	Logger      *slog.Logger
}

// Run 持续投递，直到 ctx 取消。
func (d *Dispatcher) Run(ctx context.Context) {
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		d.deliverOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverOnce 领取一批事件并逐条投递；单条失败不影响其余事件。
func (d *Dispatcher) deliverOnce(ctx context.Context) {
	if d.Store == nil || d.Reporter == nil {
		return
	}
	now := time.Now().UTC()
	entries, err := d.Store.Claim(ctx, d.WorkerID, d.batchSize(), now)
	if err != nil {
		d.logger().ErrorContext(ctx, "claim audit outbox events", "error", err)
		return
	}
	for _, entry := range entries {
		event := platformaudit.Event{
			ActorID: entry.ActorID, Action: entry.Action,
			ResourceType: entry.ResourceType, ResourceID: entry.ResourceID,
			RequestID: entry.RequestID, CorrelationID: entry.RequestID,
			Result: entry.Result, LoginIP: entry.LoginIP, ClientIP: entry.ClientIP, IPSource: entry.IPSource,
		}
		if reportErr := d.Reporter.Report(ctx, event); reportErr != nil {
			// 失败的尝试一律保留在 outbox：业务已经提交，审计不能丢。
			code := "PLATFORM_AUDIT_UNAVAILABLE"
			retryAt := now.Add(d.backoff(entry.Attempts + 1))
			if err := d.Store.MarkFailed(ctx, entry.ID, code, retryAt, now); err != nil {
				d.logger().ErrorContext(ctx, "mark audit outbox failure", "error", err, "event_id", entry.EventID)
			}
			continue
		}
		if err := d.Store.MarkDelivered(ctx, entry.ID, now); err != nil {
			d.logger().ErrorContext(ctx, "mark audit outbox delivered", "error", err, "event_id", entry.EventID)
		}
	}
}

func (d *Dispatcher) batchSize() int {
	if d.BatchSize <= 0 || d.BatchSize > 200 {
		return 50
	}
	return d.BatchSize
}

// backoff 指数退避，上限可配置；投递失败永远不会立即热循环。
func (d *Dispatcher) backoff(attempts uint) time.Duration {
	base := time.Second
	for i := uint(1); i < attempts && i < 8; i++ {
		base *= 2
	}
	if max := d.MaxBackoff; max > 0 && base > max {
		return max
	}
	return base
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
