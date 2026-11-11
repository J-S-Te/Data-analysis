package metricfacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SnapshotRow 保存每个租户/来源当前完整快照，覆盖更新有时间和外层租约保护。
type SnapshotRow struct {
	TenantID         string    `gorm:"primaryKey;size:64"`
	SourceCode       string    `gorm:"primaryKey;size:32"`
	SourceSnapshotAt time.Time `gorm:"precision:3"`
	CollectedAt      time.Time `gorm:"precision:3"`
	PayloadJSON      string    `gorm:"type:json"`
}

// TableName 指向版本化迁移建立的独立指标快照表。
// 参数：无。
// 返回值：固定表名。
func (SnapshotRow) TableName() string { return "dashboard_metric_snapshot" }

// SourceKey 将来源映射到独立同步水位，避免污染既有看板来源状态。
// 参数：source为固定业务系统编码。
// 返回值：独立同步源编码；未知来源为空。
func SourceKey(source string) string {
	switch source {
	case "contract_management":
		return "contract_metrics"
	case "project_management":
		return "project_metrics"
	case "settlement":
		return "finance_metrics"
	}
	return ""
}

// Save 在调用者受许可/租约保护的事务内原子替换完整来源快照。
// 参数：tx为现有聚合写事务，bundle为完整来源，now为可信采集时刻。
// 返回值：非法、旧快照或数据库错误；错误应由外层事务回滚。
func Save(tx *gorm.DB, bundle Bundle, now time.Time) error {
	if tx == nil || SourceKey(bundle.SourceCode) == "" {
		return ErrInvalid
	}
	if err := Validate(bundle, bundle.TenantID, bundle.SourceCode, now); err != nil {
		return err
	}
	raw, err := json.Marshal(bundle)
	if err != nil || len(raw) > MaxBytes {
		return ErrInvalid
	}
	stamp, _ := time.Parse(time.RFC3339Nano, bundle.SnapshotAt)
	stamp = stamp.UTC().Truncate(time.Millisecond)
	row := SnapshotRow{TenantID: bundle.TenantID, SourceCode: bundle.SourceCode,
		SourceSnapshotAt: stamp, CollectedAt: now.UTC().Truncate(time.Millisecond), PayloadJSON: string(raw)}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	var current SnapshotRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id=? AND source_code=?", row.TenantID, row.SourceCode).Take(&current).Error; err != nil {
		return err
	}
	if current.SourceSnapshotAt.After(stamp) {
		return errors.New("指标来源快照早于当前已发布版本")
	}
	return tx.Model(&SnapshotRow{}).Where("tenant_id=? AND source_code=?", row.TenantID, row.SourceCode).
		Updates(map[string]any{"source_snapshot_at": row.SourceSnapshotAt,
			"collected_at": row.CollectedAt, "payload_json": row.PayloadJSON}).Error
}

// Store 只读取数析自有指标表，不跨系统查询业务数据库。
type Store struct {
	DB  *gorm.DB
	Now func() time.Time
}

// now 返回本次只读计算采用的可信UTC时间。
// 参数：无。
// 返回值：可注入测试时钟或系统UTC时刻。
func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Read 读取完整已发布来源，在可信范围内汇总并附真实同步状态。
// 参数：ctx为请求，tenant为验签租户，section/period为已验证查询，scope为在线范围。
// 返回值：明确未发布或统计；数据库/快照损坏返回错误，不伪装无数据。
func (s *Store) Read(ctx context.Context, tenant, section, period string, scope Scope) (Summary, error) {
	if s == nil || s.DB == nil || tenant == "" || SourceFor(section) == "" {
		return Summary{}, ErrInvalid
	}
	if err := validateScope(scope); err != nil {
		return Summary{}, err
	}
	var result Summary
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row SnapshotRow
		err := tx.Where("tenant_id=? AND source_code=?", tenant, SourceFor(section)).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = Summary{Available: false, Section: section, Period: period,
				SourceCode: SourceFor(section), Stale: true, StaleReason: "指标来源尚未完整发布"}
			return nil
		}
		if err != nil {
			return err
		}
		var bundle Bundle
		if len(row.PayloadJSON) > MaxBytes || json.Unmarshal([]byte(row.PayloadJSON), &bundle) != nil {
			return ErrInvalid
		}
		if err := Validate(bundle, tenant, row.SourceCode, s.now()); err != nil {
			return err
		}
		result, err = Summarize(bundle, section, period, scope)
		if err != nil {
			return err
		}
		return attachFreshness(tx, tenant, row, &result, s.now())
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	return result, err
}

// attachFreshness 同一读事务读取真实来源状态，失败时保留可用旧统计并标陈旧。
// 参数：tx为读事务，tenant/row为来源，out为统计，now为可信计算时刻。
// 返回值：水位存储错误；未首次成功、停用、失败或超期均明确标陈旧。
func attachFreshness(tx *gorm.DB, tenant string, row SnapshotRow, out *Summary, now time.Time) error {
	var state struct {
		Enabled       bool
		LastStatus    string
		LastSuccessAt *time.Time
	}
	err := tx.Table("sync_source").Where("tenant_id=? AND subsystem_code=?", tenant, SourceKey(row.SourceCode)).Take(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		out.Stale, out.StaleReason = true, "尚无指标来源成功凭据"
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case !state.Enabled:
		out.StaleReason = "指标来源已停用"
	case state.LastStatus != "SUCCESS":
		out.StaleReason = "指标来源最近状态为" + state.LastStatus
	case state.LastSuccessAt == nil:
		out.StaleReason = "指标来源尚未成功同步"
	case now.Sub(*state.LastSuccessAt) > 48*time.Hour || now.Sub(row.CollectedAt) > 48*time.Hour:
		out.StaleReason = "指标同步超过48小时展示容差"
	case state.LastSuccessAt.After(now.Add(time.Minute)) || row.CollectedAt.After(now.Add(time.Minute)):
		out.StaleReason = "指标来源水位异常"
	}
	out.Stale = out.StaleReason != ""
	return nil
}
