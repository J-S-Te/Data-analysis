package bootstrap

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// requireIntegrationSync 在现有权限和许可门禁后拒绝未启用的外部来源管理与手动入队。
// 参数：enabled 为当前环境显式同步模式。
// 返回值：关闭时输出明确未启用错误的中间件；不修改历史状态或既有队列。
func requireIntegrationSync(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled {
			response.Error(c, apperror.New(http.StatusServiceUnavailable, "INTEGRATION_SYNC_DISABLED", "外部业务同步未启用，历史快照保留；标准财务事实仍可独立发布"))
			c.Abort()
			return
		}
		c.Next()
	}
}
