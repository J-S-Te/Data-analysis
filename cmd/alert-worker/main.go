// alert-worker：预警规则计算（聚合库上执行，幂等 upsert alert_item）。
// 对齐 customer_and_opportunity/cmd/*-worker；阈值参数读取平台 cfg 命名空间 data_analysis.alert_rules（设计方案 §3.6/§6）。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/alertworker"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/integrationsync"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shutdown"
)

// main 启动受许可保护的本地聚合告警评估，不装配外部业务数据源。
// 参数：无；配置来自环境和命令行。
// 返回值：无；配置或许可初始化失败时非零退出。
func main() {
	once := flag.Bool("once", envBool("ALERT_WORKER_ONCE"), "run one alert evaluation pass and exit")
	interval := flag.Duration("interval", envDuration("ALERT_WORKER_INTERVAL", 5*time.Minute), "alert evaluation interval")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// B2：关闭预算在启动期解析，非法值失败关闭（不静默退回默认值）。
	drainBudget, err := shutdown.BudgetFromEnv(os.Getenv)
	if err != nil {
		logger.Error("alert-worker shutdown budget invalid", "error", err)
		os.Exit(1)
	}
	if _, err := integrationsync.EnabledFromEnv(os.Getenv); err != nil {
		logger.Error("integration sync configuration invalid", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	licenseCheck, err := commercial.Start(ctx)
	if err != nil {
		logger.Error("commercial license configuration invalid")
		os.Exit(1)
	}
	if envBool("ALERT_WORKER_DISABLED") {
		logger.Info("alert-worker disabled")
		return
	}
	dsn := os.Getenv("DASHBOARD_MYSQL_DSN")
	if dsn == "" {
		logger.Error("DASHBOARD_MYSQL_DSN is required")
		os.Exit(1)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		logger.Error("open dashboard database failed", "error", err)
		os.Exit(1)
	}
	tenantID := os.Getenv("OIDC_TENANT_ID")
	if tenantID == "" {
		logger.Error("OIDC_TENANT_ID is required for alert rule isolation")
		os.Exit(1)
	}
	// 到期窗口的"今天"按业务时区（DASHBOARD_TIMEZONE，默认 Asia/Shanghai）换算，
	// 与看板/聚合共用同一键；非法时区在启动期失败关闭，不静默漂回 UTC。
	businessLocation, err := aggregation.LoadBusinessLocation(os.Getenv("DASHBOARD_TIMEZONE"))
	if err != nil {
		logger.Error("business day timezone invalid", "error", err)
		os.Exit(1)
	}
	worker := alertworker.New(alertworker.NewGormStore(db, tenantID))
	worker.Logger = logger
	worker.SetBusinessLocation(businessLocation)
	logger.Info("alert business day timezone", "timezone", businessLocation.String())
	worker.LicenseCheck = func(ctx context.Context) error { return licenseCheck.Allow(ctx, commercial.MutateBusiness) }
	// 告警生命周期收敛默认关闭，保持现网"只增不关"的行为；产品策略确认后由
	// ALERT_AUTO_RESOLVE=true 显式启用（见 docs/alert-lifecycle.md）。
	// 其余策略项（确认轮数、复开策略、收缩安全阀比例）取值非法时启动失败，
	// 避免带病运行把配置错误变成批量误关/误开。
	lifecycle, lifecycleErr := alertworker.LifecycleOptionsFromEnv(os.Getenv)
	if lifecycleErr != nil {
		logger.Error("alert lifecycle configuration invalid", "error", lifecycleErr)
		os.Exit(1)
	}
	worker.SetLifecycle(lifecycle)
	logger.Info("alert lifecycle policy",
		"auto_resolve", lifecycle.AutoResolve,
		"confirm_rounds", lifecycle.ConfirmRounds,
		"reopen_policy", string(lifecycle.ReopenPolicy),
		"close_guard_ratio", lifecycle.CloseGuardRatio,
	)

	if *once {
		count, runErr := worker.RunOnce(ctx)
		if runErr != nil {
			logger.Error("alert evaluation failed", "error", runErr, "processed", count)
			os.Exit(1)
		}
		logger.Info("alert evaluation completed", "processed", count)
		return
	}
	// 长驻路径：单循环放进可 join 的后台组件，收到信号后最多等预算时间，
	// 超预算点名并非零退出（在途规则评估可能仍在写 alert_item）。
	group := shutdown.New()
	result := group.Run("alert-worker-loop", func() error {
		return alertworker.RunLoop(ctx, *interval, worker, logger)
	})
	if err := group.Join(drainBudget); err != nil {
		logger.Error("alert-worker shutdown incomplete", "error", err)
		os.Exit(1)
	}
	if err := <-result; err != nil {
		logger.Error("alert-worker stopped with error", "error", err)
		os.Exit(1)
	}
}

func envBool(key string) bool {
	value, _ := strconv.ParseBool(os.Getenv(key))
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
