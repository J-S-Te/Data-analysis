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
	"github.com/unified-identity-auth-platform/data-analysis/internal/importreport"
	"github.com/unified-identity-auth-platform/data-analysis/internal/metrics"
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
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shutdown"
	"github.com/unified-identity-auth-platform/data-analysis/internal/standardfacts"
)

// App 是数据看板服务运行时容器，保存当前启动配置、数据库连接与路由/HTTP 服务实例，供主流程统一关闭或复用。
type App struct {
	StopLicense context.CancelFunc
	Config      Config
	DB          *gorm.DB
	Router      *gin.Engine
	Server      *http.Server
	// Background 收集随进程退出的后台循环（事务内审计 outbox 投递等）；
	// dashboard-api 关闭时必须先 StopBackground 再 Background.Join，不能直接退进程。
	Background *shutdown.Group
	// StopBackground 取消 Background 中后台循环的运行上下文；nil 表示没有后台循环。
	StopBackground context.CancelFunc
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
	// 后台循环使用独立可取消上下文：进程收到 SIGTERM 时必须能取消 outbox 投递，
	// 否则 dashboard-api 只剩 HTTP Shutdown 有界，投递 goroutine 会在退出瞬间被硬杀。
	backgroundContext, stopBackground := context.WithCancel(context.Background())
	background := shutdown.New()
	initialized := false
	defer func() {
		if !initialized {
			stopLicense()
			stopBackground()
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
	// outbox store 同时服务投递循环与 /metrics 计数：两者必须读同一张表、同一租约口径。
	auditStore := &auditoutbox.GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: 20}
	// 批次 2：事务内审计 outbox 的投递循环；平台不可用时事件留在本地重试。
	if auditReporter != nil {
		auditDispatcher := &auditoutbox.Dispatcher{
			Store:    auditStore,
			Reporter: auditReporter, WorkerID: "data-analysis-api-audit", Poll: 2 * time.Second,
			BatchSize: 50, MaxAttempts: 20, MaxBackoff: 5 * time.Minute, Logger: slog.Default(),
		}
		background.Run("audit-outbox-dispatcher", func() error {
			auditDispatcher.Run(backgroundContext)
			return nil
		})
	} else {
		// 审计发布器配置不完整时不会启动投递循环：事务内审计仍写入
		// bi_audit_outbox，但没有任何循环把它送到平台，表会只增不减、
		// 平台的数据分析审计也会静默缺失。保持既有"启动不 fail-fast"语义
		// （是否必开由 PLATFORM_AUDIT_REQUIRED 与 /readyz 决定），
		// 但必须留下可检索的 WARN。
		slog.Warn("audit outbox dispatcher not started: PLATFORM_AUDIT_CLIENT_ID/AUDIT_INGEST_CLIENT_ID and matching secret incomplete; rows written to bi_audit_outbox will never be delivered to the platform until the audit publisher is configured",
			"table", "bi_audit_outbox", "dispatcher", "disabled",
			"impact", "transactional audit events accumulate locally and are not visible on the platform",
			"readyz_field", "audit_outbox_dispatcher")
	}
	var centralClient central.Client
	if config.CentralSessionEnabled {
		client, err := central.NewHTTPClient(config.PlatformBaseURL, config.CentralSessionPublicBaseURL, config.OIDCClientID, config.OIDCRedirectURI)
		if err != nil {
			return nil, err
		}
		centralClient = client
	}
	authService, err := oidc.NewService(ctx, db, oidc.Options{
		CentralClient:           centralClient,
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
	bridge, err := newDashboardEmbedBridge(db, config, licenseCheck)
	if err != nil {
		return nil, err
	}

	router := gin.New()
	router.Use(middleware.RequestID(), gin.Recovery())
	// C2：/metrics 固定挂在服务根路径，由 MetricsAccess 口径保护（token 或回环），
	// 不参与会话鉴权、不写审计、不随 APP_PATH_PREFIX 变化。
	mountMetrics(router, config.MetricsAccessToken, &metrics.Handler{
		Outbox: auditStore, StartedAt: time.Now(), Logger: slog.Default(),
	})
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
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "audit": "disabled",
				"audit_outbox_dispatcher": auditOutboxDispatcherState(auditReporter), "audit_outbox_required": config.AuditRequired})
			return
		}
		auditStatus := "disabled"
		if auditReporter != nil {
			auditStatus = "enabled"
		}
		// C14：能力维度快照；嵌入开关与财务来源是模式事实，不影响整体就绪。
		// 开关打开但配置不完整时能力状态为 unconfigured，会话能力同步降级为原生模式。
		embedState := "disabled"
		if config.DashboardEmbedEnabled {
			embedState = "unconfigured"
			if dashboardEmbedReady(config) {
				embedState = "ready"
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
			"audit":  auditStatus,
			// B4：审计 Outbox 投递器的启动状态必须可观测。投递循环只在审计发布器
			// 配置完整时启动，配置缺失时事务审计仍写入 bi_audit_outbox 却永不投递。
			// 可选审计未配置时只由这两个字段表达降级，不新造状态值、不新增 503。
			"audit_outbox_dispatcher": auditOutboxDispatcherState(auditReporter),
			"audit_outbox_required":   config.AuditRequired,
			"capabilities": gin.H{
				"dashboard_embed":  gin.H{"enabled": config.DashboardEmbedEnabled, "state": embedState},
				"finance_source":   gin.H{"mode": config.FinanceSource, "state": "ready"},
				"integration_sync": gin.H{"enabled": config.IntegrationSyncEnabled, "state": dashboardSyncState(config.IntegrationSyncEnabled)},
			},
		})
	})
	base.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	embedReady := dashboardEmbedReady(config)
	authHandler := oidc.NewHandler(authService, oidc.HTTPOptions{
		CookieName:            "data_analysis_session",
		PublicOrigin:          config.PublicOrigin,
		SecureCookie:          config.SecureCookie,
		DashboardEmbedEnabled: &embedReady,
	})
	base.GET("/auth/login", authHandler.Login)
	base.GET("/auth/callback", authHandler.Callback)
	base.GET("/auth/session", middleware.SessionAuth(authService, "data_analysis_session"), authHandler.SessionState)
	base.POST("/auth/activity", middleware.SessionAuth(authService, "data_analysis_session"), middleware.RequireSameOriginWrite(config.PublicOrigin), authHandler.Activity)
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

	mountDashboardEmbedRoutes(api, base, bridge, config, licenseCheck)
	(&standardfacts.Handler{Store: &standardfacts.Store{DB: db, LicenseCheck: licenseCheck}, FinanceSource: config.FinanceSource}).Register(api)

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
	dashboardRepository := dashboardinfrastructure.NewGORMRepositoryWithApprovalSource(
		db, dashboardLocation, config.DashboardApprovalSource,
	).WithFinanceSource(config.FinanceSource).WithIntegrationSyncEnabled(config.IntegrationSyncEnabled)
	dashboardHandler := dashboardhttp.NewHandler(dashboardapplication.NewService(dashboardRepository))
	api.GET("/dashboard/contract-import-history", middleware.RequirePermission("dashboard.contract.view"), importreport.HistoryHandler(db))
	api.GET("/dashboard/contract", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.Contract)
	api.GET("/dashboard/project", middleware.RequirePermission("dashboard.project.view"), dashboardHandler.Project)
	api.GET("/dashboard/report", middleware.RequirePermission("dashboard.report.view"), dashboardHandler.Business("report"))
	api.GET("/dashboard/finance", middleware.RequirePermission("dashboard.finance.view"), dashboardHandler.Business("finance"))
	api.GET("/dashboard/contracts", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.Contracts)
	api.GET("/dashboard/contract-regions", middleware.RequirePermission("dashboard.contract.view"), dashboardHandler.ContractRegions)
	api.GET("/dashboard/projects", middleware.RequirePermission("dashboard.project.view"), dashboardHandler.Projects)
	api.GET("/dashboard/trend", middleware.RequirePermission("dashboard.overview.view"), dashboardHandler.Trend)
	api.GET("/admin/sources", middleware.RequirePermission("aggregation.manage"), requireIntegrationSync(config.IntegrationSyncEnabled), adminHandler.ListSources)
	api.POST("/admin/sources/:id/trigger", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("aggregation.manage"), requireIntegrationSync(config.IntegrationSyncEnabled), func(c *gin.Context) { adminHandler.TriggerSource(c, c.Param("id")) })
	api.POST("/admin/sources/trigger-all", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("aggregation.manage"), requireIntegrationSync(config.IntegrationSyncEnabled), adminHandler.TriggerAllSources)
	api.GET("/alert-rules", middleware.RequirePermission("alert.manage"), adminHandler.ListAlertRules)
	api.PUT("/alert-rules", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), adminHandler.PutAlertRules)
	api.DELETE("/alert-rules/:id", middleware.RequireSameOriginWrite(config.PublicOrigin), middleware.RequirePermission("alert.manage"), func(c *gin.Context) { adminHandler.DeleteAlertRule(c, c.Param("id")) })

	server := newHTTPServer(config.ListenAddr, router)
	initialized = true
	return &App{
		Config: config, DB: db, Router: router, Server: server, StopLicense: stopLicense,
		Background: background, StopBackground: stopBackground,
	}, nil
}

