package infrastructure

import (
	"strings"
	"testing"
	"time"
)

// TestMonthGroupingQueriesBindBusinessOffset 验证看板三处按月分组都先按业务时区
// 换算再取月前缀，且偏移是绑定参数而非硬编码 +08:00。
// 参数：t 为测试上下文。
// 返回值：无；任一处仍按 UTC 月分组或写死偏移时失败。
func TestMonthGroupingQueriesBindBusinessOffset(t *testing.T) {
	queries := map[string]string{
		"项目快照趋势": projectTrendQuery,
		"合同签约月":  contractSigningPeriodExpr("mysql"),
		"项目创建月":  projectCreationPeriodExpr("mysql"),
	}
	for name, query := range queries {
		if !strings.Contains(query, "CONVERT_TZ(") {
			t.Fatalf("%s 未按业务时区换算月份：%s", name, query)
		}
		if !strings.Contains(query, "'+00:00', ?") {
			t.Fatalf("%s 的业务时区偏移必须是绑定参数：%s", name, query)
		}
		if strings.Contains(query, "+08:00") {
			t.Fatalf("%s 不得硬编码 +08:00：%s", name, query)
		}
	}
	// 项目趋势窗口下界必须是业务月边界，不能再用 UTC 的"现在"。
	if strings.Contains(projectTrendQuery, "UTC_TIMESTAMP") {
		t.Fatalf("项目趋势窗口不得使用 UTC_TIMESTAMP：%s", projectTrendQuery)
	}
	if strings.Count(projectTrendQuery, "CONVERT_TZ(") != 2 {
		t.Fatalf("项目趋势的月前缀与窗口分区都必须换算业务时区：%s", projectTrendQuery)
	}
	// SQLite 方言用时间修饰符占位符而不是 CONVERT_TZ。
	for name, query := range map[string]string{
		"合同签约月": contractSigningPeriodExpr("sqlite"),
		"项目创建月": projectCreationPeriodExpr("sqlite"),
	} {
		if !strings.Contains(query, "strftime('%Y-%m'") || !strings.Contains(query, "?") {
			t.Fatalf("%s 的 SQLite 方言未带业务时区偏移参数：%s", name, query)
		}
	}
}

// TestProjectTrendArgsMatchQueryPlaceholders 验证项目趋势查询的绑定参数与占位符
// 一一对应：两个业务时区偏移、租户、业务月窗口下界、月份上限。
// 参数：t 为测试上下文。
// 返回值：无；参数顺序或个数与 SQL 不一致时失败。
func TestProjectTrendArgsMatchQueryPlaceholders(t *testing.T) {
	shanghai, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatal(err)
	}
	// 北京时间 2026-02-01 00:30（UTC 仍是 1 月 31 日）。
	now := time.Date(2026, time.January, 31, 16, 30, 0, 0, time.UTC)
	repository := NewGORMRepositoryWithLocation(nil, shanghai)
	args := repository.projectTrendArgs(now, "tenant-a", 3)
	if got := strings.Count(projectTrendQuery, "?"); got != len(args) {
		t.Fatalf("占位符 %d 个，绑定参数 %d 个：%s", got, len(args), projectTrendQuery)
	}
	want := []any{"+08:00", "+08:00", "tenant-a", "2025-11-30 16:00:00", 3}
	if len(args) != len(want) {
		t.Fatalf("绑定参数 = %#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("绑定参数[%d] = %#v, want %#v（全部：%#v）", i, args[i], want[i], args)
		}
	}
}

// TestBusinessMonthWindowStartUsesBusinessMonthBoundary 验证"近 N 月"窗口下界取
// 业务月月初再回退 N-1 个月，并按 UTC 墙钟输出。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBusinessMonthWindowStartUsesBusinessMonthBoundary(t *testing.T) {
	shanghai, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatal(err)
	}
	// 北京时间 2026-02-01 00:30：UTC 还在 1 月，业务月已经是 2 月。
	// 业务月月初是上海零点，换算成 UTC 墙钟要回退 8 小时（不是加 8 小时）。
	now := time.Date(2026, time.January, 31, 16, 30, 0, 0, time.UTC)
	repository := NewGORMRepositoryWithLocation(nil, shanghai)
	cases := []struct {
		name   string
		months int
		want   string
	}{
		{name: "3 months", months: 3, want: "2025-11-30 16:00:00"},
		{name: "1 month", months: 1, want: "2026-01-31 16:00:00"},
		{name: "zero clamps to 1", months: 0, want: "2026-01-31 16:00:00"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := repository.businessMonthWindowStart(now, testCase.months); got != testCase.want {
				t.Fatalf("businessMonthWindowStart(%s, %d) = %q, want %q", now.Format(time.RFC3339), testCase.months, got, testCase.want)
			}
		})
	}
}

// TestBusinessOffsetFollowsInjectedLocation 验证偏移取自注入的业务时区。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBusinessOffsetFollowsInjectedLocation(t *testing.T) {
	shanghai, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if got := NewGORMRepositoryWithLocation(nil, shanghai).businessOffset(now); got != "+08:00" {
		t.Fatalf("shanghai offset = %q, want +08:00", got)
	}
	if got := NewGORMRepositoryWithLocation(nil, time.UTC).businessOffset(now); got != "+00:00" {
		t.Fatalf("utc offset = %q, want +00:00", got)
	}
	if got := NewGORMRepositoryWithLocation(nil, nil).businessOffset(now); got != "+00:00" {
		t.Fatalf("nil location offset = %q, want +00:00", got)
	}
}
