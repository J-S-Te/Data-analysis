package infrastructure

import (
	"context"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// successfulWatermark 读取当前来源的持久成功凭据。
// 参数：watermark 为来源最近状态，失败轮次不得覆盖成功时间。
// 返回值：最近成功时间；无成功凭据时返回空。
func successfulWatermark(watermark syncWatermark) *time.Time {
	if at := applicationTimestamp(watermark.LastSuccessAt); at != nil {
		return at
	}
	if watermark.LastStatus == "SUCCESS" {
		return applicationTimestamp(watermark.LastWatermark)
	}
	return nil
}

// applicationTimestamp 解析数据库可空时间戳。
// 参数：value 为数据库字符串或空。
// 返回值：合法时间戳；无法解释时为空。
func applicationTimestamp(value *string) *time.Time {
	return application.ParseDashboardTimestamp(stringValue(value))
}

// scopedConversion 以各自owner范围汇总有效商机及普通合同佐证。
// 参数：ctx 为一致性快照上下文，scope 为可信权限范围。
// 返回值：分母、分子、双来源成功标记及数据库错误。
func (repository *GORMRepository) scopedConversion(ctx context.Context, scope queryScope) (int64, int64, bool, error) {
	watermark, known, err := repository.syncWatermark(ctx, scope.tenantID, "customer_and_opportunity")
	if err != nil {
		return 0, 0, false, err
	}
	if !known || successfulWatermark(watermark) == nil {
		return 0, 0, false, nil
	}
	wrap := func(value string) string {
		if repository.db.Dialector.Name() == "sqlite" {
			return value
		}
		return "CONVERT(" + value + " USING utf8mb4) COLLATE utf8mb4_unicode_ci"
	}
	id := "CAST(o.opportunity_id AS CHAR)"
	if repository.db.Dialector.Name() == "sqlite" {
		id = "CAST(o.opportunity_id AS TEXT)"
	}
	match := "(" + wrap("c.opportunity_id") + "=" + wrap(id) + " OR (o.contract_id IS NOT NULL AND o.contract_id<>'' AND " + wrap("c.contract_id") + "=" + wrap("o.contract_id") + "))"
	var row struct{ Count, Won int64 }
	query := repository.db.WithContext(ctx).Table("dim_opportunity o").Where("o.tenant_id=? AND o.deleted_at IS NULL AND o.opp_status<>'VOID'", scope.tenantID)
	if scope.mode == "SELF" {
		query = query.Where("o.owner_user_id=?", scope.userID)
	} else {
		query = applyOrgScope(query, scope, "o.owner_org_id")
	}
	clause := ""
	args := []any{}
	if scope.mode == "SELF" {
		clause = " AND c.owner_user_id=?"
		args = append(args, scope.userID)
	} else {
		clause = " AND c.owner_org_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(scope.orgUnitIDs)), ",") + ")"
		for _, id := range scope.orgUnitIDs {
			args = append(args, id)
		}
	}
	expression := "COUNT(*) count,COALESCE(SUM(CASE WHEN o.current_stage='已签约' AND EXISTS(SELECT 1 FROM dim_contract c WHERE c.tenant_id=o.tenant_id AND c.origin_kind='NORMAL' AND c.status IN ('active','in_progress','pending_pay','completed') AND " + match + clause + ") THEN 1 ELSE 0 END),0) won"
	err = query.Select(expression, args...).Scan(&row).Error
	return row.Count, row.Won, true, err
}

// scopedConversionFreshness 读取CRM转化分母来源的独立成功水位与当前状态。
// 参数：ctx 为调用方只读快照上下文，scope 为可信租户和权限范围。
// 返回值：CRM新鲜度及数据库错误；失败、停用与过期保留上次成功时间。
func (repository *GORMRepository) scopedConversionFreshness(ctx context.Context, scope queryScope) (domain.DataFreshness, error) {
	watermark, known, err := repository.syncWatermark(ctx, scope.tenantID, "customer_and_opportunity")
	if err != nil {
		return domain.DataFreshness{}, err
	}
	now := time.Now().UTC()
	successAt := successfulWatermark(watermark)
	freshness := repository.buildFreshness(application.FreshnessInput{
		LastSuccessAt: successAt, SyncStatus: watermark.LastStatus,
		SyncSourceKnown: known, SyncEnabled: watermark.Enabled,
	}, now)
	if successAt != nil && successAt.After(now) {
		freshness.Stale = true
		freshness.StaleReason = "CRM成功水位晚于本次查询，无法确认转化来源的新鲜度。"
	}
	return freshness, nil
}

// mergeScopedConversionFreshness 将CRM依赖的陈旧状态合并到合同看板。
// 参数：contract 为合同自身新鲜度，crm 为商机转化来源新鲜度。
// 返回值：保留合同自身三个时间的综合状态，CRM陈旧原因明确附带独立水位。
func mergeScopedConversionFreshness(contract, crm domain.DataFreshness) domain.DataFreshness {
	if !crm.Stale {
		return contract
	}
	reason := "CRM商机转化来源：" + crm.StaleReason
	if crm.SyncedAt != "" {
		reason += "（CRM最近成功同步：" + crm.SyncedAt + "）"
	}
	if contract.Stale && contract.StaleReason != "" {
		reason = "合同来源：" + contract.StaleReason + "；" + reason
	}
	if !contract.Stale || (crm.SyncStatus != domain.SyncStatusSuccess && crm.SyncStatus != domain.SyncStatusSnapshot) {
		contract.SyncStatus = crm.SyncStatus
	}
	contract.Stale = true
	contract.StaleReason = reason
	return contract
}
