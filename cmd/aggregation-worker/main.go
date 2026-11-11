// aggregation-worker：夜间跨库同步 + T+1 预计算。
// 支持单次执行和固定间隔串行执行；各同步通道互不阻塞并汇总错误。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/integrationsync"
	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shutdown"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// main 校验来源配置并启动受许可保护的聚合轮次。
// 参数：无；配置来自命令行及运行环境。
// 返回值：无；配置或执行故障以非零退出状态报告。
func main() {
	once := flag.Bool("once", os.Getenv("AGGREGATION_ONCE") == "true", "run a single sync pass and exit")
	interval := flag.Duration("interval", 60*time.Second, "loop interval (ignored with --once)")
	queueLease := flag.Duration("sync-job-lease", 2*time.Minute, "database-clock queue lease; heartbeat renews healthy long tasks")
	queueHeartbeat := flag.Duration("sync-job-heartbeat", 30*time.Second, "must be at most one third of lease")
	queueTimeout := flag.Duration("sync-job-timeout", 30*time.Minute, "single queued task timeout; unfinished task is safely requeued")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	// B2：关闭预算在启动期解析，非法值失败关闭（不静默退回默认值）。
	drainBudget, err := shutdown.BudgetFromEnv(os.Getenv)
	if err != nil {
		logger.Error("aggregation-worker shutdown budget invalid", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	integrationSyncEnabled, err := integrationsync.EnabledFromEnv(os.Getenv)
	if err != nil {
		logger.Error("integration sync configuration invalid", "error", err)
		os.Exit(1)
	}
	metricFactsEnabled, err := metricfacts.EnabledFromEnv(os.Getenv)
	if err != nil {
		logger.Error("metric facts configuration invalid", "error", err)
		os.Exit(1)
	}
	importOptions, err := importReportOptionsFromEnv()
	if err != nil {
		logger.Error("contract import report configuration invalid", "error", err)
		os.Exit(1)
	}
	projection, err := projectProjectionOptionsFromEnv()
	if err != nil {
		logger.Error("project projection configuration invalid", "error", err)
		os.Exit(1)
	}
	businessSources, err := businessSourcesFromEnv(integrationSyncEnabled, os.Getenv)
	if err != nil {
		logger.Error("business dashboard configuration invalid", "error", err)
		os.Exit(1)
	}
	licenseCheck, err := commercial.Start(ctx)
	if err != nil {
		logger.Error("commercial license configuration invalid")
		os.Exit(1)
	}
	checkBusiness := func(ctx context.Context) error { return licenseCheck.Allow(ctx, commercial.MutateBusiness) }

	aggDSN := os.Getenv("DASHBOARD_MYSQL_DSN")
	if aggDSN == "" {
		logger.Error("DASHBOARD_MYSQL_DSN is required")
		os.Exit(1)
	}
	// 财务快照业务日与看板“今天/到期”共用 DASHBOARD_TIMEZONE：非法时区在启动期
	// 失败关闭，不让 as_of_date/月前缀/90 天 cutoff 静默漂回 UTC。
	businessLocation, err := businessLocationFromEnv(os.Getenv)
	if err != nil {
		logger.Error("business day timezone invalid", "error", err)
		os.Exit(1)
	}
	sources := integrationReadSources(integrationSyncEnabled, os.Getenv)
	// 轮次重叠守卫在启动期校验：非法值失败关闭，不静默退回 warn。
	overlapGuard, err := overlapGuardFromEnv(os.Getenv)
	if err != nil {
		logger.Error("aggregation overlap guard invalid", "error", err)
		os.Exit(1)
	}
	var runner *aggregation.Runner
	hasAPISource := integrationSyncEnabled && (os.Getenv("CONTRACT_INTERNAL_URL") != "" ||
		os.Getenv("PROJECT_INTERNAL_URL") != "" || businessSources.ReportURL != "" || businessSources.FinanceURL != "")
	if len(sources) > 0 || projection.URL != "" || importOptions.Enabled || hasAPISource {
		runner, err = aggregation.NewRunner(aggDSN, os.Getenv("OIDC_TENANT_ID"), sources)
		if err != nil {
			logger.Error("aggregation runner failed", "error", err)
			os.Exit(1)
		}
		runner.LicenseCheck = checkBusiness
		runner.BusinessLocation = businessLocation
		runner.RunOverlapGuard = overlapGuard
		runner.QueueLeaseDuration = *queueLease
		runner.QueueHeartbeatInterval = *queueHeartbeat
		runner.QueueTaskTimeout = *queueTimeout
		if err := runner.ValidateQueueSettings(); err != nil {
			logger.Error("sync queue lease configuration invalid", "error", err)
			os.Exit(1)
		}
	}
	// 空 DSN 表示该业务源被显式禁用（只读 DSN 不下发时不登记、不伪造数据）；
	// 逐个源显式告警，避免运维把“看板缺数”误判为同步 bug。日志只含源与变量名。
	if integrationSyncEnabled {
		warnDisabledSources(os.Getenv, logger)
	} else {
		logger.Info("external business integration sync explicitly disabled; existing facts and queued jobs retained")
	}
	aggDB, err := gorm.Open(mysql.Open(aggDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		logger.Error("aggregation db failed", "error", err)
		os.Exit(1)
	}
	var apiSync *aggregation.APISyncRunner
	if integrationSyncEnabled {
		apiSync = aggregation.NewAPISyncRunner(aggDB, aggregation.APISyncOptions{
			LicenseCheck:         checkBusiness,
			MachineTokenURL:      os.Getenv("MACHINE_TOKEN_URL"),
			MachineTokenIssuer:   firstNonEmpty(os.Getenv("MACHINE_TOKEN_ISSUER"), os.Getenv("OIDC_ISSUER")),
			MachineTokenAudience: firstNonEmpty(os.Getenv("MACHINE_TOKEN_AUDIENCE"), "basic-platform-application"),
			ContractCredential: aggregation.MachineCredential{
				ClientID: os.Getenv("CONTRACT_MACHINE_CLIENT_ID"), ClientSecret: os.Getenv("CONTRACT_MACHINE_CLIENT_SECRET"),
				Scope: firstNonEmpty(os.Getenv("CONTRACT_MACHINE_TOKEN_SCOPE"), "dashboard.contract.read"),
			},
			ProjectCredential: aggregation.MachineCredential{
				ClientID: os.Getenv("PROJECT_MACHINE_CLIENT_ID"), ClientSecret: os.Getenv("PROJECT_MACHINE_CLIENT_SECRET"),
				Scope: firstNonEmpty(os.Getenv("PROJECT_MACHINE_TOKEN_SCOPE"), "dashboard.project.read"),
			},
			ContractInternalURL:  os.Getenv("CONTRACT_INTERNAL_URL"),
			ProjectInternalURL:   os.Getenv("PROJECT_INTERNAL_URL"),
			ProjectReportURL:     businessSources.ReportURL,
			SettlementFinanceURL: businessSources.FinanceURL,
			MetricFactsEnabled:   metricFactsEnabled,
			SettlementCredential: businessSources.FinanceCredential,
			TenantID:             os.Getenv("OIDC_TENANT_ID"),
		})
	}
	if runner != nil {
		runner.ProjectProjection = projection
		runner.ProjectAPI = apiSync
	}
	var importSync func(context.Context) error
	if importOptions.Enabled {
		importAPI := aggregation.NewAPISyncRunner(aggDB, aggregation.APISyncOptions{
			LicenseCheck: checkBusiness, MachineTokenURL: importOptions.TokenURL,
			MachineTokenIssuer:   firstNonEmpty(os.Getenv("MACHINE_TOKEN_ISSUER"), os.Getenv("OIDC_ISSUER")),
			MachineTokenAudience: firstNonEmpty(os.Getenv("MACHINE_TOKEN_AUDIENCE"), "basic-platform-application"),
			TenantID:             os.Getenv("OIDC_TENANT_ID"), ContractCredential: importOptions.Credential,
		})
		importSync = func(ctx context.Context) error { return runner.SyncImportReport(ctx, importAPI, importOptions.URL) }
	}
	pass := integrationPass(integrationSyncEnabled, runner, apiSync, importSync, os.Getenv, checkBusiness)
	attachSnapshotRetention(&pass, runner, businessSources.Retention)
	if !pass.Configured() {
		// 无任何同步通道：once 模式单次执行后退出合理；长驻模式必须保持
		// 存活等待配置（RunLoop 对空 pass 安全），否则容器会陷入重启循环。
		if integrationSyncEnabled {
			logger.Warn("no database sources or internal API endpoints configured; nothing to sync")
		}
		if *once {
			return
		}
	} else if *once {
		if err := pass.RunOnce(ctx); err != nil {
			logger.Error("aggregation pass failed", "error", err)
			os.Exit(1)
		}
		logger.Info("aggregation pass completed")
		return
	}
	// 长驻路径：轮次循环放进可 join 的后台组件，收到信号后最多等预算时间，
	// 超预算点名单侧而非零退出（在途轮次可能仍在写聚合库）。
	if err := runLoopWithDrain(ctx, *interval, pass, drainBudget, logger); err != nil {
		logger.Error("aggregation-worker stopped with error", "error", err)
		os.Exit(1)
	}
}

// runLoopWithDrain 运行聚合循环，并在收到关闭信号后有界等待在途轮次。
// 参数：ctx 控制取消；interval 为间隔；pass 为通道；drainBudget 为预算；logger 记录日志。
// 返回值：循环自身错误；或超预算点名未退出组件的错误（调用方必须非零退出）。
func runLoopWithDrain(
	ctx context.Context,
	interval time.Duration,
	pass aggregation.Pass,
	drainBudget time.Duration,
	logger *slog.Logger,
) error {
	group := shutdown.New()
	result := group.Run("aggregation-loop", func() error {
		return aggregation.RunLoop(ctx, interval, pass, logger)
	})
	// 关闭预算只在信号到达后启动；同时观察循环结果，避免启动校验失败后空挂。
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
	}
	if err := group.Join(drainBudget); err != nil {
		return fmt.Errorf("aggregation-worker shutdown incomplete: %w", err)
	}
	return <-result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// businessLocationFromEnv 从 DASHBOARD_TIMEZONE 解析财务快照业务日时区。
// 参数：getenv 为环境读取函数（生产为 os.Getenv，测试可替换）。
// 返回值：已校验的时区对象；空值取默认 Asia/Shanghai，非法值返回错误，
// 由调用方记录日志并非零退出（fail-closed）。日志与错误只含时区名，不含凭据。
func businessLocationFromEnv(getenv func(string) string) (*time.Location, error) {
	return aggregation.LoadBusinessLocation(getenv("DASHBOARD_TIMEZONE"))
}

// aggregationSourceCodes 是本进程登记的全部业务只读源；顺序仅用于稳定的启动日志。
var aggregationSourceCodes = []string{"contract_management", "customer_and_opportunity", "project_management", "settlement"}

// overlapGuardFromEnv 从 AGG_RUN_OVERLAP_GUARD 解析轮次重叠守卫策略。
// 参数：getenv 为环境读取函数（生产为 os.Getenv，测试可替换）。
// 返回值：warn（默认）/enforce 或非法值错误；错误由调用方日志并非零退出。
func overlapGuardFromEnv(getenv func(string) string) (aggregation.RunOverlapGuard, error) {
	return aggregation.ParseRunOverlapGuard(getenv("AGG_RUN_OVERLAP_GUARD"))
}

// warnDisabledSources 对每个只读 DSN 为空（源被显式禁用）的业务源输出一条 WARN。
// 参数：getenv 为环境读取函数（生产为 os.Getenv，测试可替换）；logger 为结构化日志器。
// 返回值：无。只记录源编码与环境变量名，绝不记录 DSN 值或凭据，也不改变退出码与同步行为。
// 注意：project_management 仍可能由项目投影/API 通道提供数据，此处仅表达“数据库直读被禁用”。
func warnDisabledSources(getenv func(string) string, logger *slog.Logger) {
	for _, code := range aggregationSourceCodes {
		envName := readDSNEnv(code)
		if getenv(envName) == "" {
			logger.Warn("aggregation source disabled: read DSN is empty", "source", code, "env", envName)
		}
	}
}

func readDSNEnv(code string) string {
	switch code {
	case "contract_management":
		return "CONTRACT_READ_DSN"
	case "customer_and_opportunity":
		return "CRM_READ_DSN"
	case "project_management":
		return "PROJECT_READ_DSN"
	case "settlement":
		return "SETTLEMENT_READ_DSN"
	default:
		return code + "_READ_DSN"
	}
}

// projectProjectionOptionsFromEnv 仅为启用的外源同步读取严格投影配置。
// 参数：无；配置来自环境变量。
// 返回值：投影参数或配置错误；关闭时返回空参数。
func projectProjectionOptionsFromEnv() (aggregation.ProjectProjectionOptions, error) {
	enabled, err := integrationsync.EnabledFromEnv(os.Getenv)
	if err != nil || !enabled {
		return aggregation.ProjectProjectionOptions{}, err
	}
	options := aggregation.ProjectProjectionOptions{URL: os.Getenv("PROJECT_PROJECTION_URL")}
	if value, exists := os.LookupEnv("PROJECT_PROJECTION_HTTP_TIMEOUT"); exists {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return options, fmt.Errorf("invalid PROJECT_PROJECTION_HTTP_TIMEOUT")
		}
		options.HTTPTimeout = duration
	}
	for _, item := range []struct {
		name   string
		target *int
	}{
		{"PROJECT_PROJECTION_MAX_PROJECTS", &options.MaxProjects},
		{"PROJECT_PROJECTION_MAX_BYTES", &options.MaxBytes},
		{"PROJECT_PROJECTION_MAX_LINE_BYTES", &options.MaxLineBytes},
	} {
		if value, exists := os.LookupEnv(item.name); exists {
			number, err := strconv.Atoi(value)
			if err != nil || number <= 0 {
				return options, fmt.Errorf("invalid %s", item.name)
			}
			*item.target = number
		}
	}
	return options, aggregation.ValidateProjectProjectionOptions(options)
}
