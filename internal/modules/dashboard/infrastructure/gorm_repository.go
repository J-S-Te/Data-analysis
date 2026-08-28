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
	}
	err := repository.db.WithContext(ctx).Table("api_contract_dashboard").
		Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return domain.ContractSnapshot{}, false, nil
	}
	if err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	return domain.ContractSnapshot{
		TenantID: row.TenantID, SnapshotAt: row.SnapshotAt, TotalAmountMinor: row.TotalAmountMinor,
		TotalContracts: row.TotalContracts, ApprovalContracts: row.ApprovalContracts,
		ActiveContracts: row.ActiveContracts, ExpiredContracts: row.ExpiredContracts,
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
