package application

import (
	"context"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

type snapshotRepositoryStub struct {
	contract          domain.ContractSnapshot
	contractAvailable bool
	project           domain.ProjectSnapshot
	projectAvailable  bool
}

func (stub snapshotRepositoryStub) LatestContract(context.Context, string) (domain.ContractSnapshot, bool, error) {
	return stub.contract, stub.contractAvailable, nil
}

func (stub snapshotRepositoryStub) LatestProject(context.Context, string) (domain.ProjectSnapshot, bool, error) {
	return stub.project, stub.projectAvailable, nil
}

func TestProjectInitializesEmptyStatusCountsForMissingSnapshot(t *testing.T) {
	service := NewService(snapshotRepositoryStub{})
	snapshot, err := service.Project(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Available || snapshot.TenantID != "tenant-1" || snapshot.StatusCounts == nil {
		t.Fatalf("unexpected missing snapshot: %#v", snapshot)
	}
}

func TestContractKeepsTenantBoundaryAtApplicationLayer(t *testing.T) {
	service := NewService(snapshotRepositoryStub{contract: domain.ContractSnapshot{TenantID: "wrong-tenant"}, contractAvailable: true})
	snapshot, err := service.Contract(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TenantID != "tenant-1" || !snapshot.Available {
		t.Fatalf("unexpected contract snapshot: %#v", snapshot)
	}
}

type advancedSnapshotRepositoryStub struct {
	snapshotRepositoryStub
	page     int
	pageSize int
}

func (stub *advancedSnapshotRepositoryStub) ListContracts(_ context.Context, _ string, page, pageSize int) ([]domain.ContractDetail, int64, error) {
	stub.page, stub.pageSize = page, pageSize
	return nil, 0, nil
}

func (stub *advancedSnapshotRepositoryStub) ListTrend(context.Context, string, int) ([]domain.TrendPoint, error) {
	return nil, nil
}

func TestContractsNormalizesPaginationBeforeRepositoryCall(t *testing.T) {
	service := NewService(&advancedSnapshotRepositoryStub{})
	repository := service.snapshots.(*advancedSnapshotRepositoryStub)
	_, _, err := service.Contracts(context.Background(), "tenant-1", 0, 101)
	if err != nil {
		t.Fatal(err)
	}
	// 应用边界不得传递仓储会静默重新解释的值，
	// 确保响应元数据与查询语义一致。
	if repository.page != 1 || repository.pageSize != 20 {
		t.Fatalf("pagination = (%d, %d), want (1, 20)", repository.page, repository.pageSize)
	}
}

// 趋势接口在无数据时必须返回空数组而不是 nil：nil 会被序列化成 JSON null，
// 前端据此显示"趋势加载失败"而不是"暂无数据"。
func TestTrendReturnsEmptyArrayWhenNoData(t *testing.T) {
	service := NewService(&advancedSnapshotRepositoryStub{})
	points, err := service.Trend(context.Background(), "tenant-1", 6)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if points == nil {
		t.Fatal("无数据时必须返回空数组，不能返回 nil")
	}
	if len(points) != 0 {
		t.Fatalf("points = %d，want 0", len(points))
	}
}
