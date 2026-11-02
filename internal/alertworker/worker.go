// Package alertworker 根据聚合表计算已启用的预警规则。
package alertworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	RuleContractExpiry = "CONTRACT_EXPIRY"
	defaultExpiryDays  = 30
	maxExpiryDays      = 3650
)

// Rule 是 alert_rule 中 Worker 执行所需的最小字段集合。
type Rule struct {
	Code          string
	Severity      string
	ThresholdJSON []byte
}

// ContractExpiryCandidate 是从 dim_contract 筛出的合同到期候选项。
type ContractExpiryCandidate struct {
	TenantID   string
	OwnerOrgID string
	TargetRef  string
	Title      string
	DueDate    time.Time
	// InWindow 表示到期日位于 [基准日, 基准日+提醒窗口天数] 内。
	// 条件集合包含刚过期但未续签的合同（InWindow=false），它们仍算"条件成立"，
	// 因此保持 OPEN，不会因为滑出窗口被关闭。
	InWindow bool
}

// Alert 是写入 alert_item 的标准化预警快照。
type Alert struct {
	ID          string
	TenantID    string
	AlertType   string
	RuleCode    string
	Severity    string
	OwnerOrgID  string
	TargetRef   string
	Title       string
	DueDate     time.Time
	EvaluatedAt time.Time
}

// Store 定义规则读取、候选查询、幂等写入和生命周期收敛边界。
type Store interface {
	ListEnabledRules(ctx context.Context) ([]Rule, error)
	FindContractExpiryCandidates(ctx context.Context, evaluatedAt time.Time, days int) ([]ContractExpiryCandidate, error)
	UpsertAlerts(ctx context.Context, alerts []Alert) error
	// CountActiveAlerts 返回同一规则下仍未关闭（OPEN/ACK）的告警基数，
	// 供调用方在本轮命中集合异常收缩时触发安全阀。
	CountActiveAlerts(ctx context.Context, ruleCode string) (int64, error)
	// ReconcileAlerts 按本轮实际命中的告警集合收敛同一规则的历史告警状态。
	// 只有启用了 LifecycleOptions.AutoResolve 才会调用；实现必须幂等且租户隔离。
	ReconcileAlerts(ctx context.Context, ruleCode string, active []Alert, evaluatedAt time.Time, options ReconcileOptions) error
}

// ReopenPolicy 控制条件重新命中时是否允许规则评估自动复开告警。
type ReopenPolicy string

const (
	// ReopenPolicyAuto 允许规则自动复开自己关闭的告警（默认）。
	ReopenPolicyAuto ReopenPolicy = "auto"
	// ReopenPolicyManual 禁止规则复开：告警关闭后只能由人工重新打开/确认。
	ReopenPolicyManual ReopenPolicy = "manual"
)

const (
	defaultConfirmRounds   = 2
	defaultCloseGuardRatio = 0.5
	maxConfirmRounds       = 10
)

// LifecycleOptions 控制告警生命周期收敛策略。
// 零值保持现网行为：只按幂等键更新存在的告警，不自动关闭、不自动重开。
// 收敛默认关闭（ALERT_AUTO_RESOLVE=false），由部署方显式开启；启用后也只关闭
// 规则自己算出的告警、只复开规则自己关闭的告警，人工关闭（MANUAL）永远不会
// 被规则评估覆盖。
type LifecycleOptions struct {
	// AutoResolve 统一控制"条件消失自动 CLOSED"和"条件重新命中自动复开"，是总开关。
	AutoResolve bool
	// ConfirmRounds 是连续未命中确认轮数：达到该轮数才关闭，避免单轮抖动误关。
	// 零值按 defaultConfirmRounds(2) 生效。
	ConfirmRounds int
	// ReopenPolicy 控制是否允许规则自动复开；零值按 auto 生效。
	ReopenPolicy ReopenPolicy
	// CloseGuardRatio 是收缩安全阀比例：本轮命中数低于"未关闭基数×比例"时
	// 只告警不关闭。零值按 defaultCloseGuardRatio(0.5) 生效。
	CloseGuardRatio float64
}

// ReconcileOptions 是单次收敛所需的策略快照，避免仓储层自行读取全局配置。
type ReconcileOptions struct {
	ConfirmRounds int
	ReopenPolicy  ReopenPolicy
}

