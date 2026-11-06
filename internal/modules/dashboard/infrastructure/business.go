package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/standardfacts"
	"gorm.io/gorm"
)

// LatestBusiness 按明确来源读取当前租户财务或报告快照。
// 参数：ctx 为已授权上下文，tenant 为可信租户，section 为看板类别。
// 返回值：真实快照、未发布状态或来源错误；不跨来源回退。
func (repository *GORMRepository) LatestBusiness(ctx context.Context, tenant, section string) (domain.BusinessSnapshot, error) {
	if section == "finance" && repository.financeSource == "standard" {
		return (&standardfacts.Store{DB: repository.db}).Read(ctx, tenant)
	}
	var row struct{ Payload string }
	err := repository.db.WithContext(ctx).Table("business_dashboard_snapshot").Select("payload").Where("tenant_id = ? AND section = ?", tenant, section).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.BusinessSnapshot{TenantID: tenant, Metrics: map[string]any{}, MissingMetrics: []string{}, Basis: "尚未完成来源同步，请配置受控只读数据来源。"}, nil
	}
	if err != nil {
		return domain.BusinessSnapshot{}, err
	}
	var value domain.BusinessSnapshot
	if err = json.Unmarshal([]byte(row.Payload), &value); err != nil {
		return domain.BusinessSnapshot{}, err
	}
	if value.TenantID != tenant || value.Metrics == nil || !value.Available || value.SnapshotAt == "" {
		return domain.BusinessSnapshot{}, errors.New("invalid business statistics snapshot")
	}
	return value, nil
}
