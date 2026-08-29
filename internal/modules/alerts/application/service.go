// Package application implements alert-center use cases and owns its storage
// port. It intentionally does not depend on Gin or GORM.
package application

import (
	"context"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

const listLimit = 200

// Repository persists tenant-scoped alert state.
type Repository interface {
	ListByTenant(context.Context, string, int) ([]domain.Item, error)
	SummaryByTenant(context.Context, string) (domain.Summary, error)
	UpdateStatus(context.Context, string, string, string, time.Time, *time.Time) (bool, error)
}

// Service coordinates alert reads and state transitions.
type Service struct {
	repository Repository
	now        func() time.Time
}

// NewService constructs the production alert service.
func NewService(repository Repository) *Service {
	return NewServiceWithClock(repository, func() time.Time { return time.Now().UTC() })
}

// NewServiceWithClock constructs the alert service with a deterministic clock
// for unit tests. The clock is local to this module, not a cross-system helper.
func NewServiceWithClock(repository Repository, now func() time.Time) *Service {
	return &Service{repository: repository, now: now}
}

// List returns at most 200 alerts visible in the given tenant.
func (service *Service) List(ctx context.Context, tenantID string) ([]domain.Item, error) {
	return service.repository.ListByTenant(ctx, tenantID, listLimit)
}

// Summary 返回当前租户的预警总量、状态和严重度聚合。
func (service *Service) Summary(ctx context.Context, tenantID string) (domain.Summary, error) {
	return service.repository.SummaryByTenant(ctx, tenantID)
}

// UpdateStatus changes one alert state inside its tenant boundary. It reports
// found=false when the caller cannot see the alert or it no longer exists.
func (service *Service) UpdateStatus(ctx context.Context, tenantID, id, status string) (bool, error) {
	now := service.now()
	var closedAt *time.Time
	if status == "CLOSED" {
		closedAt = &now
	}
	return service.repository.UpdateStatus(ctx, id, tenantID, status, now, closedAt)
}
