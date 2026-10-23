package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
)

func (repository *GORMRepository) LatestBusiness(ctx context.Context, tenant, section string) (domain.BusinessSnapshot, error) {
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
