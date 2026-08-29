// Package application implements dashboard query use cases. Its repository is
// a port owned by this module rather than a cross-subsystem shared abstraction.
package application

import (
	"context"
	"errors"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// ErrInvalidProjectSnapshot identifies a persisted project snapshot whose
// status-count payload cannot be interpreted. HTTP adapters keep the existing
// public error code for this condition without depending on GORM details.
var ErrInvalidProjectSnapshot = errors.New("invalid project dashboard snapshot")

// SnapshotRepository reads tenant-scoped dashboard aggregates.
type SnapshotRepository interface {
	LatestContract(context.Context, string) (domain.ContractSnapshot, bool, error)
	LatestProject(context.Context, string) (domain.ProjectSnapshot, bool, error)
}

// Service coordinates dashboard read use cases without knowing the transport
// protocol or storage technology.
type Service struct{ snapshots SnapshotRepository }

type advancedRepository interface {
	ListContracts(context.Context, string, int, int) ([]domain.ContractDetail, int64, error)
	ListTrend(context.Context, string, int) ([]domain.TrendPoint, error)
}

// NewService constructs the dashboard application service.
func NewService(snapshots SnapshotRepository) *Service { return &Service{snapshots: snapshots} }

// Contract returns the latest tenant contract aggregate. The returned snapshot
// is intentionally empty-but-valid when aggregation has not run yet.
func (service *Service) Contract(ctx context.Context, tenantID string) (domain.ContractSnapshot, error) {
	snapshot, available, err := service.snapshots.LatestContract(ctx, tenantID)
	if err != nil {
		return domain.ContractSnapshot{}, err
	}
	snapshot.TenantID = tenantID
	snapshot.Available = available
	return snapshot, nil
}

// Project returns the latest tenant project aggregate. The returned snapshot
// is intentionally empty-but-valid when aggregation has not run yet.
func (service *Service) Project(ctx context.Context, tenantID string) (domain.ProjectSnapshot, error) {
	snapshot, available, err := service.snapshots.LatestProject(ctx, tenantID)
	if err != nil {
		return domain.ProjectSnapshot{}, err
	}
	snapshot.TenantID = tenantID
	snapshot.Available = available
	if snapshot.StatusCounts == nil {
		snapshot.StatusCounts = map[string]int{}
	}
	return snapshot, nil
}

// Contracts 返回租户合同下钻明细，并限制分页大小。
func (service *Service) Contracts(ctx context.Context, tenantID string, page, pageSize int) ([]domain.ContractDetail, int64, error) {
	repository, ok := service.snapshots.(advancedRepository)
	if !ok {
		return nil, 0, errors.New("contract detail repository is not configured")
	}
	return repository.ListContracts(ctx, tenantID, page, pageSize)
}

// Trend 返回合同与项目月度趋势。
func (service *Service) Trend(ctx context.Context, tenantID string, months int) ([]domain.TrendPoint, error) {
	repository, ok := service.snapshots.(advancedRepository)
	if !ok {
		return nil, errors.New("dashboard trend repository is not configured")
	}
	return repository.ListTrend(ctx, tenantID, months)
}
