// Package commercial keeps business decisions distinct from authentication and
// operational recovery. The runtime adapter is installed once at process start.
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
	} // Only the explicitly disabled deployment has no adapter.
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
		// Unknown new routes stay restricted: GET alone is not proof of a
		// historical read (embed grant issuance itself is a GET).
		operation := MutateBusiness
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			for _, suffix := range []string{"/alerts", "/alerts/summary", "/dictionary", "/dictionary/metrics/:code/versions", "/dashboard/contract", "/dashboard/project", "/dashboard/contracts", "/dashboard/trend", "/admin/sources", "/alert-rules"} {
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
