package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/embedbridge"
	"github.com/unified-identity-auth-platform/data-analysis/internal/importreport"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	adminapplication "github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	admininfrastructure "github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/infrastructure"
	adminhttp "github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/interfaces/http"
	alertsapplication "github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	alertsinfrastructure "github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/infrastructure"
	alertshttp "github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/interfaces/http"
	dashboardapplication "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	dashboardinfrastructure "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/infrastructure"
	dashboardhttp "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/interfaces/http"
	dictionaryapplication "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	dictionaryhttp "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/interfaces/http"
	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformcatalog"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// App 是数据看板服务运行时容器，保存当前启动配置、数据库连接与路由/HTTP 服务实例，供主流程统一关闭或复用。
type App struct {
	StopLicense context.CancelFunc
	Config      Config
	DB          *gorm.DB
	Router      *gin.Engine
	Server      *http.Server
}

// New 使用给定 Config 创建数据库连接、OIDC 服务、嵌入桥和 API 路由，返回可运行的 App。
// 参数：config 为数据库、认证、许可和机器信任绑定的运行配置。
// 返回值：可运行的App；配置或初始化失败返回错误。
func New(config Config) (*App, error) {
	// 业务日时区先于任何 I/O 校验：非法值必须让启动失败，而不是回退 UTC 改变到期口径。
	dashboardLocation, err := LoadDashboardLocation(config.DashboardTimezone)
	if err != nil {
		return nil, err
	}
	reportVerifier, err := reportStatusVerifier(config)
	if err != nil {
		return nil, err
	}
	licenseContext, stopLicense := context.WithCancel(context.Background())
	initialized := false
	defer func() {
		if !initialized {
			stopLicense()
		}
	}()
	licenseCheck, err := commercial.Start(licenseContext)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(mysql.Open(config.MySQLDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return nil, err
	}
	// 表结构由版本化 SQL 迁移创建（migrations/，compose 初始化时执行）；不使用运行时 AutoMigrate。

	ctx := context.Background()
	// 审批中口径的启动期硬校验：instances 依赖 contract_approval 通道首次成功。
	// 通道尚未成功时启动失败（而不是运行期静默返回 0）：先迁移并跑通审批投影通道，
	// 再切 DASHBOARD_APPROVAL_SOURCE。
	if err := dashboardinfrastructure.ValidateApprovalSource(ctx, db, config.OIDCTenantID, config.DashboardApprovalSource); err != nil {
		return nil, err
	}
	localCatalog := platformcatalog.DataAnalysisManifest()
	localRoleConfigHash, err := platformcatalog.ClaimsRoleConfigHash(localCatalog)
	if err != nil {
		return nil, err
	}
	auditReporter := platformaudit.NewReporter(config.PlatformBaseURL, config.AuditClientID, config.AuditClientSecret, config.AuditApplicationCode, config.AuditEnvironmentCode)
	// 批次 2：事务内审计 outbox 的投递循环；平台不可用时事件留在本地重试。
	if auditReporter != nil {
		auditDispatcher := &auditoutbox.Dispatcher{
			Store:    &auditoutbox.GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: 20},
			Reporter: auditReporter, WorkerID: "data-analysis-api-audit", Poll: 2 * time.Second,
			BatchSize: 50, MaxAttempts: 20, MaxBackoff: 5 * time.Minute, Logger: slog.Default(),
		}
		go auditDispatcher.Run(ctx)
	}
	authService, err := oidc.NewService(ctx, db, oidc.Options{
		Issuer:                  config.OIDCIssuer,
		BackchannelIssuer:       config.OIDCBackchannel,
		ClientID:                config.OIDCClientID,
		ClientSecret:            config.OIDCClientSecret,
		RedirectURL:             config.OIDCRedirectURI,
		TenantID:                config.OIDCTenantID,
		ApplicationCode:         config.AuditApplicationCode,
		EnvironmentCode:         config.AuditEnvironmentCode,
		CatalogVersion:          localCatalog.Version,
		RoleConfigHash:          localRoleConfigHash,
		PathPrefix:              config.PathPrefix,
		SessionIdleTTL:          config.OIDCSessionIdleTTL,
		SessionAbsoluteTTL:      config.OIDCSessionAbsoluteTTL,
		CodecKey:                config.CodecKey,
		AuthorizationContextURL: config.AuthorizationContextURL,
		Reporter:                auditReporter,
	})
	if err != nil {
		return nil, err
	}
	bridge, err := embedbridge.New(db, embedbridge.Options{
		LicenseCheck:        licenseCheck,
		MetabaseInternalURL: config.MetabaseInternalURL,
		MetabaseBasePath:    config.MetabaseBasePath,
		EmbeddingSecret:     config.MetabaseEmbeddingSecret,
		DashboardIDs:        config.DashboardIDs,
		PathPrefix:          config.PathPrefix,
		PublicOrigin:        config.PublicOrigin,
		EmbedPublicOrigin:   config.EmbedPublicOrigin,
	})
	if err != nil {
		return nil, err
	}

	router := gin.New()
	router.Use(middleware.RequestID(), gin.Recovery())
	base := router.Group(strings.TrimRight(config.PathPrefix, "/"))
	if reportVerifier != nil {
		status := importreport.StatusHandler(db, reportVerifier, func(ctx context.Context) error { return licenseCheck.Allow(ctx, commercial.ReadHistory) })
		// 合同系统把 CONTRACT_IMPORT_REPORT_BASE_URL 当作服务 origin，固定请求
		// /internal/v1/contract-imports/report-status（不带 APP_PATH_PREFIX）。
		// 只保留这一个权威挂载；再挂 base 前缀会让同一机器端点出现两个路径，
		// 与实际调用契约漂移，并绕过按组/前缀设计的访问控制与审计规则。
		mountInternalReportStatus(router, status)
	}
	base.GET("/livez", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	base.GET("/readyz", func(c *gin.Context) {
		sqlDB, pingErr := db.DB()
		if pingErr != nil || sqlDB.PingContext(c.Request.Context()) != nil {
			response.Error(c, errors.New("database unavailable"))
			return
		}
		if config.AuditRequired && auditReporter == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "audit": "disabled"})
			return
		}
		auditStatus := "disabled"
		if auditReporter != nil {
			auditStatus = "enabled"
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "audit": auditStatus})
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
	base.POST("/auth/backchannel-logout", authService.BackchannelLogout)

	api := base.Group("/api/v1", middleware.SessionAuth(authService, "data_analysis_session"))
	api.Use(middleware.AuditWrites(auditReporter, slog.Default(), config.TrustedProxies))
	// 组级同源兜底（SEC-D11）：cookie 会话下的所有不安全方法先过 Origin/CSRF 校验，
	// 即使个别路由忘记单独挂载 RequireSameOriginWrite 也默认受保护；
	// PublicOrigin 为空时该中间件失败关闭，拒绝全部写请求。
	api.Use(middleware.RequireSameOriginWrite(config.PublicOrigin))
	api.Use(commercial.Middleware(licenseCheck))
	api.GET("/auth/me", authHandler.AuthMe)
	// 浏览器显式活动续期：组级 RequireSameOriginWrite 已覆盖该 POST 的 Origin/CSRF 校验，
	// commercial 白名单与审计豁免分别在 gate.go / audit.go 中按 /auth/me 同级处理。
	api.POST("/auth/activity", authHandler.Activity)

	// 嵌入桥（设计方案 §9）
	api.GET("/embed/:dashboard", func(c *gin.Context) { bridge.Issue(c, c.Param("dashboard")) })
	// embed-proxy 以 URL 内的一次性令牌为唯一凭证；CORS 只对 BI_EMBED_PUBLIC_ORIGIN
	// 明确配置的独立嵌入源回显，不透明源（Origin: null）与其他外源一律不设置 ACAO，
	// 且仅作用于该前缀，会话接口不受影响。
	embedProxy := base.Group("/api/v1/embed-proxy", middleware.EmbedProxyCORS(config.EmbedPublicOrigin))
	embedProxy.GET("/:token", func(c *gin.Context) { bridge.Proxy(c, c.Param("token")) })
	embedProxy.GET("/:token/*resource", func(c *gin.Context) {
		bridge.ProxyResource(c, c.Param("token"), c.Param("resource"))
	})

	// 业务桩（聚合库表就绪后生效）
	alertHandler := alertshttp.NewHandler(alertsapplication.NewService(alertsinfrastructure.NewGORMRepository(db)))
	api.GET("/alerts", middleware.RequirePermission("alert.view"), alertHandler.List)
	api.GET("/alerts/summary", middleware.RequirePermission("alert.view"), alertHandler.Summary)
	api.POST("/alerts/:id/ack", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { alertHandler.UpdateStatus(c, c.Param("id"), "ACK") })
	api.POST("/alerts/:id/close", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { alertHandler.UpdateStatus(c, c.Param("id"), "CLOSED") })

	dictHandler := dictionaryhttp.NewHandler(dictionaryapplication.NewCatalogService(db))
	api.GET("/dictionary", middleware.RequirePermission("dictionary.view"), dictHandler.Get)
	api.PUT("/dictionary", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("dictionary.manage"), dictHandler.Put)
	api.POST("/dictionary/metrics", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("dictionary.manage"), dictHandler.Create)
	api.PUT("/dictionary/metrics/:code", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("dictionary.manage"), dictHandler.Update)
	api.PATCH("/dictionary/metrics/:code/enabled", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("dictionary.manage"), dictHandler.SetEnabled)
	api.DELETE("/dictionary/metrics/:code", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("dictionary.manage"), dictHandler.Delete)
	api.GET("/dictionary/metrics/:code/versions", middleware.RequirePermission("dictionary.view"), dictHandler.History)

	adminHandler := adminhttp.NewHandler(adminapplication.NewService(admininfrastructure.NewGORMRepository(db)))
	dashboardHandler := dashboardhttp.NewHandler(dashboardapplication.NewService(dashboardinfrastructure.NewGORMRepositoryWithApprovalSource(db, dashboardLocation, config.DashboardApprovalSource)))
	api.GET("/dashboard/contract-import-history", middleware.RequirePermission("dashboard.contract.view"), importreport.HistoryHandler(db))
	api.GET("/dashboard/contract", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.Contract)
	api.GET("/dashboard/project", middleware.RequirePermission("dashboard.project.view"), dashboardHandler.Project)
	api.GET("/dashboard/report", middleware.RequirePermission("dashboard.report.view"), dashboardHandler.Business("report"))
	api.GET("/dashboard/finance", middleware.RequirePermission("dashboard.finance.view"), dashboardHandler.Business("finance"))
	api.GET("/dashboard/contracts", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.Contracts)
	api.GET("/dashboard/trend", middleware.RequirePermission("dashboard.overview.view"), dashboardHandler.Trend)
	api.GET("/admin/sources", middleware.RequirePermission("aggregation.manage"), adminHandler.ListSources)
	api.POST("/admin/sources/:id/trigger", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("aggregation.manage"), func(c *gin.Context) { adminHandler.TriggerSource(c, c.Param("id")) })
	api.POST("/admin/sources/trigger-all", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("aggregation.manage"), adminHandler.TriggerAllSources)
	api.GET("/alert-rules", middleware.RequirePermission("alert.manage"), adminHandler.ListAlertRules)
	api.PUT("/alert-rules", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), adminHandler.PutAlertRules)
	api.DELETE("/alert-rules/:id", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { adminHandler.DeleteAlertRule(c, c.Param("id")) })

	server := newHTTPServer(config.ListenAddr, router)
	initialized = true
	return &App{Config: config, DB: db, Router: router, Server: server, StopLicense: stopLicense}, nil
}

