package alertworker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	rules         []Rule
	candidates    []ContractExpiryCandidate
	alerts        map[string]Alert
	requestedDays int
	findCalls     int
	reconcile     []string
	reconcileAt   time.Time
	reconcileOpts ReconcileOptions
	activeCount   int64
	countCalls    int
	order         []string
}

func (s *memoryStore) ListEnabledRules(context.Context) ([]Rule, error) {
	return s.rules, nil
}

func (s *memoryStore) FindContractExpiryCandidates(
	_ context.Context,
	_ time.Time,
	days int,
) ([]ContractExpiryCandidate, error) {
	s.findCalls++
	s.requestedDays = days
	return s.candidates, nil
}

func (s *memoryStore) UpsertAlerts(_ context.Context, alerts []Alert) error {
	if s.alerts == nil {
		s.alerts = make(map[string]Alert)
	}
	for _, alert := range alerts {
		key := alert.TenantID + "/" + alert.AlertType + "/" + alert.TargetRef
		s.alerts[key] = alert
	}
	s.order = append(s.order, "upsert")
	return nil
}

func (s *memoryStore) CountActiveAlerts(_ context.Context, ruleCode string) (int64, error) {
	s.countCalls++
	s.order = append(s.order, "count")
	return s.activeCount, nil
}

func (s *memoryStore) ReconcileAlerts(_ context.Context, ruleCode string, active []Alert, evaluatedAt time.Time, options ReconcileOptions) error {
	s.reconcile = make([]string, 0, len(active))
	for _, alert := range active {
		if alert.RuleCode == ruleCode {
			s.reconcile = append(s.reconcile, alert.TargetRef)
		}
	}
	s.reconcileAt = evaluatedAt
	s.reconcileOpts = options
	s.order = append(s.order, "reconcile")
	return nil
}

func TestRunOnceUpsertsContractExpiryAlertsIdempotently(t *testing.T) {
	dueDate := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	store := &memoryStore{
		rules: []Rule{{Code: RuleContractExpiry, Severity: "high", ThresholdJSON: []byte(`{"days": 21}`)}},
		candidates: []ContractExpiryCandidate{{
			TenantID:  "tenant-1",
			TargetRef: "contract-1",
			Title:     "合同即将到期",
			DueDate:   dueDate,
		}},
	}
	worker := New(store)
	worker.now = func() time.Time { return time.Date(2026, time.August, 17, 8, 0, 0, 0, time.UTC) }

	for run := 0; run < 2; run++ {
		count, err := worker.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
		if count != 1 {
			t.Fatalf("RunOnce() count = %d, want 1", count)
		}
	}
	if store.requestedDays != 21 {
		t.Fatalf("requested days = %d, want 21", store.requestedDays)
	}
	if len(store.alerts) != 1 {
		t.Fatalf("idempotent alert count = %d, want 1", len(store.alerts))
	}
	for _, alert := range store.alerts {
		if len(alert.ID) != 26 {
			t.Errorf("alert ID length = %d, want 26", len(alert.ID))
		}
		if alert.Severity != "HIGH" {
			t.Errorf("severity = %q, want HIGH", alert.Severity)
		}
		if !alert.DueDate.Equal(dueDate) {
			t.Errorf("due date = %v, want %v", alert.DueDate, dueDate)
		}
	}
}

func TestRunOnceRejectsInvalidThresholdAndContinuesOtherRules(t *testing.T) {
	store := &memoryStore{rules: []Rule{
		{Code: RuleContractExpiry, ThresholdJSON: []byte(`{"days": 0}`)},
		{Code: "PROJECT_DELAY"},
	}}
	worker := New(store)

	count, err := worker.RunOnce(context.Background())
	if count != 0 {
		t.Fatalf("RunOnce() count = %d, want 0", count)
	}
	if err == nil {
		t.Fatal("RunOnce() error = nil, want joined configuration errors")
	}
	for _, want := range []string{"days must be between", "PROJECT_DELAY is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("RunOnce() error = %q, want substring %q", err, want)
		}
	}
	if store.findCalls != 0 {
		t.Fatalf("candidate query calls = %d, want 0", store.findCalls)
	}
}

