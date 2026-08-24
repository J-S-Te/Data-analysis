package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// EmbedProxyCORS 允许 sandbox 无 allow-same-origin（opaque origin）的 iframe 读取
// 一次性令牌嵌入代理的响应。嵌入代理用 URL 内的一次性令牌认证，不依赖 Cookie，
// 因此允许 opaque origin（序列化为字面量 "null"）不会引入越权；该策略只挂载在
// /api/v1/embed-proxy 前缀，绝不放宽到需要会话认证的接口。
func EmbedProxyCORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Origin") == "null" {
			c.Header("Access-Control-Allow-Origin", "null")
			c.Header("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
