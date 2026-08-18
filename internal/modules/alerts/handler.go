// Package alerts 预警中心 API（设计方案 §9；骨架：读聚合库 alert_item，行级范围过滤 TODO）。
package alerts

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type AlertItem struct {
	ID        string  `gorm:"column:id;primaryKey" json:"id"`
	TenantID  string  `gorm:"column:tenant_id" json:"tenant_id"`
	AlertType string  `gorm:"column:alert_type" json:"alert_type"`
	RuleCode  string  `gorm:"column:rule_code" json:"rule_code"`
	Severity  string  `gorm:"column:severity" json:"severity"`
	TargetRef string  `gorm:"column:target_ref" json:"target_ref"`
	Title     string  `gorm:"column:title" json:"title"`
	DueDate   *string `gorm:"column:due_date" json:"due_date"`
	Status    string  `gorm:"column:status" json:"status"`
	ClosedAt  *string `gorm:"column:closed_at" json:"closed_at"`
	CreatedAt string  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt string  `gorm:"column:updated_at" json:"updated_at"`
}

func (AlertItem) TableName() string { return "alert_item" }

// Store 隔离 HTTP 层与数据库实现，便于对路由的认证、授权和响应契约做独立测试。
type Store interface {
	ListByTenant(ctx context.Context, tenantID string, limit int) ([]AlertItem, error)
	UpdateStatus(ctx context.Context, id, tenantID, status string, updatedAt time.Time, closedAt *time.Time) (bool, error)
}

type gormStore struct{ db *gorm.DB }

func (s *gormStore) ListByTenant(ctx context.Context, tenantID string, limit int) ([]AlertItem, error) {
	var items []AlertItem
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("severity DESC, due_date ASC").
		Limit(limit).
		Find(&items).Error
	return items, err
}

func (s *gormStore) UpdateStatus(
	ctx context.Context,
	id, tenantID, status string,
	updatedAt time.Time,
	closedAt *time.Time,
) (bool, error) {
	updates := map[string]interface{}{
		"status":     status,
		"updated_at": updatedAt,
		"closed_at":  closedAt,
	}
	result := s.db.WithContext(ctx).
		Model(&AlertItem{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Updates(updates)
	return result.RowsAffected > 0, result.Error
}

type Handler struct{ store Store }

func NewHandler(db *gorm.DB) *Handler { return NewHandlerWithStore(&gormStore{db: db}) }

// NewHandlerWithStore 装配可替换存储的 Handler，生产环境由 NewHandler 使用 GORM 实现。
func NewHandlerWithStore(store Store) *Handler { return &Handler{store: store} }

// List 返回预警列表（范围过滤：骨架仅按 tenant；ORG_SUBTREE/TEAM 待 OQ-D6）。
func (h *Handler) List(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	items, err := h.store.ListByTenant(c.Request.Context(), principal.TenantID, 200)
	if err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "ALERT_LIST_FAILED", "failed to list alerts"))
		return
	}
	response.OK(c, items)
}

// UpdateStatus ack/close 预警（alert.manage；写入审计 TODO）。
func (h *Handler) UpdateStatus(c *gin.Context, id string, status string) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	now := timeNow()
	var closedAt *time.Time
	if status == "CLOSED" {
		closedAt = &now
	}
	updated, err := h.store.UpdateStatus(c.Request.Context(), id, principal.TenantID, status, now, closedAt)
	if err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "ALERT_UPDATE_FAILED", "failed to update alert"))
		return
	}
	if !updated {
		response.Error(c, apperror.New(http.StatusNotFound, "ALERT_NOT_FOUND", "预警不存在"))
		return
	}
	response.OK(c, gin.H{"id": id, "status": status})
}
