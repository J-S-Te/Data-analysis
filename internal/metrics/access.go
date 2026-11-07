// Package metrics 暴露 dashboard-api 的只读文本指标（C2）。
//
// 手写 Prometheus text exposition（version=0.0.4），不引入 prometheus 客户端依赖：
// 本仓只需要少量 gauge，加一个外部 SDK 会连带引入采集/注册表/中间件等整条依赖链。
//
// 安全口径复用 platform 的 MetricsAccess（SEC-B4）：
//   - 配置 METRICS_ACCESS_TOKEN 时，抓取端必须携带 Authorization: Bearer <token>
//     （常量时间比较），否则 401；
//   - 未配置 token 时只放行 TCP 对端为回环的本机抓取，其余 403（fail-closed）。
//
// 只看连接对端 RemoteAddr，完全不看 X-Forwarded-For：代理头可伪造，
// 采信它等于给公网一个冒充回环的机会。
package metrics

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Path 是 dashboard-api 的指标端点路径。
// 固定挂在服务根路径（不随 APP_PATH_PREFIX 变化）：Prometheus 抓取配置按部署 origin
// 约定路径，前缀化会让同一镜像在不同环境出现不同抓取地址。
const Path = "/metrics"

// RequireAccess 保护 Path（SEC-B4/B2C2）。
// 参数：accessToken 为部署配置的抓取令牌，空表示只允许回环抓取。
// 返回值：gin 中间件；拒绝时分别返回 401（token 不匹配/缺失）与 403（非回环且无 token）。
func RequireAccess(accessToken string) gin.HandlerFunc {
	expected := strings.TrimSpace(accessToken)
	return func(c *gin.Context) {
		if expected != "" {
			token, ok := bearerToken(c.GetHeader("Authorization"))
			// 空 token 直接拒绝：ConstantTimeCompare 对不同长度返回 0，但显式判空
			// 让“没带凭据”与“带错凭据”走同一条明确分支。
			if ok && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
				c.Next()
				return
			}
			c.Abort()
			c.String(http.StatusUnauthorized, "metrics access denied\n")
			return
		}
		if isLoopbackPeer(c.Request.RemoteAddr) {
			c.Next()
			return
		}
		c.Abort()
		c.String(http.StatusForbidden, "metrics access denied\n")
	}
}

// isLoopbackPeer 判断 TCP 对端是否为回环地址。
// 参数：remoteAddr 为 http.Request.RemoteAddr（host:port 或裸 host）。
// 返回值：对端可解析且为回环时为 true；解析失败一律 false（fail-closed）。
func isLoopbackPeer(remoteAddr string) bool {
	peer := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	address := net.ParseIP(strings.Trim(peer, "[]"))
	return address != nil && address.IsLoopback()
}

// bearerToken 解析 Authorization 头里的 Bearer 令牌。
// 参数：header 为原始 Authorization 值。
// 返回值：令牌与是否形如 "Bearer <token>"；不匹配返回 false。
func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", false
	}
	return fields[1], true
}
