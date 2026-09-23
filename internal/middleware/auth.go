// Package middleware 提供会话鉴权中间件（对齐 CRM internal/middleware/auth.go）。
package middleware

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// SessionAuth 只信任 Cookie 对应的服务端会话；请求头中的用户/角色声明不参与认证。
func SessionAuth(authenticator auth.Authenticator, cookieName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Request.Cookie(cookieName)
		if err != nil || cookie.Value == "" {
			response.Error(c, apperror.ErrUnauthenticated)
			c.Abort()
			return
		}
		principal, err := authenticator.Authenticate(c.Request.Context(), cookie.Value)
		if err != nil {
			response.Error(c, apperror.ErrUnauthenticated)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	}
}

// RequirePermission 权限守卫（manifest EXACT 匹配）。
func RequirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := auth.FromContext(c.Request.Context())
		if !ok {
			response.Error(c, apperror.ErrUnauthenticated)
			c.Abort()
			return
		}
		if !principal.HasPermission(permission) {
			response.Error(c, apperror.ErrForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireSameOriginWrite Cookie 写请求必须同源且带 CSRF 头。
//
// 安全理由（SEC-D11）：前端 X-CSRF-Token 恒为 "1"，没有会话熵，防护完全依赖
// 本中间件的来源判定，因此必须失败关闭：
//   - PublicOrigin 为空或非法（配置缺失）→ 拒绝全部不安全方法。原来的 url.Parse
//     比较会让"缺失的 Origin 头"与空基准相等而放行任何跨站写请求；
//   - Origin 必须存在、可解析为 http(s) 源且与 PublicOrigin 规范化后完全一致；
//   - Sec-Fetch-Site: cross-site 直接拒绝（纵深防御，不能用于放行缺失 Origin）。
func RequireSameOriginWrite(publicOrigin string) gin.HandlerFunc {
	expected := normalizeOrigin(publicOrigin)
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		// 失败关闭：没有可信来源基准（PublicOrigin 为空/非法）时宁可拒绝全部写请求。
		origin := normalizeOrigin(c.GetHeader("Origin"))
		if expected == "" || origin == "" || origin != expected || c.GetHeader("X-CSRF-Token") != "1" || strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") {
			response.Error(c, apperror.ErrForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

// normalizeOrigin 将 URL 规范化为 "scheme://host"（小写）；非 http(s)、缺 host、
// 带用户信息/查询/片段或带非根路径的值一律视为非法（返回空串）。
func normalizeOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

// RequestID 为请求注入追踪号。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = randomHex(16)
		}
		c.Set("request_id", id)
		c.Writer.Header().Set("X-Request-ID", id)
		c.Next()
	}
}

func randomHex(size int) string {
	const alphabet = "0123456789abcdef"
	raw := make([]byte, size*2)
	seed := uint64(0)
	for i := range raw {
		if i%8 == 0 {
			seed = uint64(timeNowNanos())
		}
		seed = seed*6364136223846793005 + 1442695040888963407
		raw[i] = alphabet[seed>>56&0x0F]
	}
	return string(raw)
}

func timeNowNanos() int64 { return nowNanos() }

var _ = errors.New // keep errors import for future use
