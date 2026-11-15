package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// SecurityFailures 必须在 SessionAuth 之前挂载：认证失败会 Abort，后续审计中间件
// 看不到 401/403。已认证的拒绝由 AuditWrites 负责，避免同一请求重复写安全事件。
func SecurityFailures(reporter platformaudit.Reporter, trustedProxies []string) gin.HandlerFunc {
	resolver := NewClientIPResolver(trustedProxies)
	return func(c *gin.Context) {
		c.Next()
		status := c.Writer.Status()
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return
		}
		if _, ok := auth.FromContext(c.Request.Context()); ok {
			return
		}
		securityReporter, ok := reporter.(platformaudit.SecurityReporter)
		if !ok {
			return
		}
		clientIP, ipSource := resolver.Resolve(c.Request)
		requestID := c.GetString("request_id")
		event := platformaudit.SecurityEvent{
			OccurredAt: time.Now().UTC(), Action: "AUTHENTICATION_DENIED",
			Method: c.Request.Method, Path: c.Request.URL.Path,
			ClientIP: clientIP, UserAgent: c.Request.UserAgent(), RequestID: requestID,
			Result: "DENIED", ReasonCode: strconv.Itoa(status), RiskLevel: "HIGH",
			Summary: "data analysis authentication failed; ip_source=" + ipSource,
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
		defer cancel()
		if err := securityReporter.ReportSecurity(ctx, event); err != nil {
			slog.ErrorContext(c.Request.Context(), "report security event failed", "error", err, "request_id", requestID, "status", status)
		}
	}
}
