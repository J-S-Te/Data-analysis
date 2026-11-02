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
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// main 启动受许可保护的聚合执行进程。
// 参数：无，配置由环境和命令行提供。
// 返回值：无，初始化或单轮失败时进程非零退出。
func main() {
	once := flag.Bool("once", os.Getenv("AGGREGATION_ONCE") == "true", "run a single sync pass and exit")
	interval := flag.Duration("interval", 60*time.Second, "loop interval (ignored with --once)")
	queueLease := flag.Duration("sync-job-lease", 2*time.Minute, "database-clock queue lease; heartbeat renews healthy long tasks")
	queueHeartbeat := flag.Duration("sync-job-heartbeat", 30*time.Second, "must be at most one third of lease")
	queueTimeout := flag.Duration("sync-job-timeout", 30*time.Minute, "single queued task timeout; unfinished task is safely requeued")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
	sources := map[string]string{}
	for _, code := range aggregationSourceCodes {
		if dsn := os.Getenv(readDSNEnv(code)); dsn != "" {
			sources[code] = dsn
		}
	}
	// 轮次重叠守卫在启动期校验：非法值失败关闭，不静默退回 warn。
	overlapGuard, err := overlapGuardFromEnv(os.Getenv)
	if err != nil {
		logger.Error("aggregation overlap guard invalid", "error", err)
		os.Exit(1)
	}
	var runner *aggregation.Runner
	if len(sources) > 0 || projection.URL != "" || importOptions.Enabled {
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
	warnDisabledSources(os.Getenv, logger)
	aggDB, err := gorm.Open(mysql.Open(aggDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		logger.Error("aggregation db failed", "error", err)
		os.Exit(1)
	}
	apiSync := aggregation.NewAPISyncRunner(aggDB, aggregation.APISyncOptions{
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
		ContractInternalURL: os.Getenv("CONTRACT_INTERNAL_URL"),
		ProjectInternalURL:  os.Getenv("PROJECT_INTERNAL_URL"),
		TenantID:            os.Getenv("OIDC_TENANT_ID"),
	})
	if runner != nil {
		runner.ProjectProjection = projection
		runner.ProjectAPI = apiSync
	}
	pass := aggregation.Pass{LicenseCheck: checkBusiness}
	if importOptions.Enabled {
		importAPI := aggregation.NewAPISyncRunner(aggDB, aggregation.APISyncOptions{
			LicenseCheck: checkBusiness, MachineTokenURL: importOptions.TokenURL,
			MachineTokenIssuer:   firstNonEmpty(os.Getenv("MACHINE_TOKEN_ISSUER"), os.Getenv("OIDC_ISSUER")),
			MachineTokenAudience: firstNonEmpty(os.Getenv("MACHINE_TOKEN_AUDIENCE"), "basic-platform-application"),
			TenantID:             os.Getenv("OIDC_TENANT_ID"), ContractCredential: importOptions.Credential,
		})
		pass.SyncImportReport = func(ctx context.Context) error { return runner.SyncImportReport(ctx, importAPI, importOptions.URL) }
	}
	if runner != nil {
		// 主动跨库同步：从源库（合同/客户）读取并写入聚合库维表，保证看板有数据。
		pass.SyncSources = func(ctx context.Context) error {
			if err := runner.EnsureSyncSources(ctx); err != nil {
				return err
			}
			return runner.RunOnce(ctx)
		}
		// 跨系统同步也支持由已持久化的访问/管理请求（队列）触发。
		pass.SyncJobs = runner.RunQueued
	}
	if os.Getenv("CONTRACT_INTERNAL_URL") != "" {
		pass.SyncContractDashboard = apiSync.SyncContractDashboard
	}
	if os.Getenv("PROJECT_INTERNAL_URL") != "" {
		pass.SyncProjectDashboard = apiSync.SyncProjectDashboard
	}
	if !pass.Configured() {
		// 无任何同步通道：once 模式单次执行后退出合理；长驻模式必须保持
		// 存活等待配置（RunLoop 对空 pass 安全），否则容器会陷入重启循环。
		logger.Warn("no database sources or internal API endpoints configured; nothing to sync")
		if *once {
			return
		}
		if err := aggregation.RunLoop(ctx, *interval, pass, logger); err != nil {
			logger.Error("aggregation-worker stopped with error", "error", err)
			os.Exit(1)
		}
		return
	}
	if *once {
		if err := pass.RunOnce(ctx); err != nil {
			logger.Error("aggregation pass failed", "error", err)
			os.Exit(1)
		}
		logger.Info("aggregation pass completed")
		return
	}
	if err := aggregation.RunLoop(ctx, *interval, pass, logger); err != nil {
		logger.Error("aggregation-worker stopped with error", "error", err)
		os.Exit(1)
	}
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

// Explicit zero/negative/overflow limits are configuration errors, not defaults.
func projectProjectionOptionsFromEnv() (aggregation.ProjectProjectionOptions, error) {
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
