package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/embedbridge"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary"
	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// App 是数据看板服务运行时容器，保存当前启动配置、数据库连接与路由/HTTP 服务实例，供主流程统一关闭或复用。
type App struct {
	Config Config
	DB     *gorm.DB
	Router *gin.Engine
	Server *http.Server
}

// New 使用给定 Config 创建数据库连接、OIDC 服务、嵌入桥和 API 路由，返回可运行的 App。
// 参数 config 为必需运行参数；返回 *App 时包含聚合后的配置、DB、Router 与 HTTP Server，返回 error 时表示初始化阶段任一组件装配失败（如数据库 DSN 无效、OIDC 初始化失败）。
func New(config Config) (*App, error) {
	db, err := gorm.Open(mysql.Open(config.MySQLDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return nil, err
	}
	// 表结构由版本化 SQL 迁移创建（migrations/，compose 初始化时执行）；不使用运行时 AutoMigrate。

	ctx := context.Background()
	authService, err := oidc.NewService(ctx, db, oidc.Options{
		Issuer:                  config.OIDCIssuer,
		BackchannelIssuer:       config.OIDCBackchannel,
		ClientID:                config.OIDCClientID,
		ClientSecret:            config.OIDCClientSecret,
		RedirectURL:             config.OIDCRedirectURI,
		TenantID:                config.OIDCTenantID,
		ApplicationCode:         config.AuditApplicationCode,
		EnvironmentCode:         config.AuditEnvironmentCode,
		PathPrefix:              config.PathPrefix,
		SessionTTL:              15 * timeMinute,
		CodecKey:                config.CodecKey,
		AuthorizationContextURL: config.AuthorizationContextURL,
	})
	if err != nil {
		return nil, err
	}
	bridge, err := embedbridge.New(db, embedbridge.Options{
		MetabaseInternalURL: config.MetabaseInternalURL,
		MetabaseBasePath:    config.MetabaseBasePath,
		EmbeddingSecret:     config.MetabaseEmbeddingSecret,
		DashboardIDs:        config.DashboardIDs,
		PathPrefix:          config.PathPrefix,
	})
	if err != nil {
		return nil, err
	}

	router := gin.New()
	router.Use(middleware.RequestID(), gin.Recovery())
	base := router.Group(strings.TrimRight(config.PathPrefix, "/"))
	base.GET("/livez", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	base.GET("/readyz", func(c *gin.Context) {
		sqlDB, pingErr := db.DB()
		if pingErr != nil || sqlDB.PingContext(c.Request.Context()) != nil {
			response.Error(c, errors.New("database unavailable"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	base.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	authHandler := oidc.NewHandler(authService, oidc.HTTPOptions{
		CookieName:   "data_analysis_session",
		PublicOrigin: config.PublicOrigin,
		SecureCookie: config.SecureCookie,
	})
	base.GET("/auth/login", authHandler.Login)
	base.GET("/auth/callback", authHandler.Callback)
	base.GET("/auth/logout", authHandler.Logout)

	api := base.Group("/api/v1", middleware.SessionAuth(authService, "data_analysis_session"))
	api.Use(middleware.AuditWrites(platformaudit.NewReporter(config.PlatformBaseURL, config.AuditClientID, config.AuditClientSecret, config.AuditApplicationCode, config.AuditEnvironmentCode), slog.Default()))
	api.GET("/auth/me", authHandler.AuthMe)

	// 嵌入桥（设计方案 §9）
	api.GET("/embed/:dashboard", func(c *gin.Context) { bridge.Issue(c, c.Param("dashboard")) })
	base.GET("/api/v1/embed-proxy/:token", func(c *gin.Context) { bridge.Proxy(c, c.Param("token")) })
	base.GET("/api/v1/embed-proxy/:token/*resource", func(c *gin.Context) {
		bridge.ProxyResource(c, c.Param("token"), c.Param("resource"))
	})

	// 业务桩（聚合库表就绪后生效）
	alertHandler := alerts.NewHandler(db)
	api.GET("/alerts", middleware.RequirePermission("alert.view"), alertHandler.List)
	api.POST("/alerts/:id/ack", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { alertHandler.UpdateStatus(c, c.Param("id"), "ACK") })
	api.POST("/alerts/:id/close", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { alertHandler.UpdateStatus(c, c.Param("id"), "CLOSED") })

	dictHandler := dictionary.NewHandler()
	api.GET("/dictionary", middleware.RequirePermission("dictionary.view"), dictHandler.Get)

	adminHandler := admin.NewHandler(db)
	dashboardHandler := dashboard.NewHandler(db)
	api.GET("/dashboard/contract", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.Contract)
	api.GET("/dashboard/project", middleware.RequirePermission("dashboard.project.view"), dashboardHandler.Project)
	api.GET("/admin/sources", middleware.RequirePermission("aggregation.manage"), adminHandler.ListSources)
	api.POST("/admin/sources/:id/trigger", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("aggregation.manage"), func(c *gin.Context) { adminHandler.TriggerSource(c, c.Param("id")) })
	api.GET("/alert-rules", middleware.RequirePermission("alert.manage"), adminHandler.ListAlertRules)
	api.PUT("/alert-rules", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), adminHandler.PutAlertRules)

	server := &http.Server{Addr: config.ListenAddr, Handler: router}
	return &App{Config: config, DB: db, Router: router, Server: server}, nil
}

const timeMinute = 60 * 1e9 // ns

var _ = slog.Default
