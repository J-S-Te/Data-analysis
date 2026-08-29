// Package http adapts the metric dictionary use case to Gin.
package http

import (
	"github.com/gin-gonic/gin"
	"net/http"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// Handler exposes the read-only metric dictionary.
type Handler struct{ catalog *application.CatalogService }

// NewHandler constructs the dictionary HTTP adapter.
func NewHandler(catalog *application.CatalogService) *Handler { return &Handler{catalog: catalog} }

// Get returns the currently published metric dictionary.
func (handler *Handler) Get(c *gin.Context) {
	p, _ := auth.FromContext(c.Request.Context())
	response.OK(c, handler.catalog.ListForTenant(c.Request.Context(), p.TenantID))
}

// Put 保存当前租户的指标定义，仅 admin 可操作。
func (handler *Handler) Put(c *gin.Context) {
	p, ok := auth.FromContext(c.Request.Context())
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHENTICATED", "message": "未登录"})
		return
	}
	admin := false
	for _, r := range p.Roles {
		if r == "admin" {
			admin = true
		}
	}
	if !admin {
		c.JSON(http.StatusForbidden, gin.H{"code": "FORBIDDEN", "message": "仅管理员可以修改指标字典"})
		return
	}
	var metrics []domain.Metric
	if err := c.ShouldBindJSON(&metrics); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "METRIC_PAYLOAD_INVALID", "message": "指标定义格式无效"})
		return
	}
	if err := handler.catalog.SaveTenantMetrics(c.Request.Context(), p.TenantID, p.UserID, metrics); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "METRIC_SAVE_FAILED", "message": err.Error()})
		return
	}
	handler.Get(c)
}
