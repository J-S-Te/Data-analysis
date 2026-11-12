// Package http 将看板用例适配到既有 Gin API 契约。
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

// dashboardQueryService 是此 HTTP 适配器使用的本地
// 应用边界。它仅属于本模块，防止其他子系统
// 通过导入而耦合到看板用例。
type dashboardQueryService interface {
	Contract(context.Context, string) (domain.ContractSnapshot, error)
	Project(context.Context, string) (domain.ProjectSnapshot, error)
}
type advancedDashboardService interface {
	Contracts(context.Context, string, int, int) ([]domain.ContractDetail, int64, error)
	Trend(context.Context, string, int) ([]domain.TrendPoint, error)
	ContractRegions(context.Context, string) (domain.ContractRegionDistribution, error)
}

// Handler 通过 Gin 暴露看板读取用例，不导入 GORM。
type Handler struct{ service dashboardQueryService }

// NewHandler 构造看板 HTTP 适配器。
func NewHandler(service dashboardQueryService) *Handler { return &Handler{service: service} }

// Contract 返回当前租户的合同看板快照。
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

// Project 返回当前租户的项目看板快照。
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

// ContractRegions 返回签约事实按客户区域×币种的聚合（字典指标 1.3 区域分布）。
// 金额不跨币种合计：多币种租户按行返回，前端不做跨币种相加。
// 参数：c 为当前HTTP上下文。
// 返回值：无；以可用性信封输出区域聚合，非全租户范围明确不可用。
func (handler *Handler) ContractRegions(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	service, ok := handler.service.(advancedDashboardService)
	if !ok {
		response.Error(c, apperror.New(501, "DASHBOARD_REGIONS_UNAVAILABLE", "区域分布暂不可用"))
		return
	}
	distribution, err := service.ContractRegions(c.Request.Context(), principal.TenantID)
	if err != nil {
		response.Error(c, apperror.Wrap(err, 500, "DASHBOARD_REGIONS_FAILED", "区域分布加载失败"))
		return
	}
	if distribution.Items == nil {
		distribution.Items = []domain.ContractRegionTotal{}
	}
	response.OK(c, distribution)
}

// Projects 返回当前租户可信范围内的项目分页明细。
// 参数：c 为带可信主体的 HTTP 上下文，分页参数通过查询字符串提供。
// 返回值：无；响应包含成功投影标记、列表、总数和分页信息。
func (handler *Handler) Projects(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	service, ok := handler.service.(interface {
		Projects(context.Context, string, int, int) (domain.ProjectDetails, error)
	})
	if !ok {
		response.Error(c, apperror.New(501, "PROJECT_DETAIL_UNAVAILABLE", "项目明细暂不可用"))
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
	result, err := service.Projects(c.Request.Context(), principal.TenantID, page, pageSize)
	if err != nil {
		response.Error(c, apperror.Wrap(err, 500, "PROJECT_DETAIL_FAILED", "项目明细加载失败"))
		return
	}
	response.OK(c, result)
}
