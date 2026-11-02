package aggregation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
)

// RunOverlapGuard 控制“同租户/子系统存在未结束且开始更早的轮次”时的 precompute 处理方式。
// 两种取值都会跳过 precompute（避免混代次写派生事实），区别只在是否把重叠计为当轮错误。
type RunOverlapGuard string

const (
	// RunOverlapGuardWarn：跳过 precompute + WARN，当轮不因此失败（默认）。
	RunOverlapGuardWarn RunOverlapGuard = "warn"
	// RunOverlapGuardEnforce：同样跳过 precompute，但把重叠计为当轮错误；
	// 错误在全部来源执行完之后才汇总，不阻断其它来源。
	RunOverlapGuardEnforce RunOverlapGuard = "enforce"

	// AggregationRunSubsystem 是定时全量同步轮次在 sync_run 中的子系统编码。
	AggregationRunSubsystem = "aggregation"
	// syncRunStaleAfter：未结束且开始时间早于该阈值的轮次视为崩溃残留，不再阻断
	// precompute（否则一次异常退出会永久冻结派生事实）。取值大于队列单任务超时
	// （默认 30m）与常规轮次耗时，避免把正常长轮次当残留。
	syncRunStaleAfter = 2 * time.Hour
	// syncRunOwnerMaxLen 与 sync_run.owner 的列宽一致，超出时截断而不是让写入失败。
	syncRunOwnerMaxLen = 128
)

// ParseRunOverlapGuard 解析 AGG_RUN_OVERLAP_GUARD。
// 参数：raw 为环境变量原值，空值取默认 warn。
// 返回值：归一化策略；非法值返回错误，由启动装配失败关闭（不静默回退）。
func ParseRunOverlapGuard(raw string) (RunOverlapGuard, error) {
	switch normalized := RunOverlapGuard(strings.ToLower(strings.TrimSpace(raw))); normalized {
	case "":
		return RunOverlapGuardWarn, nil
	case RunOverlapGuardWarn, RunOverlapGuardEnforce:
		return normalized, nil
	default:
		return "", errors.New("AGG_RUN_OVERLAP_GUARD must be warn or enforce")
	}
}

// overlapGuard 返回生效的重叠守卫策略；零值/未知值按 warn（默认，不阻断）。
func (r *Runner) overlapGuard() RunOverlapGuard {
	if r.RunOverlapGuard == RunOverlapGuardEnforce {
		return RunOverlapGuardEnforce
	}
	return RunOverlapGuardWarn
}

// SyncRun 是一轮聚合任务的起止记录（sync_run 表）。
// finished_at 为空表示该轮仍在进行，或进程异常退出后残留。
type SyncRun struct {
	TenantID      string     `gorm:"column:tenant_id;primaryKey"`
	SubsystemCode string     `gorm:"column:subsystem_code;primaryKey"`
	RunStartedAt  time.Time  `gorm:"column:run_started_at;primaryKey"`
	Owner         string     `gorm:"column:owner"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
}

// TableName 返回 sync_run 表名。
func (SyncRun) TableName() string { return "sync_run" }

// syncRunStart 把轮次开始时间对齐到 DATETIME(3) 精度，保证起止两条语句命中同一主键。
func syncRunStart(at time.Time) time.Time {
	return at.UTC().Truncate(time.Millisecond)
}

// beginSyncRun 记录一轮聚合任务的开始。
// 参数：ctx 为取消上下文，startedAt 为轮次开始时间。
// 返回值：记录成功为 true；记录失败（如表未迁移）只 WARN 并返回 false，
// 本轮照常同步，只是重叠守卫不可用——审计表不可用不得冻结事实同步。
func (r *Runner) beginSyncRun(ctx context.Context, startedAt time.Time) bool {
	row := SyncRun{
		TenantID: r.tenantID, SubsystemCode: AggregationRunSubsystem,
		RunStartedAt: syncRunStart(startedAt), Owner: syncRunOwner(),
	}
	if err := r.aggregationWrite(ctx, func(tx *gorm.DB) error { return tx.Create(&row).Error }); err != nil {
		slog.Warn("sync run record unavailable; overlap guard disabled for this round", "error", err)
		return false
	}
	return true
}

// finishSyncRun 记录一轮聚合任务的结束时间。
// 参数：ctx 为取消后的上下文（调用方用 context.WithoutCancel 保证收尾写入），
// startedAt 必须与 beginSyncRun 传入值一致。
// 返回值：无；收尾失败只记录 WARN（下一轮的陈旧阈值会兜底）。
func (r *Runner) finishSyncRun(ctx context.Context, startedAt time.Time) {
	finishedAt := time.Now().UTC().Truncate(time.Millisecond)
	err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return tx.Model(&SyncRun{}).
			Where("tenant_id = ? AND subsystem_code = ? AND run_started_at = ?", r.tenantID, AggregationRunSubsystem, syncRunStart(startedAt)).
			Update("finished_at", finishedAt).Error
	})
	if err != nil {
		slog.Warn("sync run finish record failed", "error", err)
	}
}

// overlappingSyncRuns 统计同租户/子系统下“未结束、开始更早且未超过陈旧阈值”的其它轮次。
// 参数：ctx 为取消上下文，startedAt 为本轮开始时间（不含本轮自身）。
// 返回值：重叠轮次数与查询错误。
func (r *Runner) overlappingSyncRuns(ctx context.Context, startedAt time.Time) (int64, error) {
	start := syncRunStart(startedAt)
	var count int64
	err := r.aggDB.WithContext(ctx).Model(&SyncRun{}).
		Where("tenant_id = ? AND subsystem_code = ?", r.tenantID, AggregationRunSubsystem).
		Where("finished_at IS NULL AND run_started_at < ? AND run_started_at >= ?", start, start.Add(-syncRunStaleAfter)).
		Count(&count).Error
	return count, err
}

// precomputeOverlapGuard 判定本轮是否应跳过 precompute。
// 参数：ctx 为取消上下文，runStartedAt 为本轮开始时间，recorded 表示本轮是否成功写入 sync_run。
// 返回值：skip 为 true 时调用方必须跳过 precompute；err 只在 enforce 模式下非 nil
// （warn 模式只 WARN，不把重叠计为当轮失败）。检测/记录不可用时放行 precompute。
func (r *Runner) precomputeOverlapGuard(ctx context.Context, runStartedAt time.Time, recorded bool) (bool, error) {
	if !recorded {
		return false, nil
	}
	overlaps, err := r.overlappingSyncRuns(ctx, runStartedAt)
	if err != nil {
		slog.Warn("sync run overlap detection failed; precompute continues", "error", err)
		return false, nil
	}
	if overlaps == 0 {
		return false, nil
	}
	guard := r.overlapGuard()
	slog.Warn("precompute skipped: overlapping aggregation run detected",
		"overlapping_runs", overlaps, "guard", string(guard))
	if guard == RunOverlapGuardEnforce {
		return true, fmt.Errorf("precompute skipped: %d earlier %s run(s) still unfinished", overlaps, AggregationRunSubsystem)
	}
	return true, nil
}

// syncRunOwner 返回实例标识（hostname:pid），用于排障时区分轮次来源。
// 只包含主机名与进程号，不含 DSN、凭据或租户信息。
func syncRunOwner() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	owner := fmt.Sprintf("%s:%d", host, os.Getpid())
	if len(owner) > syncRunOwnerMaxLen {
		owner = owner[:syncRunOwnerMaxLen]
	}
	return owner
}
