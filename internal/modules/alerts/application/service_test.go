package application

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

type repositoryStub struct {
	updatedTenantID string
	closedAt        *time.Time
}

func (stub *repositoryStub) ListByTenant(context.Context, string, int) ([]domain.Item, error) {
	return nil, nil
}

func (stub *repositoryStub) UpdateStatus(_ context.Context, _ string, tenantID, _ string, _ time.Time, closedAt *time.Time) (bool, error) {
	stub.updatedTenantID = tenantID
	stub.closedAt = closedAt
	return true, nil
}

func TestUpdateStatusClosesWithinTenant(t *testing.T) {
	stub := &repositoryStub{}
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	service := NewServiceWithClock(stub, func() time.Time { return now })
	updated, err := service.UpdateStatus(context.Background(), "tenant-1", "alert-1", "CLOSED")
	if err != nil || !updated || stub.updatedTenantID != "tenant-1" || stub.closedAt == nil || !stub.closedAt.Equal(now) {
		t.Fatalf("unexpected close result updated=%v tenant=%q closed=%v err=%v", updated, stub.updatedTenantID, stub.closedAt, err)
	}
}
