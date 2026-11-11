package aggregation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// dimensionSyncKey 记录某次完整源快照实际读到的主键，不保存业务内容。
type dimensionSyncKey struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement"`
	TenantID  string `gorm:"uniqueIndex:uk_dimension_sync_key,priority:1"`
	RunID     string `gorm:"uniqueIndex:uk_dimension_sync_key,priority:2"`
	Dimension string `gorm:"uniqueIndex:uk_dimension_sync_key,priority:3"`
	SourceKey string `gorm:"uniqueIndex:uk_dimension_sync_key,priority:4"`
	ParentKey string `gorm:"uniqueIndex:uk_dimension_sync_key,priority:5"`
	CreatedAt time.Time
}

// TableName 返回完整快照主键暂存表名。
// 参数：无。
// 返回值：仅用于删除对账的暂存表名。
func (dimensionSyncKey) TableName() string { return "dimension_sync_key" }

// dimensionTables 返回固定来源允许对账的维表清单。
// 参数：source 为内部来源编码。
// 返回值：固定维表清单；未知来源返回错误，拒绝拼接任意表名。
func dimensionTables(source string) ([]string, error) {
	switch source {
	case "contract_management":
		return []string{"dim_contract"}, nil
	case "customer_and_opportunity":
		return []string{"dim_customer", "dim_opportunity"}, nil
	case ContractApprovalSource:
		return []string{"dim_contract_approval"}, nil
	default:
		return nil, errors.New("unsupported dimension reconciliation source")
	}
}

// dimensionKeyPredicate 返回维表与暂存集合之间的精确主键连接条件。
// 参数：table 为内部白名单维表名称。
// 返回值：固定SQL条件；未知表返回错误。
func dimensionKeyPredicate(table string) (string, error) {
	switch table {
	case "dim_contract":
		return "k.source_key = dim_contract.contract_id AND k.parent_key = ''", nil
	case "dim_customer":
		return "k.source_key = CAST(dim_customer.customer_id AS CHAR) AND k.parent_key = ''", nil
	case "dim_opportunity":
		return "k.source_key = CAST(dim_opportunity.opportunity_id AS CHAR) AND k.parent_key = ''", nil
	case "dim_contract_approval":
		return "k.source_key = dim_contract_approval.approval_id AND " +
			"k.parent_key = dim_contract_approval.contract_id", nil
	default:
		return "", errors.New("unsupported dimension reconciliation table")
	}
}

// approvalReconciliationKey 编码审批实例的合同与审批复合主键。
// 参数：contract 为所属合同ID，approval 为审批实例ID。
// 返回值：仅供内部暂存使用的复合键；ID由源系统生成，不含分隔符。
func approvalReconciliationKey(contract, approval string) string { return contract + "\x1f" + approval }

// stageDimensionKeys 保存一个有界读取批次中的主键集合。
// 参数：ctx 为许可/取消上下文，run 为随机轮次令牌，table 为维表，keys 为本批主键。
// 返回值：写入失败或键无效返回错误，不发布删除。
func (r *Runner) stageDimensionKeys(ctx context.Context, run, table string, keys []string) error {
	if _, err := dimensionKeyPredicate(table); err != nil {
		return err
	}
	if run == "" || r.tenantID == "" || len(keys) > sourceReadBatchSize {
		return errors.New("invalid dimension staging batch")
	}
	rows := make([]dimensionSyncKey, 0, len(keys))
	for _, key := range keys {
		parent := ""
		if table == "dim_contract_approval" {
			parts := strings.Split(key, "\x1f")
			if len(parts) != 2 || parts[0] == "" {
				return errors.New("invalid approval reconciliation key")
			}
			parent, key = parts[0], parts[1]
		}
		if key == "" {
			return errors.New("empty dimension reconciliation key")
		}
		rows = append(rows, dimensionSyncKey{
			TenantID: r.tenantID, RunID: run, Dimension: table,
			SourceKey: key, ParentKey: parent, CreatedAt: time.Now().UTC(),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error { return tx.Create(&rows).Error })
}

// cleanupDimensionKeys 有界回收本来源旧失败轮次的主键暂存。
// 参数：ctx 为权限上下文，now 为UTC清理时间，tables 为当前持锁来源的维表。
// 返回值：最多删除500项；取消、许可、租约或数据库错误直接返回。
func (r *Runner) cleanupDimensionKeys(ctx context.Context, now time.Time, tables ...string) error {
	for _, table := range tables {
		if _, err := dimensionKeyPredicate(table); err != nil {
			return err
		}
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		var ids []uint64
		query := tx.Model(&dimensionSyncKey{}).
			Where("tenant_id = ? AND created_at < ?", r.tenantID, now.Add(-24*time.Hour))
		if len(tables) > 0 {
			query = query.Where("dimension IN ?", tables)
		}
		if err := query.Order("id ASC").Limit(sourceReadBatchSize).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		return tx.Where("tenant_id = ? AND id IN ?", r.tenantID, ids).Delete(&dimensionSyncKey{}).Error
	})
}

// runDimensionSnapshot 在完整只读RR事务成功提交后发布维表删除对账。
// 参数：ctx 为取消/许可上下文，src 为源库，source 为已持锁来源，scan 为分批读取与写入。
// 返回值：读取、提交、权限或对账失败返回错误；失败轮次保留原维表及暂存供有界回收。
func (r *Runner) runDimensionSnapshot(
	ctx context.Context, src *gorm.DB, source string, scan func(*gorm.DB, string) error,
) error {
	tables, err := dimensionTables(source)
	if err != nil {
		return err
	}
	if err = r.checkLicense(ctx); err != nil {
		return err
	}
	if err = r.cleanupDimensionKeys(ctx, time.Now().UTC(), tables...); err != nil {
		return err
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return fmt.Errorf("create dimension sync token: %w", err)
	}
	run := hex.EncodeToString(token)
	err = src.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := scan(tx.Where("tenant_id = ?", r.tenantID), run); err != nil {
			return err
		}
		return r.checkLicense(ctx)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	return r.reconcileDimensions(ctx, source, run)
}

// reconcileDimensions 原子删除完整成功快照中已不存在的本租户维表行。
// 参数：ctx 为许可/取消上下文，source 为当前持锁来源，run 为本轮完整集合令牌。
// 返回值：全部维表删除及暂存清理一起提交；任一步失败全部回滚。
func (r *Runner) reconcileDimensions(ctx context.Context, source, run string) error {
	tables, err := dimensionTables(source)
	if err != nil {
		return err
	}
	if run == "" || r.tenantID == "" {
		return errors.New("dimension reconciliation identity required")
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		for _, table := range tables {
			predicate, err := dimensionKeyPredicate(table)
			if err != nil {
				return err
			}
			query := "DELETE FROM " + table + " WHERE tenant_id = ? AND NOT EXISTS (" +
				"SELECT 1 FROM dimension_sync_key k WHERE k.tenant_id = ? " +
				"AND k.run_id = ? AND k.dimension = ? AND " + predicate + ")"
			if err = tx.Exec(query, r.tenantID, r.tenantID, run, table).Error; err != nil {
				return err
			}
		}
		return tx.Where("tenant_id = ? AND run_id = ? AND dimension IN ?", r.tenantID, run, tables).
			Delete(&dimensionSyncKey{}).Error
	})
}
