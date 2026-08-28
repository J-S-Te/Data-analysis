// Package infrastructure contains the GORM adapter owned by the alert module.
package infrastructure

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

type alertRecord struct {
	ID        string  `gorm:"column:id;primaryKey"`
	TenantID  string  `gorm:"column:tenant_id"`
	AlertType string  `gorm:"column:alert_type"`
	RuleCode  string  `gorm:"column:rule_code"`
	Severity  string  `gorm:"column:severity"`
	TargetRef string  `gorm:"column:target_ref"`
	Title     string  `gorm:"column:title"`
	DueDate   *string `gorm:"column:due_date"`
	Status    string  `gorm:"column:status"`
	ClosedAt  *string `gorm:"column:closed_at"`
	CreatedAt string  `gorm:"column:created_at"`
	UpdatedAt string  `gorm:"column:updated_at"`
}

func (alertRecord) TableName() string { return "alert_item" }

func (record alertRecord) toDomain() domain.Item {
	return domain.Item{ID: record.ID, TenantID: record.TenantID, AlertType: record.AlertType, RuleCode: record.RuleCode,
		Severity: record.Severity, TargetRef: record.TargetRef, Title: record.Title, DueDate: record.DueDate,
		Status: record.Status, ClosedAt: record.ClosedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

// GORMRepository persists alert state in the data-analysis database.
type GORMRepository struct{ db *gorm.DB }

// NewGORMRepository constructs the alert persistence adapter.
func NewGORMRepository(db *gorm.DB) *GORMRepository { return &GORMRepository{db: db} }

// ListByTenant reads alerts in the previous severity and due-date order.
func (repository *GORMRepository) ListByTenant(ctx context.Context, tenantID string, limit int) ([]domain.Item, error) {
	var records []alertRecord
	err := repository.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("severity DESC, due_date ASC").Limit(limit).Find(&records).Error
	if err != nil {
		return nil, err
	}
	items := make([]domain.Item, 0, len(records))
	for _, record := range records {
		items = append(items, record.toDomain())
	}
	return items, nil
}

// UpdateStatus changes alert state only when the record belongs to the tenant.
func (repository *GORMRepository) UpdateStatus(ctx context.Context, id, tenantID, status string, updatedAt time.Time, closedAt *time.Time) (bool, error) {
	result := repository.db.WithContext(ctx).Model(&alertRecord{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Updates(map[string]interface{}{"status": status, "updated_at": updatedAt, "closed_at": closedAt})
	return result.RowsAffected > 0, result.Error
}