func TestRunLoopRejectsNonPositiveInterval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := RunLoop(context.Background(), 0, New(&memoryStore{}), logger)
	if err == nil {
		t.Fatal("RunLoop() error = nil, want invalid interval error")
	}
}

// TestWorkerAutoResolveDefaultsOffWithDefaultOptions 验证未配置时保持现网行为：
// 不查询未关闭基数、不触发任何收敛。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWorkerAutoResolveDefaultsOffWithDefaultOptions(t *testing.T) {
	store := &memoryStore{
		rules:      []Rule{{Code: RuleContractExpiry, Severity: "HIGH"}},
		candidates: []ContractExpiryCandidate{{TenantID: "tenant-1", TargetRef: "contract-1"}},
	}
	worker := New(store)
	worker.SetLifecycle(LifecycleOptions{})
	lifecycle := worker.Lifecycle()
	if lifecycle.AutoResolve {
		t.Fatal("默认策略不得启用自动收敛")
	}
	if lifecycle.ConfirmRounds != defaultConfirmRounds || lifecycle.ReopenPolicy != ReopenPolicyAuto || lifecycle.CloseGuardRatio != defaultCloseGuardRatio {
		t.Fatalf("默认策略未补齐默认值 %+v", lifecycle)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.reconcile != nil || store.countCalls != 0 || len(store.order) != 1 || store.order[0] != "upsert" {
		t.Fatalf("默认策略触发了收敛 reconcile=%v order=%v", store.reconcile, store.order)
	}
}

// TestWorkerCloseGuardSkipsWhenNoActiveHits 验证本轮命中为 0 时只告警不关闭：
// 必须仍以 WARN 记录安全阀，而不是把整条规则批量 CLOSED。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWorkerCloseGuardSkipsWhenNoActiveHits(t *testing.T) {
	var logged bytes.Buffer
	store := &memoryStore{
		rules:       []Rule{{Code: RuleContractExpiry, Severity: "HIGH"}},
		candidates:  nil,
		activeCount: 3,
	}
	worker := New(store)
	worker.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
	worker.SetLifecycle(LifecycleOptions{AutoResolve: true})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.reconcile != nil {
		t.Fatalf("空命中集合触发收敛 %v", store.reconcile)
	}
	if store.countCalls != 1 || len(store.order) != 2 || store.order[0] != "upsert" || store.order[1] != "count" {
		t.Fatalf("安全阀判定顺序不正确 order=%v", store.order)
	}
	if entry := logged.String(); !strings.Contains(entry, "close guard skipped reconcile") || !strings.Contains(entry, "no_active_hits") {
		t.Fatalf("安全阀未记录 WARN 日志: %s", entry)
	}
}

// TestWorkerCloseGuardSkipsWhenActiveShrinksBelowRatio 验证命中集合低于
// "未关闭基数×比例" 时只告警不关闭（1 < 0.5×4）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWorkerCloseGuardSkipsWhenActiveShrinksBelowRatio(t *testing.T) {
	store := &memoryStore{
		rules:       []Rule{{Code: RuleContractExpiry, Severity: "HIGH"}},
		candidates:  []ContractExpiryCandidate{{TenantID: "tenant-1", TargetRef: "contract-1"}},
		activeCount: 4,
	}
	worker := New(store)
	worker.SetLifecycle(LifecycleOptions{AutoResolve: true})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.reconcile != nil {
		t.Fatalf("收缩到比例阈值以下仍触发收敛 %v", store.reconcile)
	}
}

