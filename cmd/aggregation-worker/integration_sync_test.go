package main

import (
	"context"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegrationSyncDisabledIgnoresProjectConfiguration 验证关闭总开关时不解析未启用的项目投影。
// 参数：t 为测试上下文。
// 返回值：无；外源配置阻断独立运行时失败。
func TestIntegrationSyncDisabledIgnoresProjectConfiguration(t *testing.T) {
	t.Setenv("DASHBOARD_INTEGRATION_SYNC_ENABLED", "false")
	t.Setenv("PROJECT_PROJECTION_URL", "http://uninstalled-project/snapshot")
	t.Setenv("PROJECT_PROJECTION_HTTP_TIMEOUT", "unconfigured")
	options, err := projectProjectionOptionsFromEnv()
	if err != nil || options.URL != "" {
		t.Fatalf("disabled projection=%+v err=%v", options, err)
	}
}

// TestIntegrationSyncDisabledIgnoresImportConfiguration 验证关闭总开关时不会注册合同历史摘要同步。
// 参数：t 为测试上下文。
// 返回值：无；历史合同外源配置被读取或启用时失败。
func TestIntegrationSyncDisabledIgnoresImportConfiguration(t *testing.T) {
	t.Setenv("DASHBOARD_INTEGRATION_SYNC_ENABLED", "false")
	t.Setenv("CONTRACT_IMPORT_SYNC_ENABLED", "invalid")
	options, err := importReportOptionsFromEnv()
	if err != nil || options.Enabled {
		t.Fatalf("disabled import=%+v err=%v", options, err)
	}
}

// TestIntegrationSyncDisabledSkipsReadSources 验证四类外部DSN不会在关闭时被读取。
// 参数：t 为测试上下文。
// 返回值：无；读取了未启用的业务库配置时失败。
func TestIntegrationSyncDisabledSkipsReadSources(t *testing.T) {
	sources := integrationReadSources(false, func(key string) string { t.Fatalf("disabled sync read external configuration %s", key); return "" })
	if len(sources) != 0 {
		t.Fatalf("disabled sources=%v", sources)
	}
	enabled := integrationReadSources(true, func(string) string { return "synthetic-not-a-real-dsn" })
	if len(enabled) != 4 {
		t.Fatalf("enabled source count=%d", len(enabled))
	}
}

// TestAggregationWorkerRejectsInvalidIntegrationFlag 验证聚合进程在许可及数据库初始化前拒绝非法总开关。
// 参数：t 为测试上下文，子进程只运行预期配置失败分支。
// 返回值：无；非法配置未失败或触达后续初始化时失败。
func TestAggregationWorkerRejectsInvalidIntegrationFlag(t *testing.T) {
	if os.Getenv("DA_AGG_FLAG_TEST_CHILD") == "1" {
		main()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAggregationWorkerRejectsInvalidIntegrationFlag$")
	command.Env = append(os.Environ(), "DA_AGG_FLAG_TEST_CHILD=1", "DASHBOARD_INTEGRATION_SYNC_ENABLED=invalid", "DASHBOARD_MYSQL_DSN=")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "integration sync configuration invalid") {
		t.Fatalf("startup did not reject flag: err=%v output=%s", err, output)
	}
	if strings.Contains(string(output), "commercial license configuration invalid") || strings.Contains(string(output), "DASHBOARD_MYSQL_DSN is required") {
		t.Fatalf("later initialization reached: %s", output)
	}
}

// TestIntegrationSyncDisabledRegistersNoExternalTasks 验证关闭时外源及旧队列没有可执行回调。
// 参数：t 为测试上下文。
// 返回值：无；误注册任何外源任务或丢失许可检查依赖时失败。
func TestIntegrationSyncDisabledRegistersNoExternalTasks(t *testing.T) {
	callbacks := 0
	check := func(context.Context) error { return nil }
	pass := integrationPass(false, &aggregation.Runner{}, &aggregation.APISyncRunner{}, func(context.Context) error { callbacks++; return nil }, func(key string) string { t.Fatalf("disabled task read %s", key); return "" }, check)
	if pass.Configured() || pass.SyncJobs != nil || pass.SyncSources != nil || pass.SyncImportReport != nil || pass.SyncContractDashboard != nil || pass.SyncProjectDashboard != nil {
		t.Fatal("disabled integration registered external callbacks")
	}
	if pass.LicenseCheck == nil {
		t.Fatal("license dependency discarded")
	}
	if err := pass.RunOnce(context.Background()); err != nil || callbacks != 0 {
		t.Fatalf("disabled task execution callbacks=%d err=%v", callbacks, err)
	}
}

// TestIntegrationSyncEnabledKeepsExistingTaskRegistration 验证执行器统一管理来源且不重复同步API。
// 参数：t 为测试上下文。
// 返回值：无；默认兼容模式漏装任务时失败。
func TestIntegrationSyncEnabledKeepsExistingTaskRegistration(t *testing.T) {
	pass := integrationPass(true, &aggregation.Runner{}, &aggregation.APISyncRunner{}, func(context.Context) error { return nil }, func(string) string { return "http://synthetic-service" }, func(context.Context) error { return nil })
	if !pass.Configured() || pass.SyncJobs == nil || pass.SyncSources == nil || pass.SyncImportReport == nil || pass.SyncContractDashboard != nil || pass.SyncProjectDashboard != nil {
		t.Fatal("执行器来源/队列丢失或重复装配API回调")
	}
}

// TestAPIOnlyPassKeepsLegacyCallbacks 验证无完整执行器时仍兼容独立API入口。
// 参数：t 为配置测试上下文。
// 返回值：无；独立API同步被意外移除时失败。
func TestAPIOnlyPassKeepsLegacyCallbacks(t *testing.T) {
	pass := integrationPass(true, nil, &aggregation.APISyncRunner{}, nil,
		func(string) string { return "http://synthetic-service" }, nil)
	if pass.SyncContractDashboard == nil || pass.SyncProjectDashboard == nil || !pass.Configured() {
		t.Fatal("独立API兼容入口丢失")
	}
}
