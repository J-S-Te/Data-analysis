// aggregation-worker：夜间跨库同步 + T+1 预计算。
// 支持单次执行和固定间隔串行执行；各同步通道互不阻塞并汇总错误。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	once := flag.Bool("once", os.Getenv("AGGREGATION_ONCE") == "true", "run a single sync pass and exit")
	interval := flag.Duration("interval", 60*time.Second, "loop interval (ignored with --once)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	aggDSN := os.Getenv("DASHBOARD_MYSQL_DSN")
	if aggDSN == "" {
		logger.Error("DASHBOARD_MYSQL_DSN is required")
		os.Exit(1)
	}
	sources := map[string]string{}
	for _, code := range []string{"contract_management", "customer_and_opportunity"} {
		if dsn := os.Getenv(readDSNEnv(code)); dsn != "" {
			sources[code] = dsn
		}
	}
	var runner *aggregation.Runner
	var err error
	if len(sources) > 0 {
		runner, err = aggregation.NewRunner(aggDSN, os.Getenv("OIDC_TENANT_ID"), sources)
		if err != nil {
			logger.Error("aggregation runner failed", "error", err)
			os.Exit(1)
		}
	}
	aggDB, err := gorm.Open(mysql.Open(aggDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		logger.Error("aggregation db failed", "error", err)
		os.Exit(1)
	}
	apiSync := aggregation.NewAPISyncRunner(aggDB, aggregation.APISyncOptions{
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if runner != nil {
		if err := runner.EnsureSyncSources(ctx); err != nil {
			logger.Error("ensure sync sources failed", "error", err)
			os.Exit(1)
		}
	}
	pass := aggregation.Pass{}
	if runner != nil {
		// 主动跨库同步：从源库（合同/客户）读取并写入聚合库维表，保证看板有数据。
		pass.SyncSources = runner.RunOnce
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

func readDSNEnv(code string) string {
	switch code {
	case "contract_management":
		return "CONTRACT_READ_DSN"
	case "customer_and_opportunity":
		return "CRM_READ_DSN"
	default:
		return code + "_READ_DSN"
	}
}
