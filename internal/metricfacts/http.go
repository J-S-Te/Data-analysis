package metricfacts

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// Handler 在既有会话、商业许可和审计边界内读取新增经营指标。
type Handler struct {
	Store              *Store
	IntegrationEnabled bool
}

// Register 注册只读指标查询，具体分区权限在处理器再次验证。
// 参数：api为已装配认证与许可中间件的路由组。
// 返回值：无。
func (h *Handler) Register(api *gin.RouterGroup) { api.GET("/dashboard/metrics", h.Get) }

// metricQuery 严格解析分区和月份，拒绝重复参数及浏览器自报租户。
// 参数：c为当前HTTP查询。
// 返回值：分区、可空月份或参数错误。
func metricQuery(c *gin.Context) (string, string, error) {
	query := c.Request.URL.Query()
	for key, values := range query {
		if (key != "section" && key != "period") || len(values) != 1 {
			return "", "", ErrInvalid
		}
	}
	section, period := query.Get("section"), query.Get("period")
	if SourceFor(section) == "" || period != "" && !ValidPeriod(period) {
		return "", "", ErrInvalid
	}
	if period == "" && section != "contract" {
		period = time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01")
	}
	return section, period, nil
}

// Get 使用可信主体的分区权限和数据范围读取安全汇总。
// 参数：c为已认证请求，不接收客户端范围或操作者声明。
// 返回值：无；错误使用既有中文错误模型且不泄漏源响应。
func (h *Handler) Get(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok || principal.TenantID == "" || principal.UserID == "" {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	section, period, err := metricQuery(c)
	if err != nil {
		response.Error(c, apperror.New(400, "METRIC_FACTS_INVALID_QUERY", "请选择有效看板分区与月份"))
		return
	}
	if !principal.HasPermission("dashboard." + section + ".view") {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	scope := Scope{Mode: principal.ScopeMode, UserID: principal.UserID,
		OrganizationIDs: principal.OrganizationIDs, TeamIDs: principal.TeamIDs}
	if err := validateScope(scope); err != nil {
		response.Error(c, apperror.New(403, "METRIC_FACTS_SCOPE_DENIED", "缺少明确的数据范围"))
		return
	}
	out, err := h.Store.Read(c.Request.Context(), principal.TenantID, section, period, scope)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrScope) {
			status = http.StatusForbidden
		}
		response.Error(c, apperror.New(status, "METRIC_FACTS_UNAVAILABLE", "指标来源暂不可读，请稍后重试"))
		return
	}
	if !h.IntegrationEnabled {
		out.Stale, out.StaleReason = true, "业务集成同步已停用"
	}
	response.OK(c, out)
}
