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
		if reporter == nil || (isReadMethod(c.Request.Method) && !sensitiveRead(c.Request.URL.Path)) {
			return
		}
		principal, _ := auth.FromContext(c.Request.Context())
		status := c.Writer.Status()
		resourceType, resourceID := auditResource(c)
		event := platformaudit.Event{ActorID: principal.UserID, ActorName: principal.DisplayName, Action: "DATA_ANALYSIS:" + c.Request.Method + ":" + strings.ReplaceAll(strings.Trim(c.FullPath(), "/"), "/", "."), ResourceType: resourceType, ResourceID: resourceID, RequestID: c.GetString("request_id"), CorrelationID: c.GetString("request_id"), Result: auditResult(status), RiskLevel: auditRiskLevel(c.Request.Method, c.FullPath(), status), ReasonCode: strconv.Itoa(status), UserLoginIP: requestClientIP(c.Request)}
		if err := reporter.Report(c.Request.Context(), event); err != nil && logger != nil {
			logger.Error("report platform audit", "error", err, "request_id", event.RequestID)
		}
	}
}

// auditResult 区分成功、拒绝与失败：401/403 记为 DENIED，便于识别越权/未授权尝试。
func auditResult(status int) string {
	switch {
	case status >= 200 && status < 400:
		return "SUCCESS"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "DENIED"
	default:
		return "FAILURE"
	}
}

// auditRiskLevel 计算粗略风险等级，使敏感/破坏性/被拒绝操作能在审计查询中被筛出。
func auditRiskLevel(method, path string, status int) string {
	if status >= http.StatusInternalServerError {
		return "HIGH"
	}
	lowered := strings.ToLower(path)
	if method == http.MethodDelete || containsAnyAudit(lowered,
		"delete", "approval", "approve", "reject", "sign", "password", "credential",
		"secret", "permission", "role", "authorization", "admin") {
		return "HIGH"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "MEDIUM"
	}
	return "LOW"
}

func containsAnyAudit(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

// sensitiveRead 识别会暴露数据的读操作（下载、导出、嵌入令牌签发）。
func sensitiveRead(path string) bool {
	lowered := strings.ToLower(path)
	return containsAnyAudit(lowered, "embed", "download", "export")
}

// requestClientIP accepts only public addresses from the managed frontend
// proxy. The right-most XFF value is used to avoid trusting a client-supplied
// left-most value, and Docker/private addresses are rejected. Forwarding
// headers are only honoured when the direct peer is the trusted reverse proxy,
// so a directly reachable service cannot have its audit source address spoofed.
func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if trustedProxyPeer(r.RemoteAddr) {
		if ip := publicClientIP(r.Header.Get("X-Real-IP")); ip != nil {
			return ip.String()
		}
		values := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(values) - 1; i >= 0; i-- {
			if ip := publicClientIP(values[i]); ip != nil {
				return ip.String()
			}
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

// trustedProxyPeer reports whether the direct peer address belongs to the
// trusted reverse proxy (loopback or private/link-local Docker gateway).
// A public peer means the service is directly reachable, so client-supplied
// forwarding headers must not be trusted.
func trustedProxyPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
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
