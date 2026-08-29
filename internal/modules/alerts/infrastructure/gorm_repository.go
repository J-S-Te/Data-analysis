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

// SummaryByTenant 在数据库侧完成聚合，避免将租户全部预警加载到应用内存。
func (repository *GORMRepository) SummaryByTenant(ctx context.Context, tenantID string) (domain.Summary, error) {
	var counts struct{ Total, Open, Ack, Closed int64 }
	if err := repository.db.WithContext(ctx).Table("alert_item").Select("COUNT(*) total, COALESCE(SUM(status = 'OPEN'),0) open, COALESCE(SUM(status = 'ACK'),0) ack, COALESCE(SUM(status = 'CLOSED'),0) closed").Where("tenant_id = ?", tenantID).Scan(&counts).Error; err != nil {
		return domain.Summary{}, err
	}
	summary := domain.Summary{Total: counts.Total, Open: counts.Open, Ack: counts.Ack, Closed: counts.Closed, BySeverity: map[string]int64{}, ByType: map[string]int64{}}
	var severity []struct {
		Key   string `gorm:"column:key"`
		Count int64  `gorm:"column:count"`
	}
	if err := repository.db.WithContext(ctx).Table("alert_item").Select("severity key, COUNT(*) count").Where("tenant_id = ?", tenantID).Group("severity").Scan(&severity).Error; err != nil {
		return domain.Summary{}, err
	}
	for _, row := range severity {
		if row.Key != "" {
			summary.BySeverity[row.Key] = row.Count
		}
	}
	var types []struct {
		Key   string `gorm:"column:key"`
		Count int64  `gorm:"column:count"`
	}
	if err := repository.db.WithContext(ctx).Table("alert_item").Select("alert_type key, COUNT(*) count").Where("tenant_id = ?", tenantID).Group("alert_type").Scan(&types).Error; err != nil {
		return domain.Summary{}, err
	}
	for _, row := range types {
		if row.Key != "" {
			summary.ByType[row.Key] = row.Count
		}
	}
	return summary, nil
}

// UpdateStatus changes alert state only when the record belongs to the tenant.
func (repository *GORMRepository) UpdateStatus(ctx context.Context, id, tenantID, status string, updatedAt time.Time, closedAt *time.Time) (bool, error) {
	result := repository.db.WithContext(ctx).Model(&alertRecord{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Updates(map[string]interface{}{"status": status, "updated_at": updatedAt, "closed_at": closedAt})
	return result.RowsAffected > 0, result.Error
}
