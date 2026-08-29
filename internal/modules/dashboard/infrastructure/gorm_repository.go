// Package infrastructure contains storage adapters owned by the dashboard
// module. It is local to data analysis and is not a reusable subsystem package.
package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// GORMRepository reads materialized dashboard snapshots from MySQL views.
type GORMRepository struct{ db *gorm.DB }

// NewGORMRepository constructs the dashboard persistence adapter.
func NewGORMRepository(db *gorm.DB) *GORMRepository { return &GORMRepository{db: db} }

// LatestContract returns the newest contract aggregate for a tenant.
func (repository *GORMRepository) LatestContract(ctx context.Context, tenantID string) (domain.ContractSnapshot, bool, error) {
	var row struct {
		TenantID          string `gorm:"column:tenant_id"`
		SnapshotAt        string `gorm:"column:snapshot_at"`
		TotalAmountMinor  int64  `gorm:"column:total_amount_minor"`
		TotalContracts    int64  `gorm:"column:total_contracts"`
		ApprovalContracts int64  `gorm:"column:approval_contracts"`
		ActiveContracts   int64  `gorm:"column:active_contracts"`
		ExpiredContracts  int64  `gorm:"column:expired_contracts"`
		OpportunityCount  int64  `gorm:"column:opportunity_count"`
		WonContractCount  int64  `gorm:"column:won_contract_count"`
	}
	err := repository.db.WithContext(ctx).Table("api_contract_dashboard").
		Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return domain.ContractSnapshot{}, false, nil
	}
	if err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	var funnel struct {
		OpportunityCount int64
		WonContractCount int64
	}
	if err := repository.db.WithContext(ctx).Table("fct_contract_signing").Select("COALESCE(SUM(opportunity_count),0) opportunity_count, COALESCE(SUM(won_contract_count),0) won_contract_count").Where("tenant_id = ?", tenantID).Scan(&funnel).Error; err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	buckets := map[string]int64{}
	var bucketRows []struct {
		Bucket string `gorm:"column:bucket"`
		Count  int64  `gorm:"column:count"`
	}
	if err := repository.db.WithContext(ctx).Table("fct_contract_signing").Select("discount_rate_bucket bucket, COALESCE(SUM(contract_count),0) count").Where("tenant_id = ?", tenantID).Group("discount_rate_bucket").Scan(&bucketRows).Error; err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	for _, item := range bucketRows {
		buckets[item.Bucket] = item.Count
	}
	return domain.ContractSnapshot{
		TenantID: row.TenantID, SnapshotAt: row.SnapshotAt, TotalAmountMinor: row.TotalAmountMinor,
		TotalContracts: row.TotalContracts, ApprovalContracts: row.ApprovalContracts,
		ActiveContracts: row.ActiveContracts, ExpiredContracts: row.ExpiredContracts,
		OpportunityCount: funnel.OpportunityCount, WonContractCount: funnel.WonContractCount, DiscountBuckets: buckets,
	}, true, nil
}

// LatestProject returns the newest project aggregate for a tenant.
func (repository *GORMRepository) LatestProject(ctx context.Context, tenantID string) (domain.ProjectSnapshot, bool, error) {
	var row struct {
		TenantID         string `gorm:"column:tenant_id"`
		SnapshotAt       string `gorm:"column:snapshot_at"`
		ProjectCount     int    `gorm:"column:project_count"`
		InFlightProjects int    `gorm:"column:in_flight_projects"`
		RiskProjects     int    `gorm:"column:risk_projects"`
		ServiceItems     int    `gorm:"column:service_items"`
		StatusCountsJSON string `gorm:"column:status_counts_json"`
	}
	err := repository.db.WithContext(ctx).Table("api_project_dashboard").
		Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return domain.ProjectSnapshot{StatusCounts: map[string]int{}}, false, nil
	}
	if err != nil {
		return domain.ProjectSnapshot{}, false, err
	}
	statusCounts := map[string]int{}
	if row.StatusCountsJSON != "" {
		if err := json.Unmarshal([]byte(row.StatusCountsJSON), &statusCounts); err != nil {
			return domain.ProjectSnapshot{}, false, fmt.Errorf("%w: %v", application.ErrInvalidProjectSnapshot, err)
		}
	}
	return domain.ProjectSnapshot{
		TenantID: row.TenantID, SnapshotAt: row.SnapshotAt, ProjectCount: row.ProjectCount,
		InFlightProjects: row.InFlightProjects, RiskProjects: row.RiskProjects,
		ServiceItems: row.ServiceItems, StatusCounts: statusCounts,
	}, true, nil
}

// ListContracts 返回租户合同脱敏明细，分页参数由应用层约束后再执行。
func (repository *GORMRepository) ListContracts(ctx context.Context, tenantID string, page, pageSize int) ([]domain.ContractDetail, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	query := repository.db.WithContext(ctx).Table("dim_contract").Where("tenant_id = ?", tenantID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []domain.ContractDetail
	err := query.Select("contract_number,title,status,amount_minor,COALESCE(DATE_FORMAT(end_date,'%Y-%m-%d'),'') end_date").Order("updated_at DESC, contract_id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&rows).Error
	return rows, total, err
}

// ListTrend 返回最近若干月的合同和项目趋势；缺失月份由前端按空值展示。
func (repository *GORMRepository) ListTrend(ctx context.Context, tenantID string, months int) ([]domain.TrendPoint, error) {
	if months < 1 || months > 24 {
		months = 12
	}
	var rows []domain.TrendPoint
	err := repository.db.WithContext(ctx).Table("fct_contract_signing f").Select("f.period_month period, COALESCE(SUM(f.sign_amount_minor),0) contract_amount_minor, COALESCE(SUM(f.contract_count),0) contract_count, 0 project_count").Where("f.tenant_id = ?", tenantID).Group("f.period_month").Order("f.period_month DESC").Limit(months).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	// 项目快照按月取最新一条，避免把每日快照累加造成重复计数。
	var projects []struct {
		Period string `gorm:"column:period"`
		Count  int    `gorm:"column:project_count"`
	}
	if err := repository.db.WithContext(ctx).Table("api_project_dashboard").Select("DATE_FORMAT(snapshot_at, '%Y-%m') period, project_count").Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").Scan(&projects).Error; err != nil {
		return nil, err
	}
	projectByPeriod := make(map[string]int, len(projects))
	for _, item := range projects {
		if _, exists := projectByPeriod[item.Period]; !exists {
			projectByPeriod[item.Period] = item.Count
		}
	}
	for index := range rows {
		rows[index].ProjectCount = int64(projectByPeriod[rows[index].Period])
	}
	return rows, nil
}
