package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// AuditWrites reports every authenticated API write (and sensitive read) after its
// final status is known. It intentionally excludes request bodies.
//
// 安全理由（SEC-D4b）：原实现对审计管道 fail-open —— reporter 为 nil 直接返回，
// 上报失败仅在 logger 非 nil 时记一行（且缺路由），从不拒绝请求；凭据失效或
// ingest 故障时告警确认/规则变更等所有写入都没有审计且不可见。现在：
//  1. 上报失败一律 error 日志（含路由与请求 id）；
//  2. 强制审计模式（PLATFORM_AUDIT_REQUIRED 或 PLATFORM_ENVIRONMENT_CODE=prod）下：
//     - reporter 缺失 => 在执行业务 handler 之前就拒绝请求（503），不产生无审计的写；
//     - 写请求先缓冲响应，Report 成功才提交给客户端；失败则丢弃业务响应并返回
//     503，保证“审计写不进去 ⇒ 客户端拿不到成功”。
//
// 非强制模式保持原有放行语义，仅补上可告警的错误日志。
func AuditWrites(reporter platformaudit.Reporter, logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		// 审计失败必须留下可告警的日志；logger 缺失时退回默认 logger，绝不静默。
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		if isReadMethod(c.Request.Method) && !sensitiveRead(c.Request.URL.Path) {
			c.Next()
			return
		}
		route := c.Request.Method + " " + c.Request.URL.Path
		requestID := c.GetString("request_id")
		if reporter == nil {
			if !auditRequired() {
				// 非强制模式：审计按配置关闭，/readyz 已暴露 audit=disabled。
				c.Next()
				return
			}
			logger.ErrorContext(c.Request.Context(), "audit reporter unavailable, rejecting request",
				"route", route,
				"request_id", requestID,
			)
			c.Abort()
			response.Error(c, apperror.New(http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "审计服务不可用，操作已被拒绝"))
			return
		}
		required := auditRequired()
		origWriter := c.Writer
		var buffered *auditBuffer
		if required && !isReadMethod(c.Request.Method) {
			buffered = newAuditBuffer(origWriter)
			c.Writer = buffered
		}
		// handler panic 展平时先恢复真实 writer，外层 gin.Recovery 才能把 500 写出去；
		// 缓冲中的半成品响应被直接丢弃，不会被当成成功提交。
		defer func() { c.Writer = origWriter }()
		c.Next()
		status := c.Writer.Status()
		if status == 0 {
			status = http.StatusOK
		}
		principal, _ := auth.FromContext(c.Request.Context())
		resourceType, resourceID := auditResource(c)
		event := platformaudit.Event{ActorID: principal.UserID, ActorName: principal.DisplayName, Action: "DATA_ANALYSIS:" + c.Request.Method + ":" + strings.ReplaceAll(strings.Trim(c.FullPath(), "/"), "/", "."), ResourceType: resourceType, ResourceID: resourceID, RequestID: requestID, CorrelationID: requestID, Result: auditResult(status), RiskLevel: auditRiskLevel(c.Request.Method, c.FullPath(), status), ReasonCode: strconv.Itoa(status), UserLoginIP: requestClientIP(c.Request)}
		if err := reporter.Report(c.Request.Context(), event); err != nil {
			logger.ErrorContext(c.Request.Context(), "report platform audit failed",
				"route", route,
				"request_id", event.RequestID,
				"status", status,
				"error", err,
			)
			if required {
				// 强制审计模式：审计事件写入失败必须拒绝该请求，不能静默成功。
				c.Writer = origWriter
				response.Error(c, apperror.New(http.StatusServiceUnavailable, "AUDIT_WRITE_REJECTED", "审计记录写入失败，操作未被确认"))
			}
			return
		}
		if buffered != nil {
			buffered.flushTo(origWriter)
		}
	}
}

// auditRequired 判定是否处于强制审计模式：PLATFORM_AUDIT_REQUIRED 显式设置优先，
// 否则环境码为 prod 时默认强制。与 bootstrap 的 AuditRequired 判定保持同一语义。
func auditRequired() bool {
	raw := strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
	if raw != "" {
		required, err := strconv.ParseBool(raw)
		return err == nil && required
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("PLATFORM_ENVIRONMENT_CODE")), "prod")
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
