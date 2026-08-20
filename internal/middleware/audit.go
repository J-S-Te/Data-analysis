package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// AuditWrites reports every authenticated API write after its final status is known.
// It intentionally excludes request bodies and does not change business outcomes on report failure.
func AuditWrites(reporter platformaudit.Reporter, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if reporter == nil || isReadMethod(c.Request.Method) {
			return
		}
		principal, _ := auth.FromContext(c.Request.Context())
		result := "SUCCESS"
		if c.Writer.Status() >= http.StatusBadRequest {
			result = "FAILURE"
		}
		resourceType, resourceID := auditResource(c)
		event := platformaudit.Event{ActorID: principal.UserID, ActorName: principal.DisplayName, Action: "DATA_ANALYSIS:" + c.Request.Method + ":" + strings.ReplaceAll(strings.Trim(c.FullPath(), "/"), "/", "."), ResourceType: resourceType, ResourceID: resourceID, RequestID: c.GetString("request_id"), CorrelationID: c.GetString("request_id"), Result: result, ReasonCode: strconv.Itoa(c.Writer.Status())}
		if err := reporter.Report(c.Request.Context(), event); err != nil && logger != nil {
			logger.Error("report platform audit", "error", err, "request_id", event.RequestID)
		}
	}
}

func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
func auditResource(c *gin.Context) (string, string) {
	path := c.FullPath()
	switch {
	case strings.Contains(path, "/alerts/"):
		return "ALERT", c.Param("id")
	case strings.Contains(path, "/alert-rules"):
		return "ALERT_RULE", ""
	case strings.Contains(path, "/admin/sources/"):
		return "SYNC_SOURCE", c.Param("id")
	default:
		return "DATA_ANALYSIS", ""
	}
}