// TestWorkerReconcilePassesLifecycleOptions 验证收敛在幂等写入之后执行，并携带
// 配置的确认轮数与复开策略。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWorkerReconcilePassesLifecycleOptions(t *testing.T) {
	store := &memoryStore{
		rules:      []Rule{{Code: RuleContractExpiry, Severity: "HIGH"}},
		candidates: []ContractExpiryCandidate{{TenantID: "tenant-1", TargetRef: "contract-1"}, {TenantID: "tenant-1", TargetRef: "contract-2"}},
	}
	worker := New(store)
	worker.SetLifecycle(LifecycleOptions{AutoResolve: true, ConfirmRounds: 3, ReopenPolicy: ReopenPolicyManual, CloseGuardRatio: 0.5})
	evaluatedAt := time.Date(2026, time.August, 17, 8, 0, 0, 0, time.UTC)
	worker.now = func() time.Time { return evaluatedAt }
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.reconcile) != 2 || store.reconcile[0] != "contract-1" || store.reconcile[1] != "contract-2" {
		t.Fatalf("收敛命中集合 %v", store.reconcile)
	}
	if len(store.order) != 3 || store.order[0] != "upsert" || store.order[1] != "count" || store.order[2] != "reconcile" {
		t.Fatalf("收敛顺序 %v（必须先幂等写入、再判定安全阀、最后收敛）", store.order)
	}
	if !store.reconcileAt.Equal(evaluatedAt) {
		t.Fatalf("收敛基准时间 %v", store.reconcileAt)
	}
	if store.reconcileOpts.ConfirmRounds != 3 || store.reconcileOpts.ReopenPolicy != ReopenPolicyManual {
		t.Fatalf("收敛策略未透传 %+v", store.reconcileOpts)
	}
}

// fixedEnv 返回按固定映射取值的 getenv，未声明的键返回空串。
// 参数：values 为键值映射。
// 返回值：getenv 函数。
func fixedEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// TestLifecycleOptionsFromEnvDefaults 验证未设置时：收敛关闭、确认 2 轮、
// auto 复开、安全阀 0.5。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLifecycleOptionsFromEnvDefaults(t *testing.T) {
	options, err := LifecycleOptionsFromEnv(fixedEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if options.AutoResolve {
		t.Fatal("ALERT_AUTO_RESOLVE 默认必须为 false")
	}
	if options.ConfirmRounds != 2 || options.ReopenPolicy != ReopenPolicyAuto || options.CloseGuardRatio != 0.5 {
		t.Fatalf("默认策略不正确 %+v", options)
	}
}

// TestLifecycleOptionsFromEnvOverrides 验证合法取值被正确解析。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLifecycleOptionsFromEnvOverrides(t *testing.T) {
	options, err := LifecycleOptionsFromEnv(fixedEnv(map[string]string{
		"ALERT_AUTO_RESOLVE":      "true",
		"ALERT_CONFIRM_ROUNDS":    "3",
		"ALERT_REOPEN_POLICY":     "MANUAL",
		"ALERT_CLOSE_GUARD_RATIO": "0.8",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !options.AutoResolve || options.ConfirmRounds != 3 || options.ReopenPolicy != ReopenPolicyManual || options.CloseGuardRatio != 0.8 {
		t.Fatalf("配置解析不正确 %+v", options)
	}
}

// TestLifecycleOptionsFromEnvRejectsInvalid 验证非法取值返回错误（启动方据此非零退出），
// 而不是静默降级成默认值。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLifecycleOptionsFromEnvRejectsInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"confirm_rounds_zero":     {"ALERT_CONFIRM_ROUNDS": "0"},
		"confirm_rounds_too_big":  {"ALERT_CONFIRM_ROUNDS": "11"},
		"confirm_rounds_not_int":  {"ALERT_CONFIRM_ROUNDS": "two"},
		"reopen_policy_unknown":   {"ALERT_REOPEN_POLICY": "always"},
		"guard_ratio_zero":        {"ALERT_CLOSE_GUARD_RATIO": "0"},
		"guard_ratio_above_one":   {"ALERT_CLOSE_GUARD_RATIO": "1.5"},
		"guard_ratio_not_a_float": {"ALERT_CLOSE_GUARD_RATIO": "half"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LifecycleOptionsFromEnv(fixedEnv(env)); err == nil {
				t.Fatalf("非法配置 %v 未被拒绝", env)
			}
		})
	}
	if _, err := LifecycleOptionsFromEnv(nil); err == nil {
		t.Fatal("缺失环境读取函数未被拒绝")
	}
}
