package aggregation

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// 每个有效 CRM 商机归属其创建月份同期群。赢单要求
// 同租户内同时具有签约阶段及权威有效合同关联。
// EXISTS 防止单个商机关联多个合同时放大赢单数。
func (r *Runner) PrecomputeConversion(ctx context.Context) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	query := conversionFactQuery(r.aggDB.Dialector.Name())
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM fct_opportunity_conversion WHERE tenant_id = ?", r.tenantID).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		// 月份必须按业务时区（DASHBOARD_TIMEZONE，默认 Asia/Shanghai）归集：
		// period_month 是主键之一且口径为“业务发生日”，按 UTC 归月会把北京时间
		// 每月首日 00:00-08:00 创建的商机算到上一月。偏移作为绑定参数传入，
		// 时区可配且不硬编码 +08:00。
		offset := BusinessUTCOffset(r.businessLocation(), now)
		result := tx.Exec(query, offset, now, r.tenantID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return tx.Exec("INSERT INTO fct_opportunity_conversion (tenant_id,period_month,opportunity_count,won_contract_count,snapshot_at) VALUES (?, 'UNKNOWN',0,0,?)", r.tenantID, now).Error
		}
		return nil
	})
}

// conversionFactQuery 构造仅普通合同可佐证的去重商机转化查询。
// 参数：dialect 为数据库方言，用于安全统一月份和跨表字符比较。
// 返回值：带快照时间及租户占位符的INSERT SELECT，不修改源表排序规则。
// 占位符顺序：业务时区偏移（月份换算）、快照时间、租户。偏移只出现一次，
// 分组复用 period_month 别名，避免同一参数重复绑定。
func conversionFactQuery(dialect string) string {
	// o.created_at 是 UTC 墙钟 DATETIME，先换算到业务时区再取月前缀。
	month := "COALESCE(DATE_FORMAT(CONVERT_TZ(o.created_at, '+00:00', ?), '%Y-%m'), 'UNKNOWN')"
	id := "CAST(o.opportunity_id AS CHAR)"
	collate := ""
	if dialect == "sqlite" {
		month = "COALESCE(strftime('%Y-%m', o.created_at, ?), 'UNKNOWN')"
		id = "CAST(o.opportunity_id AS TEXT)"
	} else {
		collate = " COLLATE utf8mb4_unicode_ci"
	}
	// dim_* 的列可能是 ascii 字符集，直接写 COLLATE utf8mb4_unicode_ci 会报
	// "COLLATION ... is not valid for CHARACTER SET 'ascii'"；先 CONVERT 到 utf8mb4 再统一排序规则。
	wrap := func(expr string) string {
		if collate == "" {
			return expr
		}
		return "CONVERT(" + expr + " USING utf8mb4) COLLATE utf8mb4_unicode_ci"
	}
	match := "(" + wrap("c.opportunity_id") + " = " + wrap(id) +
		" OR (o.contract_id IS NOT NULL AND o.contract_id <> '' AND " + wrap("c.contract_id") + " = " + wrap("o.contract_id") + "))"
	return `INSERT INTO fct_opportunity_conversion (tenant_id,period_month,opportunity_count,won_contract_count,snapshot_at)
SELECT o.tenant_id, ` + month + ` AS period_month, COUNT(*), SUM(CASE WHEN o.current_stage = '已签约' AND EXISTS (
SELECT 1 FROM dim_contract c WHERE c.tenant_id=o.tenant_id AND c.origin_kind = 'NORMAL' AND c.status IN ('active','in_progress','pending_pay','completed')
AND ` + match + `) THEN 1 ELSE 0 END), ?
FROM dim_opportunity o WHERE o.tenant_id = ? AND o.deleted_at IS NULL AND o.opp_status <> 'VOID'
GROUP BY o.tenant_id, period_month`
}
