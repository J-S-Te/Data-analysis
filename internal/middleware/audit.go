package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
		event := platformaudit.Event{ActorID: principal.UserID, ActorName: principal.DisplayName, Action: "DATA_ANALYSIS:" + c.Request.Method + ":" + strings.ReplaceAll(strings.Trim(c.FullPath(), "/"), "/", "."), ResourceType: resourceType, ResourceID: resourceID, RequestID: c.GetString("request_id"), CorrelationID: c.GetString("request_id"), Result: result, ReasonCode: strconv.Itoa(c.Writer.Status()), UserLoginIP: requestClientIP(c.Request)}
		if err := reporter.Report(c.Request.Context(), event); err != nil && logger != nil {
			logger.Error("report platform audit", "error", err, "request_id", event.RequestID)
		}
	}
}

// requestClientIP accepts only public addresses from the managed frontend
// proxy. The right-most XFF value is used to avoid trusting a client-supplied
// left-most value, and Docker/private addresses are rejected.
func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := publicClientIP(r.Header.Get("X-Real-IP")); ip != nil {
		return ip.String()
	}
	values := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(values) - 1; i >= 0; i-- {
		if ip := publicClientIP(values[i]); ip != nil {
			return ip.String()
		}
	}
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if ip := publicClientIP(remote); ip != nil {
		return ip.String()
	}
	return ""
}

func publicClientIP(value string) *netip.Addr {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return nil
	}
	return &addr
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
