// Package auditoutbox 投递事务内写下的业务审计事件（批次 2）。
//
// 业务事务只负责把事件写入 bi_audit_outbox；真正投递由本包的后台循环完成。
// 平台不可用时事件留在本地，按退避重试；进程崩溃后租约到期可被重新领取。
package auditoutbox

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	platformaudit "github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

const (
	// purgeBatchSize 是单批清理行数上限，与 GORMStore 的 purgeBatchLimit 一致。
	purgeBatchSize = purgeBatchLimit
	// purgeBatches 是单轮清理的最大批次数，避免一次追平历史积压时长时间占用连接。
	purgeBatches = 20
	// purgeInterval 是清理间隔：删除是后台维护动作，不必跟随 2s 投递周期。
	purgeInterval = time.Hour
	// defaultDeliveredRetention 是已投递行的默认保留时长：0 表示关闭清理。
	// 清理删除的是仅存在于本地的审计证据（平台侧可能缺失或不可回查），保留期
	// 未经批准不得缩短，因此默认不动任何行，只有显式配置正整数天才启用。
	defaultDeliveredRetention time.Duration = 0
	// deliveredRetentionEnv 是保留时长的配置键（单位天，0/缺省关闭清理，正数启用）。
	deliveredRetentionEnv = "AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS"
)

// Store 负责领取与回写；实现必须用租约保证同一事件只被一个 worker 投递。
type Store interface {
	Claim(ctx context.Context, workerID string, limit int, now time.Time) ([]Entry, error)
	MarkDelivered(ctx context.Context, id string, now time.Time) error
	MarkFailed(ctx context.Context, id string, code string, retryAt time.Time, now time.Time) error
}

// Purger 清理已成功投递的历史行。实现必须按 batchSize 分批删除，
// 不允许无界 DELETE 把 outbox 表长时间锁住。
type Purger interface {
	PurgeDelivered(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)
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
	// 已投递行保留清理：默认关闭（0 天），只有显式配置
	// AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS 为正整数才启用。
	// 投递器未启动（审计发布器配置缺失）时本循环不存在，清理也随之不跑，由启动
	// WARN 与 /readyz 的 audit_outbox_dispatcher 字段暴露。
	retention := DeliveredRetentionFromEnv()
	if retention <= 0 {
		// 启动日志必须明确“清理未启用”：delivered 行是本地审计证据，默认永久保留。
		d.logger().InfoContext(ctx, "audit outbox delivered-row purge disabled (retention 0); delivered rows are kept indefinitely as local audit evidence",
			"table", "bi_audit_outbox", "purge", "disabled", "retention_days", 0, "env", deliveredRetentionEnv)
	} else {
		d.logger().InfoContext(ctx, "audit outbox delivered-row purge enabled",
			"table", "bi_audit_outbox", "purge", "enabled", "retention", retention.String(), "env", deliveredRetentionEnv)
	}
	var lastPurge time.Time
	for {
		d.deliverOnce(ctx)
		if now := time.Now(); purgeDue(retention, lastPurge, now) {
			lastPurge = now
			d.purgeDelivered(ctx, now.UTC().Add(-retention))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DeliveredRetentionFromEnv 读取 AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS（单位天）。
// 缺省、非法值、<=0 一律返回 0 = 关闭清理：删除的是仅本地审计证据，只有显式
// 配置正整数天才启用。该键不是必填键，缺失或写错都不得改变启动成败。
func DeliveredRetentionFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(deliveredRetentionEnv))
	if raw == "" {
		return defaultDeliveredRetention
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return defaultDeliveredRetention
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// purgeDue 报告本轮是否执行清理：保留期关闭（<=0）时永不清理，
// 且两次清理之间至少间隔 purgeInterval，避免跟随 2s 投递周期反复扫表。
func purgeDue(retention time.Duration, lastPurge, now time.Time) bool {
	if retention <= 0 {
		return false
	}
	return lastPurge.IsZero() || now.Sub(lastPurge) >= purgeInterval
}

// purgeDelivered 分批删除超过保留期的已投递行；批次间检查 ctx，失败只记日志，
// 不影响投递主流程（投递可用性优先于表维护）。
func (d *Dispatcher) purgeDelivered(ctx context.Context, cutoff time.Time) {
	purger, ok := d.Store.(Purger)
	if !ok {
		return
	}
	for batch := 0; batch < purgeBatches; batch++ {
		if ctx.Err() != nil {
			return
		}
		purged, err := purger.PurgeDelivered(ctx, cutoff, purgeBatchSize)
		if err != nil {
			if ctx.Err() == nil {
				d.logger().WarnContext(ctx, "purge delivered audit outbox rows", "cutoff", cutoff, "error", err)
			}
			return
		}
		if purged == 0 {
			return
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
		// 只转发平台审计协议已有的字段：platformaudit.Event 没有负载字段，
		// 因此迁移 000028 的 payload 保持本地证据（不新增平台侧转发字段，
		// 也不把本地负载拼进 action/resource_id 等既有列充数）。
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
