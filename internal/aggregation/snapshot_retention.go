package aggregation

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// SnapshotRetentionPolicy 限定API快照的业务时间留存和每轮清理预算。
// 零值使用近7天、近24个业务月、每批500项、每轮最多20批。
type SnapshotRetentionPolicy struct {
	RecentDays  int
	TrendMonths int
	BatchSize   int
	MaxBatches  int
}

// normalized 校验策略并为零值填入保守默认值。
// 参数：无。
// 返回值：规范策略；负值或超过清理/内存预算时返回错误。
func (p SnapshotRetentionPolicy) normalized() (SnapshotRetentionPolicy, error) {
	if p.RecentDays == 0 {
		p.RecentDays = 7
	}
	if p.TrendMonths == 0 {
		p.TrendMonths = 24
	}
	if p.BatchSize == 0 {
		p.BatchSize = 500
	}
	if p.MaxBatches == 0 {
		p.MaxBatches = 20
	}
	invalidHistory := p.RecentDays < 1 || p.RecentDays > 3660 ||
		p.TrendMonths < 1 || p.TrendMonths > 120
	invalidBudget := p.BatchSize < 1 || p.BatchSize > 500 ||
		p.MaxBatches < 1 || p.MaxBatches > 20
	if invalidHistory || invalidBudget {
		return p, errors.New("invalid snapshot retention policy")
	}
	return p, nil
}

// snapshotRetentionCheck 检查取消与外部许可/租约守卫。
// 参数：ctx 为取消上下文，guard 为可选权限检查回调。
// 返回值：取消或守卫失败原样返回，成功为nil。
func snapshotRetentionCheck(ctx context.Context, guard func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard != nil {
		return guard(ctx)
	}
	return nil
}

// PruneSnapshotHistory 有界清理白名单API快照并保留近期明细、业务月末与全局最新。
// 参数：ctx/db 为取消上下文及库，tenant/table 为隔离范围，now/location 为业务时间；policy/guard 为预算与许可检查。
// 返回值：校验、取消、守卫或数据库失败返回错误；达到批次预算时安全留待下轮。
func PruneSnapshotHistory(
	ctx context.Context, db *gorm.DB, tenant, table string,
	now time.Time, location *time.Location, policy SnapshotRetentionPolicy,
	guard func(context.Context) error,
) error {
	write := func(action func(*gorm.DB) error) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := snapshotRetentionCheck(ctx, guard); err != nil {
				return err
			}
			if err := action(tx); err != nil {
				return err
			}
			return snapshotRetentionCheck(ctx, guard)
		})
	}
	return pruneSnapshotHistory(ctx, db, tenant, table, now, location, policy, guard, write)
}

// PruneSnapshotHistory 按执行器租户清理单一API快照表并持有队列写租约保护。
// 参数：ctx 为许可/取消上下文，table 为API快照白名单，now 为业务当前时间，policy 为预算。
// 返回值：校验、权限或数据库失败返回错误；清理预算耗尽时剩余项留待下轮。
func (r *Runner) PruneSnapshotHistory(
	ctx context.Context, table string, now time.Time, policy SnapshotRetentionPolicy,
) error {
	write := func(action func(*gorm.DB) error) error {
		return r.aggregationWrite(ctx, action)
	}
	return pruneSnapshotHistory(ctx, r.aggDB, r.tenantID, table, now,
		r.businessLocation(), policy, r.checkLicense, write)
}

// snapshotRetentionKeepIDs 读取固定高水位内的全局最新及各业务月末检查点。
// 参数：ctx/db 为查询上下文及库，tenant/table 为范围，high 为固定最大ID，now/location 为月界，months为保留月数。
// 返回值：不超过months+1个保留ID；查询失败返回错误。
func snapshotRetentionKeepIDs(
	ctx context.Context, db *gorm.DB, tenant, table string, high uint64,
	now time.Time, location *time.Location, months int,
) ([]uint64, error) {
	ids := make([]uint64, 0, months+1)
	month := time.Date(now.In(location).Year(), now.In(location).Month(), 1, 0, 0, 0, 0, location)
	for i := -1; i < months; i++ {
		query := db.WithContext(ctx).Table(table).Where("tenant_id = ? AND id <= ?", tenant, high)
		if i >= 0 {
			start := month.AddDate(0, -i, 0)
			query = query.Where("snapshot_at >= ? AND snapshot_at < ?", start.UTC(), start.AddDate(0, 1, 0).UTC())
		}
		var found []uint64
		if err := query.Order("snapshot_at DESC, id DESC").Limit(1).Pluck("id", &found).Error; err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	return ids, nil
}

// pruneSnapshotHistory 执行固定高水位的有界快照清理。
// 参数：前述参数限定范围与策略，write 为具备所需租约保护的事务写入器。
// 返回值：错误不会吞没，已完成批次保留提交，失败批次回滚。
func pruneSnapshotHistory(
	ctx context.Context, db *gorm.DB, tenant, table string,
	now time.Time, location *time.Location, policy SnapshotRetentionPolicy,
	guard func(context.Context) error, write func(func(*gorm.DB) error) error,
) error {
	if table != "api_contract_dashboard" && table != "api_project_dashboard" {
		return errors.New("unsupported snapshot retention table")
	}
	if db == nil || tenant == "" || now.IsZero() {
		return errors.New("snapshot retention database, tenant and time required")
	}
	policy, err := policy.normalized()
	if err != nil {
		return err
	}
	if location == nil {
		location = time.UTC
	}
	if err = snapshotRetentionCheck(ctx, guard); err != nil {
		return err
	}
	var high uint64
	if err = db.WithContext(ctx).Table(table).Where("tenant_id = ?", tenant).
		Select("COALESCE(MAX(id),0)").Scan(&high).Error; err != nil {
		return err
	}
	if high == 0 {
		return nil
	}
	keep, err := snapshotRetentionKeepIDs(ctx, db, tenant, table, high, now, location, policy.TrendMonths)
	if err != nil {
		return err
	}
	cutoff := now.In(location).AddDate(0, 0, -policy.RecentDays).UTC()
	for batch := 0; batch < policy.MaxBatches; batch++ {
		if err = snapshotRetentionCheck(ctx, guard); err != nil {
			return err
		}
		var ids []uint64
		err = write(func(tx *gorm.DB) error {
			query := tx.Table(table).Where("tenant_id = ? AND id <= ? AND snapshot_at < ?", tenant, high, cutoff)
			if len(keep) > 0 {
				query = query.Where("id NOT IN ?", keep)
			}
			if err := query.Order("id ASC").Limit(policy.BatchSize).Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			return tx.Table(table).
				Where("tenant_id = ? AND id <= ? AND id IN ?", tenant, high, ids).
				Delete(nil).Error
		})
		if err != nil {
			return err
		}
		if len(ids) < policy.BatchSize {
			return nil
		}
	}
	return nil
}
