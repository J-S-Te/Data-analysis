// Package middleware 提供会话鉴权中间件（对齐 CRM internal/middleware/auth.go）。
package middleware

import (
	"errors"
	"net/http"
	"net/url"

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
func RequireSameOriginWrite(publicOrigin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		origin, err := url.Parse(c.GetHeader("Origin"))
		expected, expectedErr := url.Parse(publicOrigin)
		if err != nil || expectedErr != nil || origin.Scheme != expected.Scheme || origin.Host != expected.Host || c.GetHeader("X-CSRF-Token") != "1" {
			response.Error(c, apperror.ErrForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
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
