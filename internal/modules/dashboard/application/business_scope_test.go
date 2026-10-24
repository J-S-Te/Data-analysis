package application

import (
	"context"
	"errors"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type businessScopeRepository struct {
	snapshotRepositoryStub
	calls           int
	tenant, section string
	err             error
}

func (repo *businessScopeRepository) LatestBusiness(_ context.Context, tenant, section string) (domain.BusinessSnapshot, error) {
	repo.calls++
	repo.tenant, repo.section = tenant, section
	return domain.BusinessSnapshot{Available: true, TenantID: tenant, Metrics: map[string]any{"tenant_total": 9137}}, repo.err
}

// 仅含租户维度的物化结果不能提供给窄范围或缺失范围。
// 移除服务检查将暴露 tenant_total 并进入仓储。
func TestBusinessRejectsUnscopedTenantSnapshotsBeforeRepository(t *testing.T) {
	for _, section := range []string{"report", "finance"} {
		for _, scope := range []string{"ORG", "TEAM", "NONE", "", "UNKNOWN", "tenant", " TENANT "} {
			t.Run(section+"/"+scope, func(t *testing.T) {
				repo := &businessScopeRepository{}
				ctx := auth.WithPrincipal(context.Background(), auth.Principal{
					TenantID: "tenant-1", UserID: "director", ScopeMode: scope,
					OrganizationIDs: []string{"org-1"}, TeamIDs: []string{"team-1"}, Roles: []string{"admin"},
				})
				value, err := NewService(repo).Business(ctx, "tenant-1", section)
				if err == nil || value.Available || value.Metrics != nil || repo.calls != 0 {
					t.Fatalf("scope %q exposed tenant statistics: value=%#v err=%v reads=%d", scope, value, err, repo.calls)
				}
			})
		}
	}
}

func TestBusinessRequiresMatchingAuthenticatedTenant(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"missing_principal", context.Background()},
		{"mismatched_tenant", auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "other", ScopeMode: "TENANT"})},
		{"empty_principal_tenant", auth.WithPrincipal(context.Background(), auth.Principal{ScopeMode: "TENANT"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &businessScopeRepository{}
			value, err := NewService(repo).Business(tc.ctx, "tenant-1", "report")
			if err == nil || value.Available || value.Metrics != nil || repo.calls != 0 {
				t.Fatalf("invalid identity exposed tenant statistics: value=%#v err=%v reads=%d", value, err, repo.calls)
			}
		})
	}
}

func TestBusinessTenantScopeReadsRealSnapshotAndPreservesDependencyError(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "tenant-1", UserID: "boss", ScopeMode: "TENANT"})
	for _, section := range []string{"report", "finance"} {
		repo := &businessScopeRepository{}
		value, err := NewService(repo).Business(ctx, "tenant-1", section)
		if err != nil || !value.Available || value.Metrics["tenant_total"] != 9137 || repo.calls != 1 || repo.tenant != "tenant-1" || repo.section != section {
			t.Fatalf("tenant snapshot lost: value=%#v err=%v repo=%#v", value, err, repo)
		}
	}
	failure := errors.New("repository unavailable")
	repo := &businessScopeRepository{err: failure}
	_, err := NewService(repo).Business(ctx, "tenant-1", "report")
	if !errors.Is(err, failure) {
		t.Fatalf("dependency error swallowed: %v", err)
	}
}