// mountMetrics 在多路由根路径挂载只读指标端点。
// 参数：router 为根路由；accessToken 为 METRICS_ACCESS_TOKEN（空表示仅回环）；handler 为指标渲染器。
// 返回值：无。只保留一个权威挂载点：Prometheus 抓取地址是部署契约，
// 再挂一份 APP_PATH_PREFIX 副本会让同一指标出现两个地址，并与接入网关的前缀规则漂移。
func mountMetrics(router *gin.Engine, accessToken string, handler *metrics.Handler) {
	if router == nil || handler == nil {
		return
	}
	router.GET(metrics.Path, metrics.RequireAccess(accessToken), handler.Handle)
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

// dashboardSyncState 渲染集成同步开关状态；disabled 属可选未购买/被停用，
// 不计入就绪降级；enabled 恒为配置校验通过后的运行态。
// 参数：enabled 为该能力开关的运行事实。
// 返回值：ready 或 disabled。
func dashboardSyncState(enabled bool) string {
	if enabled {
		return "ready"
	}
	return "disabled"
}

// auditOutboxDispatcherState 报告审计 Outbox 投递器是否随进程启动。
// 投递循环只在审计发布器非空时启动（见 auditReporter 装配处），因此两者同源；
// 返回 running|disabled，供 /readyz 的 audit_outbox_dispatcher 字段使用。
func auditOutboxDispatcherState(reporter platformaudit.Reporter) string {
	if reporter != nil {
		return "running"
	}
	return "disabled"
}

var _ = slog.Default