// InternalReportStatusPath 是合同导入报表状态机器接口的权威调用路径。
// 合同系统以 CONTRACT_IMPORT_REPORT_BASE_URL（服务 origin）+ 该绝对路径调用，
// 因此不随 APP_PATH_PREFIX 变化，也不在 base 组下重复挂载。
const InternalReportStatusPath = "/internal/v1/contract-imports/report-status"

// reportStatusVerifier 按机器信任绑定的完整性决定是否构造报表状态验证器。
// 参数：config 为当前进程配置。
// 返回值：验证器；绑定不完整时返回 nil（端点不挂载）；租户不一致或构造失败返回错误。
//
// 机器端点默认关闭：CONTRACT_IMPORT_STATUS_CLIENT_ID 或
// REPORT_STATUS_MACHINE_OAUTH_CLIENT_ID 任一为空都不构造验证器，App.New 因此不挂载
// /internal/v1/contract-imports/report-status。缺少 OAuth 客户端绑定时宁可不出该端点，
// 也不能挂一个无法证明“是哪个客户端调的”的匿名读取入口。
func reportStatusVerifier(config Config) (*importreport.Verifier, error) {
	status := config.ContractImportStatus
	if strings.TrimSpace(status.ClientID) == "" || strings.TrimSpace(status.OAuthClientID) == "" {
		return nil, nil
	}
	if status.TenantID != config.OIDCTenantID {
		return nil, errors.New("report-status trusted tenant mismatch")
	}
	return importreport.NewVerifier(status)
}

// mountInternalReportStatus 只挂载合同契约声明的根路径。
// 参数：router 为根路由，status 为机器验签处理器。
// 返回值：无；重复挂载会扩大未审计的入口面，故由该函数集中约束。
func mountInternalReportStatus(router *gin.Engine, status gin.HandlerFunc) {
	router.GET(InternalReportStatusPath, status)
}

// newHTTPServer 构造 dashboard-api 的 http.Server。
// 安全理由（AUD-2026-031）：四项超时全缺会暴露 slowloris 等慢连接资源耗尽面；
// 取值对齐 project_management/cmd/api/main.go 的既有配置（5s/15s/45s/60s），
// 保持子系统间一致的连接生命周期口径。
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

var _ = slog.Default