// normalized 补齐零值，使 LifecycleOptions{} 与显式默认值行为一致。
// 参数：无。
// 返回值：补齐默认值后的策略副本。
func (options LifecycleOptions) normalized() LifecycleOptions {
	if options.ConfirmRounds < 1 {
		options.ConfirmRounds = defaultConfirmRounds
	}
	if options.ConfirmRounds > maxConfirmRounds {
		options.ConfirmRounds = maxConfirmRounds
	}
	if options.ReopenPolicy != ReopenPolicyManual {
		options.ReopenPolicy = ReopenPolicyAuto
	}
	if options.CloseGuardRatio <= 0 || options.CloseGuardRatio > 1 {
		options.CloseGuardRatio = defaultCloseGuardRatio
	}
	return options
}

// ParseReopenPolicy 解析 ALERT_REOPEN_POLICY 取值；空值按 auto，未知值返回错误。
// 参数：raw 为环境变量原文，大小写不敏感。
// 返回值：合法策略；非法取值返回错误，由启动方非零退出。
func ParseReopenPolicy(raw string) (ReopenPolicy, error) {
	switch ReopenPolicy(strings.ToLower(strings.TrimSpace(raw))) {
	case "", ReopenPolicyAuto:
		return ReopenPolicyAuto, nil
	case ReopenPolicyManual:
		return ReopenPolicyManual, nil
	default:
		return "", fmt.Errorf("ALERT_REOPEN_POLICY must be auto or manual, got %q", raw)
	}
}

// LifecycleOptionsFromEnv 从环境变量读取生命周期策略：
//   - ALERT_AUTO_RESOLVE（默认 false，总开关；沿用宽松解析，历史取值不阻断启动）
//   - ALERT_CONFIRM_ROUNDS（默认 2，取值 1..10）
//   - ALERT_REOPEN_POLICY（默认 auto，取值 auto|manual）
//   - ALERT_CLOSE_GUARD_RATIO（默认 0.5，取值 (0,1]）
//
// 参数：getenv 为环境变量读取函数（生产用 os.Getenv，测试注入固定值）。
// 返回值：补齐默认值的策略；新配置项取值非法时返回错误，由启动方非零退出。
func LifecycleOptionsFromEnv(getenv func(string) string) (LifecycleOptions, error) {
	if getenv == nil {
		return LifecycleOptions{}, errors.New("lifecycle options require an env getter")
	}
	autoResolve, _ := strconv.ParseBool(strings.TrimSpace(getenv("ALERT_AUTO_RESOLVE")))
	options := LifecycleOptions{AutoResolve: autoResolve}

	if raw := strings.TrimSpace(getenv("ALERT_CONFIRM_ROUNDS")); raw != "" {
		rounds, err := strconv.Atoi(raw)
		if err != nil || rounds < 1 || rounds > maxConfirmRounds {
			return LifecycleOptions{}, fmt.Errorf("ALERT_CONFIRM_ROUNDS must be an integer 1..%d, got %q", maxConfirmRounds, raw)
		}
		options.ConfirmRounds = rounds
	}

	policy, err := ParseReopenPolicy(getenv("ALERT_REOPEN_POLICY"))
	if err != nil {
		return LifecycleOptions{}, err
	}
	options.ReopenPolicy = policy

	if raw := strings.TrimSpace(getenv("ALERT_CLOSE_GUARD_RATIO")); raw != "" {
		ratio, err := strconv.ParseFloat(raw, 64)
		if err != nil || ratio <= 0 || ratio > 1 {
			return LifecycleOptions{}, fmt.Errorf("ALERT_CLOSE_GUARD_RATIO must be a number in (0,1], got %q", raw)
		}
		options.CloseGuardRatio = ratio
	}

	return options.normalized(), nil
}

// Worker 按现有聚合表计算预警；同一租户、类型和目标重复执行时幂等更新。
type Worker struct {
	LicenseCheck func(context.Context) error
	// Logger 记录生命周期安全阀拒绝关闭的告警；为 nil 时使用丢弃日志。
	Logger   *slog.Logger
	store    Store
	now      func() time.Time
	location *time.Location
	// lifecycle 是告警生命周期收敛策略；零值等于现网"只增不关"行为。
	lifecycle LifecycleOptions
}

func New(store Store) *Worker {
	return &Worker{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		now:      func() time.Time { return time.Now().UTC() },
		location: time.UTC,
	}
}

