// Package application 实现看板查询用例。仓储是
// 本模块拥有的端口，不是跨子系统共享抽象。
package application

import (
	"context"
	"errors"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// ErrInvalidProjectSnapshot 标识状态计数载荷无法
// 解释的持久化项目快照。HTTP 适配器保留该情况的
// 既有公开错误码，无需依赖 GORM 细节。
var ErrInvalidProjectSnapshot = errors.New("invalid project dashboard snapshot")

// SnapshotRepository 读取租户范围内的看板聚合。
type SnapshotRepository interface {
	LatestContract(context.Context, string) (domain.ContractSnapshot, bool, error)
	LatestProject(context.Context, string) (domain.ProjectSnapshot, bool, error)
}

// Service 协调看板读取用例，无需了解传输
// 协议或存储技术。
type Service struct{ snapshots SnapshotRepository }

type advancedRepository interface {
	ListContracts(context.Context, string, int, int) ([]domain.ContractDetail, int64, error)
	ListTrend(context.Context, string, int) ([]domain.TrendPoint, error)
}

const (
	defaultContractDetailPage     = 1
	defaultContractDetailPageSize = 20
	maxContractDetailPageSize     = 100
)

func normalizeContractDetailPagination(page, pageSize int) (int, int) {
	if page < 1 {
		page = defaultContractDetailPage
	}
	if pageSize < 1 || pageSize > maxContractDetailPageSize {
		pageSize = defaultContractDetailPageSize
	}
	return page, pageSize
}

// NewService 构造看板应用服务。
func NewService(snapshots SnapshotRepository) *Service { return &Service{snapshots: snapshots} }

// Contract 返回租户最新合同聚合。尚未运行聚合时，返回的快照
// 有意为空，但仍有效。
func (service *Service) Contract(ctx context.Context, tenantID string) (domain.ContractSnapshot, error) {
	snapshot, available, err := service.snapshots.LatestContract(ctx, tenantID)
	if err != nil {
		return domain.ContractSnapshot{}, err
	}
	snapshot.TenantID = tenantID
	snapshot.Available = available
	if snapshot.DiscountBuckets == nil {
		snapshot.DiscountBuckets = map[string]int64{}
	}
	return snapshot, nil
}

// Project 返回租户最新项目聚合。尚未运行聚合时，返回的快照
// 有意为空，但仍有效。
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
	page, pageSize = normalizeContractDetailPagination(page, pageSize)
	return repository.ListContracts(ctx, tenantID, page, pageSize)
}

// Trend 返回合同与项目月度趋势。
func (service *Service) Trend(ctx context.Context, tenantID string, months int) ([]domain.TrendPoint, error) {
	repository, ok := service.snapshots.(advancedRepository)
	if !ok {
		return nil, errors.New("dashboard trend repository is not configured")
	}
	points, err := repository.ListTrend(ctx, tenantID, months)
	if err != nil {
		return nil, err
	}
	// 契约：趋势接口永远返回数组。无数据时返回 []，否则 JSON 会变成 null，
	// 前端会把"暂无数据"误判为"趋势加载失败"。
	if points == nil {
		points = []domain.TrendPoint{}
	}
	return points, nil
}
