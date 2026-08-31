// Package http adapts dashboard use cases to the existing Gin API contract.
package http

import (
	"context"
	"errors"
	"fmt"
	stdhttp "net/http"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// dashboardQueryService is the local application boundary consumed by this
// HTTP adapter. It remains module-local so other subsystems cannot couple to
// dashboard use cases by importing it.
type dashboardQueryService interface {
	Contract(context.Context, string) (domain.ContractSnapshot, error)
	Project(context.Context, string) (domain.ProjectSnapshot, error)
}
type advancedDashboardService interface {
	Contracts(context.Context, string, int, int) ([]domain.ContractDetail, int64, error)
	Trend(context.Context, string, int) ([]domain.TrendPoint, error)
}

// Handler exposes dashboard read use cases through Gin without importing GORM.
type Handler struct{ service dashboardQueryService }

// NewHandler constructs the dashboard HTTP adapter.
func NewHandler(service dashboardQueryService) *Handler { return &Handler{service: service} }

// Contract returns the current tenant's contract dashboard snapshot.
func (handler *Handler) Contract(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	snapshot, err := handler.service.Contract(c.Request.Context(), principal.TenantID)
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "CONTRACT_DASHBOARD_FAILED", "failed to load contract dashboard"))
		return
	}
	response.OK(c, snapshot)
}

// Project returns the current tenant's project dashboard snapshot.
func (handler *Handler) Project(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	snapshot, err := handler.service.Project(c.Request.Context(), principal.TenantID)
	if err != nil {
		if errors.Is(err, application.ErrInvalidProjectSnapshot) {
			response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "PROJECT_DASHBOARD_INVALID", "invalid project dashboard snapshot"))
			return
		}
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "PROJECT_DASHBOARD_FAILED", "failed to load project dashboard"))
		return
	}
	response.OK(c, snapshot)
}

// Contracts 返回合同下钻分页数据。
func (handler *Handler) Contracts(c *gin.Context) {
	p, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	service, ok := handler.service.(advancedDashboardService)
	if !ok {
		response.Error(c, apperror.New(stdhttp.StatusNotImplemented, "DASHBOARD_DETAIL_UNAVAILABLE", "合同明细暂不可用"))
		return
	}
	var page, pageSize int
	_, _ = fmt.Sscan(c.DefaultQuery("page", "1"), &page)
	_, _ = fmt.Sscan(c.DefaultQuery("page_size", "20"), &pageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	items, total, err := service.Contracts(c.Request.Context(), p.TenantID, page, pageSize)
	if err != nil {
		response.Error(c, apperror.Wrap(err, 500, "CONTRACT_DETAIL_FAILED", "合同明细加载失败"))
		return
	}
	response.OK(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// Trend 返回月度趋势数据。
func (handler *Handler) Trend(c *gin.Context) {
	p, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	service, ok := handler.service.(advancedDashboardService)
	if !ok {
		response.Error(c, apperror.New(501, "DASHBOARD_TREND_UNAVAILABLE", "趋势数据暂不可用"))
		return
	}
	points, err := service.Trend(c.Request.Context(), p.TenantID, 12)
	if err != nil {
		response.Error(c, apperror.Wrap(err, 500, "DASHBOARD_TREND_FAILED", "趋势加载失败"))
		return
	}
	response.OK(c, points)
}
