package aggregation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
	"gorm.io/gorm"
)

type metricRoundStartKey struct{}

type metricSource struct {
	base, source string
	credential   MachineCredential
}

// metricSources 只为显式开启且具备既有机器API地址的来源登记独立采集。
// 参数：无。
// 返回值：不含任何新造凭据的来源路由，不更改既有看板同步状态。
func (r *Runner) metricSources() map[string]metricSource {
	out := map[string]metricSource{}
	if r.ProjectAPI == nil || !r.ProjectAPI.options.MetricFactsEnabled {
		return out
	}
	opts := r.ProjectAPI.options
	for _, item := range []metricSource{
		{opts.ContractInternalURL, "contract_management", opts.ContractCredential},
		{opts.ProjectInternalURL, "project_management", opts.ProjectCredential},
		{opts.SettlementFinanceURL, "settlement", opts.SettlementCredential},
	} {
		if strings.TrimSpace(item.base) != "" {
			out[metricfacts.SourceKey(item.source)] = item
		}
	}
	return out
}

// metricEndpoint 在受控既有机器来源的同一来源地址生成固定只读指标路径。
// 参数：raw为已配置的合同、项目或结算看板机器URL。
// 返回值：同源指标URL；非HTTP、凭据、查询参数或非约定路径均拒绝。
func metricEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("invalid metric facts source endpoint")
	}
	const prefix = "/internal/v1/dashboard"
	if u.Path != prefix && u.Path != prefix+"/finance" && u.Path != prefix+"/reports" {
		return "", errors.New("metric facts requires the canonical internal dashboard endpoint")
	}
	u.Path, u.RawPath = prefix+"/metrics", ""
	return u.String(), nil
}

// readMetricSnapshot 经既有独立最小机器用途读取完整指标，不跟随重定向。
// 参数：ctx为取消/许可上下文，base/credential为受控来源，source为预期系统。
// 返回值：完整同租户事实；错误不包含源响应或凭据。
func (r *APISyncRunner) readMetricSnapshot(ctx context.Context, base string,
	credential MachineCredential, source string) (metricfacts.Bundle, error) {
	if err := r.checkLicense(ctx); err != nil {
		return metricfacts.Bundle{}, err
	}
	endpoint, err := metricEndpoint(base)
	if err != nil {
		return metricfacts.Bundle{}, err
	}
	token, err := r.machineToken(ctx, credential)
	if err != nil {
		return metricfacts.Bundle{}, errors.New("metric facts machine credential unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return metricfacts.Bundle{}, errors.New("invalid metric facts request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-DA-Tenant-ID", r.options.TenantID)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: r.options.HTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return metricfacts.Bundle{}, errors.New("metric facts source request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return metricfacts.Bundle{}, fmt.Errorf("metric facts source returned HTTP %d", response.StatusCode)
	}
	return metricfacts.Decode(response.Body, r.options.TenantID, source, time.Now().UTC())
}

// syncMetricFacts 在既有来源锁和写许可/租约围栏内原子发布新增指标。
// 参数：ctx为当前轮次/租约，code为独立指标来源编码。
// 返回值：完整采集、验证或发布错误；失败保留上次成功快照。
func (r *Runner) syncMetricFacts(ctx context.Context, code string) error {
	started := time.Now().UTC()
	source, ok := r.metricSources()[code]
	if !ok {
		return errors.New("metric facts source is disabled or unconfigured")
	}
	bundle, err := r.ProjectAPI.readMetricSnapshot(ctx, source.base, source.credential, source.source)
	if err != nil {
		return err
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		roundStart := started
		if value, ok := ctx.Value(metricRoundStartKey{}).(time.Time); ok {
			roundStart = value
		}
		job, err := r.lockedQueueJob(ctx, tx, false)
		if err != nil {
			return err
		}
		if job.StartedAt != nil {
			roundStart = *job.StartedAt
		}
		if err := metricfacts.Save(tx, bundle, now); err != nil {
			return err
		}
		return r.markMetricSource(tx, code, statusSuccess, roundStart, now, nil)
	})
}

// isMetricSource 限定需要原子发布水位的三个新增来源。
// 参数：code为同步来源编码。
// 返回值：是否为本轮新增的指标来源，不改变旧来源语义。
func isMetricSource(code string) bool {
	return code == "contract_metrics" || code == "project_metrics" || code == "finance_metrics"
}

// markMetricSource 在调用者事务内更新新指标来源状态并拒绝过期轮次。
// 参数：tx为受许可或租约保护的事务，code/status为来源状态，起止时间及runErr为轮次结果。
// 返回值：数据库错误或未拥有当前来源轮次时错误，调用者必须回滚payload。
func (r *Runner) markMetricSource(tx *gorm.DB, code, status string, started, finished time.Time, runErr error) error {
	started = started.UTC().Truncate(time.Millisecond)
	updates := map[string]any{"last_status": status, "last_run_at": started, "updated_at": finished, "last_error": nil}
	if status == statusSuccess {
		updates["last_success_at"] = finished
	}
	if runErr != nil {
		updates["last_error"] = queueSourceError(runErr)
	}
	result := tx.Model(&SyncSource{}).Where("tenant_id=? AND subsystem_code=? AND (last_run_at IS NULL OR last_run_at <= ?)", r.tenantID, code, started).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("metric facts source round is no longer owned")
	}
	return nil
}

// markMetricRound 为定时新增来源传播起止状态写入错误。
// 参数：ctx为本轮许可上下文，code/status为来源状态，started为起点，runErr为采集错误。
// 返回值：状态写入错误；成功状态由发布事务负责，不从此入口调用。
func (r *Runner) markMetricRound(ctx context.Context, code, status string, started time.Time, runErr error) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return r.markMetricSource(tx, code, status, started, time.Now().UTC(), runErr)
	})
}
