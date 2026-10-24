package application

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

type repositoryStub struct {
	updatedTenantID string
	updatedScope    AlertScope
	closedAt        *time.Time
}

func (stub *repositoryStub) ListByTenant(context.Context, string, ListQuery) (ListResult, error) {
	return ListResult{}, nil
}
func (stub *repositoryStub) SummaryByTenant(context.Context, string, AlertScope) (domain.Summary, error) {
	return domain.Summary{BySeverity: map[string]int64{}, ByType: map[string]int64{}}, nil
}

func (stub *repositoryStub) UpdateStatus(_ context.Context, _ string, tenantID, _ string, scope AlertScope, _ time.Time, closedAt *time.Time) (bool, error) {
	stub.updatedTenantID = tenantID
	stub.updatedScope = scope
	stub.closedAt = closedAt
	return true, nil
}

func TestUpdateStatusClosesWithinTenant(t *testing.T) {
	stub := &repositoryStub{}
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	service := NewServiceWithClock(stub, func() time.Time { return now })
	scope := AlertScope{Mode: "ORG", OrgUnitIDs: []string{"org-1"}}
	updated, err := service.UpdateStatus(context.Background(), "tenant-1", "alert-1", "CLOSED", scope)
	if err != nil || !updated || stub.updatedTenantID != "tenant-1" || stub.updatedScope.Mode != "ORG" || len(stub.updatedScope.OrgUnitIDs) != 1 || stub.closedAt == nil || !stub.closedAt.Equal(now) {
		t.Fatalf("unexpected close result updated=%v tenant=%q scope=%+v closed=%v err=%v", updated, stub.updatedTenantID, stub.updatedScope, stub.closedAt, err)
	}
}