// SetBusinessLocation 注入业务日时区（DASHBOARD_TIMEZONE，启动期已校验）。
// 参数：location 为业务时区，nil 保持默认 UTC。
// 返回值：无。
// 到期窗口的"今天"按该时区换算：否则北京时间 00:00-08:00 期间窗口下界仍停在
// UTC 的昨天，合同会早一天进入/晚一天退出提醒窗口。
func (w *Worker) SetBusinessLocation(location *time.Location) {
	if location == nil {
		location = time.UTC
	}
	w.location = location
}

// businessLocation 返回当前业务日时区；未注入时为 UTC。
// 参数：无。
// 返回值：非空时区。
func (w *Worker) businessLocation() *time.Location {
	if w.location == nil {
		return time.UTC
	}
	return w.location
}

// businessDayStart 取业务时区"今天"对应的 UTC 墙钟零点，供 DATE() 截断。
// 参数：at 为业务时区下的时刻。
// 返回值：同一业务日的零点，时区标记为 UTC。
// 生产 DSN 未指定 loc，驱动按 UTC 格式化绑定值；传 UTC 墙钟零点可让
// DATE(?) 在 UTC 会话下截断成同一个业务日，且不引入人为 +8 小时偏移
// （人为加偏移会让 UTC 会话下的 DATE() 跳到次日）。存储时间戳仍用完整时刻。
func businessDayStart(at time.Time) time.Time {
	year, month, day := at.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// SetLifecycle 设置告警生命周期收敛策略；未调用时为默认关闭。
// 参数：options 为收敛开关集合，零值等于现网行为；未设置的子项按默认值补齐。
// 返回值：无。
func (w *Worker) SetLifecycle(options LifecycleOptions) { w.lifecycle = options.normalized() }

// Lifecycle 返回当前生效的生命周期收敛策略（已补齐默认值），供启动日志与测试断言。
// 参数：无。
// 返回值：当前策略副本。
func (w *Worker) Lifecycle() LifecycleOptions { return w.lifecycle }

// logger 返回可用日志器，避免调用点重复判空。
// 参数：无。
// 返回值：非空日志器。
func (w *Worker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return w.Logger
}

// RunOnce 计算一轮所有已启用规则，并返回成功写入的候选数量。
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	if w.LicenseCheck != nil {
		if err := w.LicenseCheck(ctx); err != nil {
			return 0, err
		}
	}
	rules, err := w.store.ListEnabledRules(ctx)
	if err != nil {
		return 0, fmt.Errorf("list enabled alert rules: %w", err)
	}

	// 评估时刻按业务时区取"现在"：到期窗口的日界跟随 DASHBOARD_TIMEZONE，
	// 与看板"今天/到期"同口径；存储的 evaluated_at 仍是同一瞬时（UTC）。
	evaluatedAt := w.now().In(w.businessLocation())
	processed := 0
	var runErrors []error
	for _, rule := range rules {
		if w.LicenseCheck != nil {
			if err := w.LicenseCheck(ctx); err != nil {
				return processed, err
			}
		}
		switch strings.ToUpper(strings.TrimSpace(rule.Code)) {
		case RuleContractExpiry:
			count, evaluateErr := w.evaluateContractExpiry(ctx, rule, evaluatedAt)
			processed += count
			if evaluateErr != nil {
				runErrors = append(runErrors, fmt.Errorf("evaluate rule %s: %w", rule.Code, evaluateErr))
			}
		default:
			runErrors = append(runErrors, fmt.Errorf("alert rule %s is not supported", rule.Code))
		}
	}
	return processed, errors.Join(runErrors...)
}

