package aggregation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	Payload   []byte `gorm:"type:json"`
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
	if err == nil {
		err = r.reconcileDimensions(ctx, source, run)
	}
	if err != nil {
		return errors.Join(err, r.cleanupDimensionRun(ctx, run))
	}
	return nil
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
			if err := publishDimensionStage(tx, r.tenantID, run, table); err != nil {
				return err
			}
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

// dimensionArgument 保留暂存参数的时间和整数类型，避免JSON浮点精度损失。
type dimensionArgument struct {
	Type  string
	Value json.RawMessage
}

// stageDimensionWrite 将完整行暂存到独立轮次，不修改当前维表。
// 参数：ctx/run 为许可上下文及轮次，table/query 为固定维表与SQL，args 为本行值。
// 返回值：非法SQL、参数或暂存失败返回错误，当前代次始终不变。
func (r *Runner) stageDimensionWrite(ctx context.Context, run, table, query string, args ...any) error {
	if dimensionUpsert(table) != query || query == "" || run == "" {
		return errors.New("invalid dimension stage")
	}
	if err := validateDimensionArguments(r.tenantID, table, args); err != nil {
		return err
	}
	values := make([]dimensionArgument, len(args))
	for i, value := range args {
		if value != nil {
			v := reflect.ValueOf(value)
			if v.Kind() == reflect.Ptr {
				if v.IsNil() {
					value = nil
				} else {
					value = v.Elem().Interface()
				}
			}
		}
		kind := "value"
		if _, ok := value.(time.Time); ok {
			kind = "time"
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		values[i] = dimensionArgument{kind, raw}
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return errors.New("dimension row exceeds staging bound")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	row := dimensionSyncKey{TenantID: r.tenantID, RunID: run, Dimension: table,
		SourceKey: "stage:" + hex.EncodeToString(token), ParentKey: "stage", CreatedAt: time.Now().UTC(), Payload: payload}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error { return tx.Create(&row).Error })
}

// dimensionUpsert 仅允许当前维表对应的固定写入SQL。
// 参数：table 为受控维表名。
// 返回值：固定SQL；非暂存来源返回空串。
func dimensionUpsert(table string) string {
	switch table {
	case "dim_contract":
		return dimContractUpsert
	case "dim_customer":
		return dimCustomerUpsert
	case "dim_opportunity":
		return dimOpportunityUpsert
	}
	return ""
}

// publishDimensionStage 在发布事务内逐批回放完整快照的暂存行。
// 参数：tx 为受许可/租约围栏保护的事务，tenant/run/table 为可信轮次边界。
// 返回值：任何行读取、解码或合并失败均回滚整次发布。
func publishDimensionStage(tx *gorm.DB, tenant, run, table string) error {
	if dimensionUpsert(table) == "" {
		return nil
	}
	var cursor uint64
	for {
		var rows []dimensionSyncKey
		err := tx.Where("tenant_id=? AND run_id=? AND dimension=? AND parent_key='stage' AND id>?", tenant, run, table, cursor).Order("id").Limit(sourceReadBatchSize).Find(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			var values []dimensionArgument
			if json.Unmarshal(row.Payload, &values) != nil {
				return errors.New("invalid staged dimension row")
			}
			args := make([]any, len(values))
			for i, value := range values {
				if value.Type == "time" {
					var at time.Time
					if err := json.Unmarshal(value.Value, &at); err != nil {
						return err
					}
					args[i] = at
				} else {
					var v any
					decoder := json.NewDecoder(strings.NewReader(string(value.Value)))
					decoder.UseNumber()
					if err := decoder.Decode(&v); err != nil {
						return err
					}
					if n, ok := v.(json.Number); ok {
						v = n.String()
					}
					args[i] = v
				}
			}
			if err := validateDimensionArguments(tenant, table, args); err != nil {
				return err
			}
			if err := tx.Exec(dimensionUpsert(table), args...).Error; err != nil {
				return err
			}
			cursor = row.ID
		}
	}
}

// validateDimensionArguments 校验暂存及发布参数形状与可信租户，拒绝跨租户回放。
// 参数：tenant/table 为本轮可信目标，args 为完整维表参数。
// 返回值：参数数目或租户不符时错误。
func validateDimensionArguments(tenant, table string, args []any) error {
	count, index := 0, 0
	switch table {
	case "dim_contract":
		count, index = 19, 1
	case "dim_customer":
		count = 12
	case "dim_opportunity":
		count = 20
	default:
		return errors.New("invalid dimension table")
	}
	if tenant == "" || len(args) != count || args[index] != tenant {
		return errors.New("invalid dimension arguments or tenant")
	}
	return nil
}

// cleanupDimensionRun 在仍持有许可和租约时分批清理本轮失败暂存。
// 参数：ctx 为原轮次上下文，run 为已生成的唯一令牌。
// 返回值：清理或围栏失败返回错误；失去权限的残留由后续有界过期回收处理。
func (r *Runner) cleanupDimensionRun(ctx context.Context, run string) error {
	for {
		removed := 0
		err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
			var ids []uint64
			if err := tx.Model(&dimensionSyncKey{}).Where("tenant_id=? AND run_id=?", r.tenantID, run).Order("id").Limit(sourceReadBatchSize).Pluck("id", &ids).Error; err != nil {
				return err
			}
			removed = len(ids)
			if removed == 0 {
				return nil
			}
			return tx.Where("tenant_id=? AND run_id=? AND id IN ?", r.tenantID, run, ids).Delete(&dimensionSyncKey{}).Error
		})
		if err != nil {
			return err
		}
		if removed < sourceReadBatchSize {
			return nil
		}
	}
}
