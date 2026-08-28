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