// evaluateContractExpiry 评估合同到期告警并在每个业务写入边界复核许可。
// 参数：ctx 为上下文；rule 为当前规则；evaluatedAt 为业务时区评估时刻。
// 返回值：成功处理数量；读取、许可或存储失败时返回实际错误。
func (w *Worker) evaluateContractExpiry(ctx context.Context, rule Rule, evaluatedAt time.Time) (int, error) {
	days, err := contractExpiryDays(rule.ThresholdJSON)
	if err != nil {
		return 0, err
	}
	// 到期窗口下界取业务时区"今天"（UTC 墙钟零点），仓储层 DATE(?) 截断后即为
	// 业务日；告警快照时间仍保留完整评估时刻。
	candidates, err := w.store.FindContractExpiryCandidates(ctx, businessDayStart(evaluatedAt), days)
	if err != nil {
		return 0, fmt.Errorf("find contract expiry candidates: %w", err)
	}
	alerts := make([]Alert, 0, len(candidates))
	for _, candidate := range candidates {
		alerts = append(alerts, Alert{
			ID:          stableAlertID(candidate.TenantID, RuleContractExpiry, candidate.TargetRef),
			TenantID:    candidate.TenantID,
			AlertType:   RuleContractExpiry,
			RuleCode:    rule.Code,
			Severity:    normalizedSeverity(rule.Severity),
			OwnerOrgID:  candidate.OwnerOrgID,
			TargetRef:   candidate.TargetRef,
			Title:       candidate.Title,
			DueDate:     candidate.DueDate,
			EvaluatedAt: evaluatedAt,
		})
	}
	if w.LicenseCheck != nil {
		if err := w.LicenseCheck(ctx); err != nil {
			return 0, err
		}
	}
	if err := w.store.UpsertAlerts(ctx, alerts); err != nil {
		return 0, fmt.Errorf("upsert contract expiry alerts: %w", err)
	}
	lifecycle := w.lifecycle.normalized()
	if !lifecycle.AutoResolve {
		return len(alerts), nil
	}
	// 收敛必须在幂等写入之后执行，否则本轮命中的告警可能被误判为未命中。
	// 安全阀以"写入后仍未关闭的告警基数"为分母：本轮算出的命中集合为 0，或明显
	// 低于基数的比例阈值时只告警不关闭。上游表缺失、租户切换或筛选条件写错都会
	// 表现为命中集合骤降，此时批量 CLOSED 会掩盖真实数据问题。
	activeCount, err := w.store.CountActiveAlerts(ctx, rule.Code)
	if err != nil {
		return 0, fmt.Errorf("count active contract expiry alerts: %w", err)
	}
	if guarded, reason := closeGuarded(len(alerts), activeCount, lifecycle.CloseGuardRatio); guarded {
		if activeCount > 0 {
			w.logger().Warn("alert lifecycle close guard skipped reconcile",
				"rule_code", rule.Code,
				"evaluated_hits", len(alerts),
				"open_total", activeCount,
				"close_guard_ratio", lifecycle.CloseGuardRatio,
				"reason", reason,
			)
		}
		return len(alerts), nil
	}
	options := ReconcileOptions{ConfirmRounds: lifecycle.ConfirmRounds, ReopenPolicy: lifecycle.ReopenPolicy}
	if w.LicenseCheck != nil {
		if err := w.LicenseCheck(ctx); err != nil {
			return 0, err
		}
	}
	if err := w.store.ReconcileAlerts(ctx, rule.Code, alerts, evaluatedAt, options); err != nil {
		return 0, fmt.Errorf("reconcile contract expiry alerts: %w", err)
	}
	return len(alerts), nil
}

// closeGuarded 判断本轮命中集合是否触发收缩安全阀（只告警、不关闭）。
// 参数：hits 为本轮命中数，openTotal 为写入后仍未关闭的告警基数，
// ratio 为生效的收缩安全阀比例。
// 返回值：是否触发安全阀，以及触发原因（用于日志定位）。
func closeGuarded(hits int, openTotal int64, ratio float64) (bool, string) {
	if hits == 0 {
		return true, "no_active_hits"
	}
	if float64(hits) < ratio*float64(openTotal) {
		return true, "active_shrunk_below_ratio"
	}
	return false, ""
}

func contractExpiryDays(raw []byte) (int, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultExpiryDays, nil
	}
	var threshold struct {
		Days int `json:"days"`
	}
	if err := json.Unmarshal(raw, &threshold); err != nil {
		return 0, fmt.Errorf("decode CONTRACT_EXPIRY threshold_json: %w", err)
	}
	if threshold.Days <= 0 || threshold.Days > maxExpiryDays {
		return 0, fmt.Errorf("CONTRACT_EXPIRY days must be between 1 and %d", maxExpiryDays)
	}
	return threshold.Days, nil
}

func normalizedSeverity(severity string) string {
	switch normalized := strings.ToUpper(strings.TrimSpace(severity)); normalized {
	case "LOW", "MEDIUM", "HIGH":
		return normalized
	default:
		return "MEDIUM"
	}
}

func stableAlertID(tenantID, alertType, targetRef string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + alertType + "\x00" + targetRef))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:26]
}
