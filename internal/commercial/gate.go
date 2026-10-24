// Package commercial 将业务决策与认证及
// 运维恢复分离。运行时适配器仅在进程启动时安装一次。
package commercial

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type Check func(context.Context, string) error

const ReadHistory = "READ_HISTORY"
const MutateBusiness = "MUTATE_BUSINESS"

func (check Check) Allow(ctx context.Context, operation string) error {
	if check == nil {
		return nil
	} // 只有显式禁用的部署才没有适配器。
	return check(ctx, operation)
}

func Reject(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	response.Error(c, apperror.New(http.StatusForbidden, "COMMERCIAL_LICENSE_DENIED", "商业授权不允许此操作，请联系管理员"))
	c.Abort()
	return true
}

func Middleware(check Check) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasSuffix(c.FullPath(), "/auth/me") {
			c.Next()
			return
		}
		// 未知新路由保持受限：仅使用 GET 不能证明是
		// 历史读取，嵌入授权签发本身也是 GET。
		operation := MutateBusiness
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			for _, suffix := range []string{"/alerts", "/alerts/summary", "/dictionary", "/dictionary/metrics/:code/versions", "/dashboard/contract", "/dashboard/project", "/dashboard/report", "/dashboard/finance", "/dashboard/contracts", "/dashboard/trend", "/admin/sources", "/alert-rules"} {
				if strings.HasSuffix(c.FullPath(), "/api/v1"+suffix) {
					operation = ReadHistory
					break
				}
			}
		}
		if !Reject(c, check.Allow(c.Request.Context(), operation)) {
			c.Next()
		}
	}
}
