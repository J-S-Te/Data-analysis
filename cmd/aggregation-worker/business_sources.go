package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

type businessSourceOptions struct {
	ReportURL         string
	FinanceURL        string
	FinanceCredential aggregation.MachineCredential
	Retention         aggregation.SnapshotRetentionPolicy
}

// businessSourcesFromEnv 校验已启用的机器来源和本地有界快照留存。
// 参数：enabled 为外部同步总开关，getenv 为不打印值的配置读取器。
// 返回值：完整配置；关闭时不读取外源配置，启用后的非法配置使启动失败。
func businessSourcesFromEnv(enabled bool, getenv func(string) string) (businessSourceOptions, error) {
	var result businessSourceOptions
	if !enabled {
		return result, nil
	}
	financeMode := getenv("DASHBOARD_FINANCE_SOURCE")
	if financeMode != "" && financeMode != "integration" && financeMode != "standard" {
		return result, errors.New("invalid DASHBOARD_FINANCE_SOURCE")
	}
	result.ReportURL = getenv("PROJECT_REPORT_URL")
	if err := validateBusinessSourceURL(result.ReportURL); err != nil {
		return result, err
	}
	if result.ReportURL != "" {
		if getenv("PROJECT_MACHINE_CLIENT_ID") == "" || getenv("PROJECT_MACHINE_CLIENT_SECRET") == "" ||
			firstNonEmpty(getenv("PROJECT_MACHINE_TOKEN_SCOPE"), "dashboard.project.read") != "dashboard.project.read" {
			return result, errors.New("project report machine credential is incomplete or scope is invalid")
		}
	}
	if financeMode != "standard" {
		result.FinanceURL = getenv("SETTLEMENT_FINANCE_URL")
		if err := validateBusinessSourceURL(result.FinanceURL); err != nil {
			return result, err
		}
		if result.FinanceURL != "" {
			result.FinanceCredential = aggregation.MachineCredential{
				ClientID: getenv("SETTLEMENT_MACHINE_CLIENT_ID"), ClientSecret: getenv("SETTLEMENT_MACHINE_CLIENT_SECRET"),
				Scope: firstNonEmpty(getenv("SETTLEMENT_MACHINE_TOKEN_SCOPE"), "dashboard.settlement.read")}
			if result.FinanceCredential.ClientID == "" || result.FinanceCredential.ClientSecret == "" ||
				result.FinanceCredential.Scope != "dashboard.settlement.read" {
				return result, errors.New("settlement machine credential is incomplete or scope is invalid")
			}
		}
	}
	policy, err := retentionPolicyFromEnv(getenv)
	result.Retention = policy
	return result, err
}

// validateBusinessSourceURL 拒绝带凭据或片段的非HTTP来源配置。
// 参数：raw 为可选受控来源URL。
// 返回值：空值表示该通道未配置；非法URL返回不含原值的错误。
func validateBusinessSourceURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(raw) != raw || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("invalid business dashboard source URL")
	}
	return nil
}

// retentionPolicyFromEnv 在接入数据库之前解析留存窗口和逐轮清理预算。
// 参数：getenv 为配置读取器。
// 返回值：默认7天/24月/500项/20批的合法策略；显式零值和非法预算被拒绝。
func retentionPolicyFromEnv(getenv func(string) string) (aggregation.SnapshotRetentionPolicy, error) {
	var policy aggregation.SnapshotRetentionPolicy
	for _, item := range []struct {
		key    string
		target *int
	}{
		{"DASHBOARD_SNAPSHOT_RECENT_DAYS", &policy.RecentDays},
		{"DASHBOARD_SNAPSHOT_TREND_MONTHS", &policy.TrendMonths},
		{"DASHBOARD_SNAPSHOT_BATCH_SIZE", &policy.BatchSize},
		{"DASHBOARD_SNAPSHOT_MAX_BATCHES", &policy.MaxBatches},
	} {
		if raw := getenv(item.key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				return policy, fmt.Errorf("invalid %s", item.key)
			}
			*item.target = value
		}
	}
	policy, err := aggregation.ValidateSnapshotRetentionPolicy(policy)
	if err == nil && policy.TrendMonths < 24 {
		err = errors.New("snapshot retention must preserve at least 24 trend months")
	}
	return policy, err
}
