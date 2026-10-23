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
		result := tx.Exec(query, now, r.tenantID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return tx.Exec("INSERT INTO fct_opportunity_conversion (tenant_id,period_month,opportunity_count,won_contract_count,snapshot_at) VALUES (?, 'UNKNOWN',0,0,?)", r.tenantID, now).Error
		}
		return nil
	})
}

// conversionFactQuery 构造商机转化事实的 INSERT ... SELECT。
//
// MySQL 下 dim_contract 与 dim_opportunity 的字符集排序规则可能不同
// （utf8mb4_unicode_ci vs utf8mb4_0900_ai_ci），两列直接比较会报
// "Illegal mix of collations"。这里对参与比较的两侧显式指定同一排序规则，
// 而不是去改数据库/表的默认设置（那会影响其他系统）。
func conversionFactQuery(dialect string) string {
	month := "COALESCE(DATE_FORMAT(o.created_at, '%Y-%m'), 'UNKNOWN')"
	id := "CAST(o.opportunity_id AS CHAR)"
	collate := ""
	if dialect == "sqlite" {
		month = "COALESCE(strftime('%Y-%m', o.created_at), 'UNKNOWN')"
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
SELECT o.tenant_id, ` + month + `, COUNT(*), SUM(CASE WHEN o.current_stage = '已签约' AND EXISTS (
SELECT 1 FROM dim_contract c WHERE c.tenant_id=o.tenant_id AND c.status IN ('active','in_progress','pending_pay','completed')
AND ` + match + `) THEN 1 ELSE 0 END), ?
FROM dim_opportunity o WHERE o.tenant_id = ? AND o.deleted_at IS NULL AND o.opp_status <> 'VOID'
GROUP BY o.tenant_id, ` + month
}
