package embedbridge

import (
	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
	"net/http"
)

// DisabledIssue 在原生模式校验原许可与看板权限后明确拒绝签发嵌入令牌。
// 参数：c 为会话请求上下文，dashboardCode 为看板代码，check 为商业许可检查。
// 返回值：无；未认证、未授权和关闭能力分别返回原有错误或明确关闭错误。
func DisabledIssue(c *gin.Context, dashboardCode string, check commercial.Check) {
	if commercial.Reject(c, check.Allow(c.Request.Context(), commercial.MutateBusiness)) {
		return
	}
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	permission, exists := permissionFor(dashboardCode)
	if !exists || !principal.HasPermission(permission) {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	rejectDisabled(c)
}

// DisabledProxy 在原生模式拒绝全部历史嵌入能力且保留许可与会话校验。
// 参数：c 为请求上下文，check 为商业许可检查。
// 返回值：无；不访问令牌库或 Metabase。
func DisabledProxy(c *gin.Context, check commercial.Check) {
	if commercial.Reject(c, check.Allow(c.Request.Context(), commercial.MutateBusiness)) {
		return
	}
	if _, ok := auth.FromContext(c.Request.Context()); !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	rejectDisabled(c)
}

// rejectDisabled 输出明确关闭状态，区别于上游暂时不可达。
// 参数：c 为当前请求上下文。
// 返回值：无；返回不含嵌入凭据的服务不可用错误。
func rejectDisabled(c *gin.Context) {
	response.Error(c, apperror.New(http.StatusServiceUnavailable, "DASHBOARD_EMBED_DISABLED", "当前使用原生分析模式，未启用嵌入看板"))
}
