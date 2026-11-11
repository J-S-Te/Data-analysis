package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
	"time"
)

// ListProjects 在只读RR快照内分页读取当前成功发布代次的项目。
// 参数：ctx 含可信会话范围，tenantID 为租户，page/pageSize 为分页参数。
// 返回值：脱敏明细及来源水位；无发布或无范围时不可用，成功空范围可用。
func (repository *GORMRepository) ListProjects(ctx context.Context, tenantID string, page, pageSize int) (domain.ProjectDetails, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	result := domain.ProjectDetails{Items: []domain.ProjectDetail{}, Page: page, PageSize: pageSize}
	scope := scopeFor(ctx, tenantID)
	if scope.mode != "TENANT" && !scope.projectRowsVisible() {
		return result, nil
	}
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state aggregation.ProjectProjectionState
		err := tx.Where("tenant_id=? AND projection_code=?", tenantID, aggregation.ProjectProjectionCode).Take(&state).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !projectPublished(state) {
			return nil
		}
		query := applyOrgScope(tx.Table("dim_project").Where("tenant_id=? AND snapshot_id=?", tenantID, state.PublishedSnapshotID), scope, "owner_org_id")
		if err = query.Count(&result.Total).Error; err != nil {
			return err
		}
		if err = query.Select("project_id,name,status,manager,owner_org_id,service_items,report_stage,risk,in_flight").Order("source_updated_at DESC, project_id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&result.Items).Error; err != nil {
			return err
		}
		var source struct{ UpdatedAt *string }
		if err = applyOrgScope(tx.Table("dim_project").Where("tenant_id=? AND snapshot_id=?", tenantID, state.PublishedSnapshotID), scope, "owner_org_id").Select("MAX(source_updated_at) updated_at").Scan(&source).Error; err != nil {
			return err
		}
		watermark, known, err := (&GORMRepository{db: tx}).syncWatermark(ctx, tenantID, "project_management")
		if err != nil {
			return err
		}
		enabled := true
		if known {
			enabled = watermark.Enabled
		}
		result.Freshness = repository.buildFreshness(application.FreshnessInput{SourceUpdatedAt: applicationTimestamp(source.UpdatedAt), LastSuccessAt: state.LastSuccessAt, SyncStatus: state.Status, SyncSourceKnown: true, SyncEnabled: enabled}, time.Now().UTC())
		result.Available = true
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}
