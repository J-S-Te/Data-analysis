package infrastructure

import (
	"context"
	"testing"
	"time"
)

// TestBusinessDateUsesDashboardTimezone 验证“今天/到期”业务日按注入时区换算。
// 参数：t 测试上下文。
// 返回值：无。
func TestBusinessDateUsesDashboardTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DashboardTimezone, err)
	}
	// UTC 2026-01-01 16:30 在上海已是 2026-01-02 00:30，正是 UTC 业务日滞后的窗口。
	instant := time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)
	cases := []struct {
		name     string
		location *time.Location
		want     string
	}{
		{name: "shanghai business day", location: shanghai, want: "2026-01-02"},
		{name: "utc", location: time.UTC, want: "2026-01-01"},
		{name: "nil location falls back to utc", location: nil, want: "2026-01-01"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := NewGORMRepositoryWithLocation(nil, testCase.location)
			if got := repository.businessDate(instant); got != testCase.want {
				t.Fatalf("businessDate(%s) = %q, want %q", instant.Format(time.RFC3339), got, testCase.want)
			}
		})
	}
}

// TestBusinessDateDefaultTimezoneIsShanghai 验证默认业务日时区取 DashboardTimezone：
// 旧的 NewGORMRepository 会在时区加载失败时静默回落 UTC（fail-open），已删除；
// 现在构造必须显式传入启动期校验过的时区。
// 参数：t 测试上下文。
// 返回值：无。
func TestBusinessDateDefaultTimezoneIsShanghai(t *testing.T) {
	repository := NewGORMRepositoryWithLocation(nil, scopedContractBusinessLocation(t))
	if repository.location == nil || repository.location.String() != DashboardTimezone {
		t.Fatalf("default business location = %v, want %s", repository.location, DashboardTimezone)
	}
	instant := time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)
	if got := repository.businessDate(instant); got != "2026-01-02" {
		t.Fatalf("default businessDate(%s) = %q, want 2026-01-02", instant.Format(time.RFC3339), got)
	}
}

// TestScopedContractExpiryFollowsRepositoryTimezone 验证到期边界取自仓储业务日时区：
// 业务日当日到期不算过期，业务日前一天到期算过期；UTC 与配置时区分别成立。
// 参数：t 测试上下文。
// 返回值：无。
func TestScopedContractExpiryFollowsRepositoryTimezone(t *testing.T) {
	db := scopedContractFixture(t)
	const tenant, org = "tenant-timezone", "org-1"
	shanghai, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DashboardTimezone, err)
	}
	for _, location := range []*time.Location{time.UTC, shanghai} {
		today := time.Now().In(location).Format("2006-01-02")
		tenantID := tenant + "-" + location.String()
		insertScopedContract(t, db, "NORMAL", tenantID, "c-due-today", "active", today, "u1", org)
		insertScopedContract(t, db, "NORMAL", tenantID, "c-due-yesterday", "active", time.Now().In(location).AddDate(0, 0, -1).Format("2006-01-02"), "u1", org)
		repository := NewGORMRepositoryWithLocation(db, location)
		snapshot, available, err := repository.scopedContract(context.Background(), queryScope{mode: "ORG", tenantID: tenantID, orgUnitIDs: []string{org}})
		if err != nil || !available {
			t.Fatalf("%s: scoped contract read failed: available=%v err=%v", location, available, err)
		}
		if snapshot.ActiveContracts != 1 || snapshot.ExpiredContracts != 1 {
			t.Fatalf("%s: active=%d expired=%d, want active=1 expired=1", location, snapshot.ActiveContracts, snapshot.ExpiredContracts)
		}
	}
}
