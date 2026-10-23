// Package infrastructure 包含告警模块拥有的 GORM 适配器。
package infrastructure

import (
	"context"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

type alertRecord struct {
	ID         string  `gorm:"column:id;primaryKey"`
	TenantID   string  `gorm:"column:tenant_id"`
	AlertType  string  `gorm:"column:alert_type"`
	RuleCode   string  `gorm:"column:rule_code"`
	Severity   string  `gorm:"column:severity"`
	OwnerOrgID string  `gorm:"column:owner_org_id"`
	TargetRef  string  `gorm:"column:target_ref"`
	Title      string  `gorm:"column:title"`
	DueDate    *string `gorm:"column:due_date"`
	Status     string  `gorm:"column:status"`
	ClosedAt   *string `gorm:"column:closed_at"`
	CreatedAt  string  `gorm:"column:created_at"`
	UpdatedAt  string  `gorm:"column:updated_at"`
}

func (alertRecord) TableName() string { return "alert_item" }

// applyAlertScope 添加行级边界。空或未知模式均
// 安全拒绝；只有显式 TENANT 范围可读取整个租户。
func applyAlertScope(db *gorm.DB, scope application.AlertScope) *gorm.DB {
	switch strings.ToUpper(strings.TrimSpace(scope.Mode)) {
	case "TENANT":
		return db
	case "ORG", "TEAM":
		seen := map[string]struct{}{}
		ids := make([]string, 0, len(scope.OrgUnitIDs))
		for _, raw := range scope.OrgUnitIDs {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return db.Where("1 = 0")
		}
		return db.Where("owner_org_id IN ?", ids)
	default:
		return db.Where("1 = 0")
	}
}

func (record alertRecord) toDomain() domain.Item {
	return domain.Item{ID: record.ID, TenantID: record.TenantID, AlertType: record.AlertType, RuleCode: record.RuleCode,
		Severity: record.Severity, TargetRef: record.TargetRef, Title: record.Title, DueDate: record.DueDate,
		Status: record.Status, ClosedAt: record.ClosedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

// GORMRepository 将告警状态持久化到数据分析数据库。
type GORMRepository struct{ db *gorm.DB }

// NewGORMRepository 构造告警持久化适配器。
func NewGORMRepository(db *gorm.DB) *GORMRepository { return &GORMRepository{db: db} }

// ListByTenant 读取租户的一页，严重性按业务等级
// 排序（CRITICAL > HIGH > MEDIUM > LOW），而非列值字母顺序。
func (repository *GORMRepository) ListByTenant(ctx context.Context, tenantID string, query application.ListQuery) (application.ListResult, error) {
	result := application.ListResult{Items: []domain.Item{}, Page: query.Page, PageSize: query.PageSize}
	db := applyAlertScope(repository.db.WithContext(ctx).Table("alert_item").Where("tenant_id = ?", tenantID), query.Scope)
	if query.Severity != "" {
		db = db.Where("severity = ?", query.Severity)
	}
	if query.Status != "" {
		db = db.Where("status = ?", query.Status)
	}
	if query.Keyword != "" {
		like := "%" + query.Keyword + "%"
		db = db.Where("(title LIKE ? OR alert_type LIKE ? OR rule_code LIKE ? OR target_ref LIKE ?)", like, like, like, like)
	}
	if err := db.Count(&result.Total).Error; err != nil {
		return result, err
	}
	var records []alertRecord
	err := db.Select("*").
		Order("CASE severity WHEN 'CRITICAL' THEN 0 WHEN 'HIGH' THEN 1 WHEN 'MEDIUM' THEN 2 WHEN 'LOW' THEN 3 ELSE 4 END, due_date ASC, id ASC").
		Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&records).Error
	if err != nil {
		return result, err
	}
	for _, record := range records {
		result.Items = append(result.Items, record.toDomain())
	}
	return result, nil
}

// SummaryByTenant 在数据库侧完成聚合，避免将租户全部预警加载到应用内存。
func (repository *GORMRepository) SummaryByTenant(ctx context.Context, tenantID string, scope application.AlertScope) (domain.Summary, error) {
	base := func() *gorm.DB {
		return applyAlertScope(repository.db.WithContext(ctx).Table("alert_item").Where("tenant_id = ?", tenantID), scope)
	}
	var counts struct{ Total, Open, Ack, Closed int64 }
	if err := base().Select("COUNT(*) total, COALESCE(SUM(status = 'OPEN'),0) open, COALESCE(SUM(status = 'ACK'),0) ack, COALESCE(SUM(status = 'CLOSED'),0) closed").Scan(&counts).Error; err != nil {
		return domain.Summary{}, err
	}
	summary := domain.Summary{Total: counts.Total, Open: counts.Open, Ack: counts.Ack, Closed: counts.Closed, BySeverity: map[string]int64{}, ByType: map[string]int64{}}
	var severity []struct {
		Key   string `gorm:"column:group_value"`
		Count int64  `gorm:"column:count"`
	}
	if err := base().Select("severity AS group_value, COUNT(*) AS count").Group("severity").Scan(&severity).Error; err != nil {
		return domain.Summary{}, err
	}
	for _, row := range severity {
		if row.Key != "" {
			summary.BySeverity[row.Key] = row.Count
		}
	}
	var types []struct {
		Key   string `gorm:"column:group_value"`
		Count int64  `gorm:"column:count"`
	}
	if err := base().Select("alert_type AS group_value, COUNT(*) AS count").Group("alert_type").Scan(&types).Error; err != nil {
		return domain.Summary{}, err
	}
	for _, row := range types {
		if row.Key != "" {
			summary.ByType[row.Key] = row.Count
		}
	}
	return summary, nil
}

// UpdateStatus 仅在记录属于该租户时变更告警状态。
//
// AUD-IP-2026 / 批次 2：状态变更与审计事件在同一事务提交；未命中记录时回滚，
// 不会留下"没改到任何告警却记了一条审计"的事件。
func (repository *GORMRepository) UpdateStatus(ctx context.Context, id, tenantID, status string, scope application.AlertScope, updatedAt time.Time, closedAt *time.Time) (bool, error) {
	var changed bool
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := applyAlertScope(tx.Model(&alertRecord{}).
			Where("id = ? AND tenant_id = ?", id, tenantID), scope).
			Updates(map[string]interface{}{"status": status, "updated_at": updatedAt, "closed_at": closedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		changed = true
		return auditoutbox.Append(tx, auditoutbox.Entry{
			TenantID: tenantID, Action: "DATA_ANALYSIS:ALERT_" + status,
			ResourceType: "alert", ResourceID: id, Result: "SUCCESS",
		})
	})
	return changed, err
}
