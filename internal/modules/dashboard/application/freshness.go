package application

import (
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// FreshnessMaxSyncAge 是成功同步或持久快照写入时间的展示容差。
// 48 小时允许错过一个 T+1 日批后再标记陈旧，不代表产品确认的同步 SLA；
// 超过该时长不隐藏旧数据，但必须显式提示可能已过期。
const FreshnessMaxSyncAge = 48 * time.Hour

// FreshnessInput 是从聚合库读取的原始新鲜度信号。
// SourceUpdatedAt 是查询范围内源业务行的最新 updated_at；LastSyncAt 是
// sync_source.last_run_at（任何状态）；SyncStatus 是同一行的 last_status。
// LastSuccessAt 可选，提供时独立于最新 attempt；省略时保留旧调用者行为。
type FreshnessInput struct {
	SourceUpdatedAt *time.Time
	LastSyncAt      *time.Time
	// LastSuccessAt independently preserves a published success across later attempts.
	LastSuccessAt   *time.Time
	SyncStatus      string
	SyncSourceKnown bool
	SyncEnabled     bool
	// PersistedSnapshot 表示全租户分支读取的是接口持久快照，而不是维表镜像。
	PersistedSnapshot bool
	// PersistedSnapshotAt 是持久快照的写入时间原文；透传原值并单独解析其年龄。
	PersistedSnapshotAt string
}

// BuildFreshness 生成响应时间口径。查询时间写入 ComputedAt，绝不用于回填
// SourceUpdatedAt 或 SyncedAt；无法证明最新时返回 Stale=true 与原因。
func BuildFreshness(input FreshnessInput, computedAt time.Time) domain.DataFreshness {
	freshness := domain.DataFreshness{
		SourceUpdatedAt: FormatDashboardTimestamp(input.SourceUpdatedAt),
		ComputedAt:      FormatDashboardTimestamp(&computedAt),
	}
	if input.PersistedSnapshot {
		freshness.SyncStatus = domain.SyncStatusSnapshot
		freshness.SyncedAt = strings.TrimSpace(input.PersistedSnapshotAt)
		snapshotAt := ParseDashboardTimestamp(freshness.SyncedAt)
		switch {
		case snapshotAt == nil:
			freshness.Stale = true
			freshness.StaleReason = "持久快照缺少有效时间水位，无法确认数据是否为最新。"
		case snapshotAt.After(computedAt):
			freshness.Stale = true
			freshness.StaleReason = "持久快照时间晚于本次查询时间，无法确认数据是否为最新。"
		case computedAt.Sub(*snapshotAt) > FreshnessMaxSyncAge:
			freshness.Stale = true
			freshness.StaleReason = "持久快照已超过 48 小时，当前展示的是旧数据。"
		}
		return freshness
	}
	if !input.SyncSourceKnown || strings.TrimSpace(input.SyncStatus) == "" {
		freshness.SyncStatus = domain.SyncStatusUnknown
		freshness.Stale = true
		freshness.StaleReason = "尚无源同步记录，无法确认数据是否为最新。"
		return freshness
	}
	status := strings.ToUpper(strings.TrimSpace(input.SyncStatus))
	freshness.SyncStatus = status
	successAt := input.LastSuccessAt
	if successAt == nil && status == domain.SyncStatusSuccess {
		successAt = input.LastSyncAt
	}
	if successAt != nil {
		freshness.SyncedAt = FormatDashboardTimestamp(successAt)
	}
	if !input.SyncEnabled {
		freshness.Stale = true
		freshness.StaleReason = "数据源同步已停用，当前展示的是停用前的旧数据。"
		return freshness
	}
	switch status {
	case domain.SyncStatusSuccess:
		if successAt == nil {
			freshness.Stale = true
			freshness.StaleReason = "同步记录缺少时间水位，无法确认数据是否为最新。"
			return freshness
		}
		if computedAt.Sub(*successAt) > FreshnessMaxSyncAge {
			freshness.Stale = true
			freshness.StaleReason = "最近一次源同步已超过 48 小时，当前展示的是旧数据。"
		}
	case domain.SyncStatusFailed:
		freshness.Stale = true
		freshness.StaleReason = "最近一次源同步失败，当前展示的是最近一次成功同步的旧数据。"
	case domain.SyncStatusRunning:
		freshness.Stale = true
		freshness.StaleReason = "源同步进行中，当前展示的是上一次成功同步的数据。"
	case domain.SyncStatusPending:
		freshness.Stale = true
		freshness.StaleReason = "数据源尚未完成首次同步，当前没有可确认的新数据。"
	default:
		freshness.SyncStatus = domain.SyncStatusUnknown
		freshness.Stale = true
		freshness.StaleReason = "源同步状态未知，无法确认数据是否为最新。"
	}
	return freshness
}

// FormatDashboardTimestamp 把时间格式化为带 Z 的 RFC3339；nil 返回空串，
// 让 API 明确表示“来源未提供”，而不是补当前时间。
func FormatDashboardTimestamp(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// ParseDashboardTimestamp 解析数据库读出的时间原文，兼容 MySQL 在不同
// parseTime 配置下返回的 RFC3339、DATETIME 文本以及 SQLite 的带时区文本。
// 无法解析时返回 nil，由调用方按“来源未提供/水位未知”处理。
func ParseDashboardTimestamp(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}
