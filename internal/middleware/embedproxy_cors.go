package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// EmbedProxyCORS 只对明确配置的独立嵌入源回显 CORS，供嵌入 iframe 读取一次性令牌
// 嵌入代理的响应。嵌入代理用 URL 内的一次性令牌认证，不依赖 Cookie，但仍必须
// 限定来源，失败关闭（SEC：S-4）：
//   - 未配置 BI_EMBED_PUBLIC_ORIGIN 或配置非法（非 http(s) 源、带路径/查询等）→ 不设置 ACAO；
//   - 请求 Origin 规范化后必须与配置 origin 完全一致 → 只回显该规范化 origin；
//   - Origin 为字面量 "null"（sandbox 无 allow-same-origin 的不透明源，可能来自截图、
//     日志或共享链接里泄露的令牌）或任何其他外源 → 不设置 ACAO；
//   - 无 Origin 请求保持原状（不回显、不干预）。
//
// 该策略只挂载在 /api/v1/embed-proxy 前缀，绝不放宽到需要会话认证的接口。
func EmbedProxyCORS(embedPublicOrigin string) gin.HandlerFunc {
	allowed := normalizeOrigin(embedPublicOrigin)
	return func(c *gin.Context) {
		// "null" 及非 http(s) 源经 normalizeOrigin 后为空串，永远不会命中回显分支。
		if origin := normalizeOrigin(c.GetHeader("Origin")); allowed != "" && origin == allowed {
			c.Header("Access-Control-Allow-Origin", allowed)
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
