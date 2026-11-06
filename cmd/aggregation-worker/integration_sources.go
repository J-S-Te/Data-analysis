package main

import (
	"context"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

// integrationReadSources 仅在外部同步明确启用时读取业务库DSN。
// 参数：enabled 为总开关，getenv 为环境读取函数。
// 返回值：启用且已配置的只读来源；关闭时不读取或解析任何外部DSN。
func integrationReadSources(enabled bool, getenv func(string) string) map[string]string {
	sources := map[string]string{}
	if !enabled {
		return sources
	}
	for _, code := range aggregationSourceCodes {
		if dsn := getenv(readDSNEnv(code)); dsn != "" {
			sources[code] = dsn
		}
	}
	return sources
}

// integrationPass 在装配边界统一选择所有外源同步与队列消费入口。
// 参数：enabled 为总开关，其余参数为已有Runner、API、摘要回调、环境及许可依赖。
// 返回值：关闭时没有外源回调但保留许可依赖的Pass；启用时沿用既有注册。
func integrationPass(
	enabled bool, runner *aggregation.Runner, api *aggregation.APISyncRunner,
	importSync func(context.Context) error, getenv func(string) string,
	check func(context.Context) error,
) aggregation.Pass {
	pass := aggregation.Pass{LicenseCheck: check}
	if !enabled {
		return pass
	}
	pass.SyncImportReport = importSync
	if runner != nil {
		pass.SyncSources = func(ctx context.Context) error {
			if err := runner.EnsureSyncSources(ctx); err != nil {
				return err
			}
			return runner.RunOnce(ctx)
		}
		pass.SyncJobs = runner.RunQueued
	}
	if api != nil && getenv("CONTRACT_INTERNAL_URL") != "" {
		pass.SyncContractDashboard = api.SyncContractDashboard
	}
	if api != nil && getenv("PROJECT_INTERNAL_URL") != "" {
		pass.SyncProjectDashboard = api.SyncProjectDashboard
	}
	return pass
}
