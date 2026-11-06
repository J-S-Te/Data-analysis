package bootstrap

import (
	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/embedbridge"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"gorm.io/gorm"
)

// newDashboardEmbedBridge 仅在启用模式构造嵌入桥，原生模式不读取嵌入凭据。
// 参数：db 为令牌存储，config 为服务配置，check 为原商业许可检查。
// 返回值：启用时返回桥或配置错误，关闭时返回空桥与空错误。
func newDashboardEmbedBridge(db *gorm.DB, config Config, check commercial.Check) (*embedbridge.Bridge, error) {
	if !config.DashboardEmbedEnabled {
		return nil, nil
	}
	return embedbridge.New(db, embedbridge.Options{
		LicenseCheck: check, MetabaseInternalURL: config.MetabaseInternalURL,
		MetabaseBasePath: config.MetabaseBasePath, EmbeddingSecret: config.MetabaseEmbeddingSecret,
		DashboardIDs: config.DashboardIDs, PathPrefix: config.PathPrefix,
		PublicOrigin: config.PublicOrigin, EmbedPublicOrigin: config.EmbedPublicOrigin,
	})
}

// mountDashboardEmbedRoutes 挂载嵌入接口并在原生模式保留认证及许可门禁。
// 参数：api 为会话与许可保护组，base 为服务根组，bridge 为可选桥，config 为配置，check 为许可检查。
// 返回值：无；关闭时不接受历史嵌入令牌，也不访问存储或上游。
func mountDashboardEmbedRoutes(api, base *gin.RouterGroup, bridge *embedbridge.Bridge, config Config, check commercial.Check) {
	if !config.DashboardEmbedEnabled {
		api.GET("/embed/:dashboard", func(c *gin.Context) { embedbridge.DisabledIssue(c, c.Param("dashboard"), check) })
		api.GET("/embed-proxy/:token", func(c *gin.Context) { embedbridge.DisabledProxy(c, check) })
		api.GET("/embed-proxy/:token/*resource", func(c *gin.Context) { embedbridge.DisabledProxy(c, check) })
		return
	}
	api.GET("/embed/:dashboard", func(c *gin.Context) { bridge.Issue(c, c.Param("dashboard")) })
	// 启用模式以短期能力为凭证，CORS 仅对明确配置的独立嵌入来源回显。
	proxy := base.Group("/api/v1/embed-proxy", middleware.EmbedProxyCORS(config.EmbedPublicOrigin))
	proxy.GET("/:token", func(c *gin.Context) { bridge.Proxy(c, c.Param("token")) })
	proxy.GET("/:token/*resource", func(c *gin.Context) { bridge.ProxyResource(c, c.Param("token"), c.Param("resource")) })
}
