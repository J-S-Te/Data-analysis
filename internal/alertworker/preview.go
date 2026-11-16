package alertworker

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PreviewContractExpiry 使用与 Worker 相同的候选查询和范围过滤做只读预览。
// 参数：db 为聚合库；tenantID 为可信租户；evaluatedAt 为业务时区评估时刻；
// days 为到期前后窗口；scope 为规则范围；limit 为样例上限。
// 返回值：命中总数、样例候选和错误；不写 alert_item，不改变规则状态。
func PreviewContractExpiry(ctx context.Context, db *gorm.DB, tenantID string, evaluatedAt time.Time, days int, scope RuleScope, limit int) (int64, []ContractExpiryCandidate, error) {
	if db == nil || tenantID == "" || days < 1 || days > 3650 {
		return 0, nil, fmt.Errorf("invalid contract expiry preview input")
	}
	if limit < 1 || limit > 20 {
		limit = 20
	}
	store := NewGormStore(db, tenantID)
	candidates, err := store.FindContractExpiryCandidates(ctx, businessDayStart(evaluatedAt), days)
	if err != nil {
		return 0, nil, err
	}
	matched := make([]ContractExpiryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if matchesRuleScope(scope, candidate) {
			matched = append(matched, candidate)
		}
	}
	sample := matched
	if len(sample) > limit {
		sample = sample[:limit]
	}
	return int64(len(matched)), sample, nil
}
