package alertworker

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

// windowStore 记录 Worker 传给仓储的到期窗口基准日，用于断言业务日口径。
type windowStore struct {
	rules   []Rule
	window  time.Time
	alerts  []Alert
	reconAt time.Time
}

func (s *windowStore) ListEnabledRules(context.Context) ([]Rule, error) { return s.rules, nil }

func (s *windowStore) FindContractExpiryCandidates(_ context.Context, evaluatedAt time.Time, _ int) ([]ContractExpiryCandidate, error) {
	s.window = evaluatedAt
	return nil, nil
}

func (s *windowStore) UpsertAlerts(_ context.Context, alerts []Alert) error {
	s.alerts = append([]Alert(nil), alerts...)
	return nil
}

func (s *windowStore) CountActiveAlerts(context.Context, string) (int64, error) { return 0, nil }

func (s *windowStore) ReconcileAlerts(_ context.Context, _ string, _ []Alert, evaluatedAt time.Time, _ ReconcileOptions) error {
	s.reconAt = evaluatedAt
	return nil
}

// TestContractExpiryWindowUsesBusinessDay 验证到期窗口下界是业务时区的"今天"：
// 北京时间已在次日凌晨（UTC 仍是前一天）时，窗口下界必须跟着跨到业务日，
// 否则合同会早/晚一天进入或退出提醒窗口。
// 参数：t 为测试上下文。
// 返回值：无；窗口下界等于 UTC 日或未按业务日截断时失败。
func TestContractExpiryWindowUsesBusinessDay(t *testing.T) {
	shanghai, err := aggregation.LoadBusinessLocation(aggregation.DefaultBusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	// UTC 2026-01-31 16:30 = 北京时间 2026-02-01 00:30，正是跨零点窗口。
	crossMidnight := time.Date(2026, time.January, 31, 16, 30, 0, 0, time.UTC)
	cases := []struct {
		name     string
		location *time.Location
		now      time.Time
		want     time.Time
	}{
		{
			name:     "shanghai cross midnight",
			location: shanghai,
			now:      crossMidnight,
			want:     time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "shanghai same utc day",
			location: shanghai,
			now:      time.Date(2026, time.February, 1, 0, 30, 0, 0, time.UTC),
			want:     time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "default utc keeps utc day",
			location: time.UTC,
			now:      crossMidnight,
			want:     time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := &windowStore{rules: []Rule{{Code: RuleContractExpiry, ThresholdJSON: []byte(`{"days": 30}`)}}}
			worker := New(store)
			worker.SetBusinessLocation(testCase.location)
			worker.now = func() time.Time { return testCase.now }
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if !store.window.Equal(testCase.want) {
				t.Fatalf("窗口下界 = %s, want %s", store.window.Format(time.RFC3339), testCase.want.Format(time.RFC3339))
			}
			// 传给 SQL 的必须是 UTC 墙钟零点：驱动按 UTC 格式化，DATE() 截断即业务日。
			if store.window.Location() != time.UTC || store.window.Format("15:04:05") != "00:00:00" {
				t.Fatalf("窗口下界应是 UTC 墙钟零点，实际 %s (%v)", store.window.Format(time.RFC3339), store.window.Location())
			}
		})
	}
}

// TestContractExpiryEvaluationTimestampKeepsInstant 验证告警快照时间仍是完整评估
// 时刻：窗口下界截断为业务日，但 evaluated_at 不能被截断。
// 参数：t 为测试上下文。
// 返回值：无。
func TestContractExpiryEvaluationTimestampKeepsInstant(t *testing.T) {
	shanghai, err := aggregation.LoadBusinessLocation(aggregation.DefaultBusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, time.January, 31, 16, 30, 0, 0, time.UTC)
	store := &windowStore{rules: []Rule{{Code: RuleContractExpiry, ThresholdJSON: []byte(`{"days": 30}`)}}}
	// 让候选命中一条，确认 UpsertAlerts 收到的 evaluated_at 仍是完整时刻。
	store.rules[0].Severity = "HIGH"
	worker := New(&candidateSeedStore{windowStore: store, at: instant})
	worker.SetBusinessLocation(shanghai)
	worker.now = func() time.Time { return instant }
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.alerts) != 1 {
		t.Fatalf("告警数 = %d, want 1", len(store.alerts))
	}
	if !store.alerts[0].EvaluatedAt.Equal(instant) {
		t.Fatalf("evaluated_at = %s, want %s", store.alerts[0].EvaluatedAt.Format(time.RFC3339), instant.Format(time.RFC3339))
	}
}

// candidateSeedStore 在窗口基准之外补一条候选，用于验证告警快照时间。
type candidateSeedStore struct {
	*windowStore
	at time.Time
}

func (s *candidateSeedStore) FindContractExpiryCandidates(ctx context.Context, evaluatedAt time.Time, days int) ([]ContractExpiryCandidate, error) {
	if _, err := s.windowStore.FindContractExpiryCandidates(ctx, evaluatedAt, days); err != nil {
		return nil, err
	}
	return []ContractExpiryCandidate{{
		TenantID:  "tenant-1",
		TargetRef: "contract-1",
		Title:     "合同即将到期",
		DueDate:   s.at,
	}}, nil
}

// TestBusinessDayStartTruncatesToBusinessDate 验证窗口基准日按业务日期截断。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBusinessDayStartTruncatesToBusinessDate(t *testing.T) {
	shanghai, err := aggregation.LoadBusinessLocation(aggregation.DefaultBusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.January, 31, 16, 30, 0, 0, time.UTC).In(shanghai)
	got := businessDayStart(at)
	if got.Format("2006-01-02 15:04:05") != "2026-02-01 00:00:00" {
		t.Fatalf("businessDayStart(%s) = %s, want 2026-02-01 00:00:00", at.Format(time.RFC3339), got.Format("2006-01-02 15:04:05"))
	}
	if got.Location() != time.UTC {
		t.Fatalf("businessDayStart 必须返回 UTC 墙钟值，实际 %v", got.Location())
	}
}
