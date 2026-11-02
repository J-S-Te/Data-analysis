package aggregation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
)

// TestLoadBusinessLocationDefaultsAndRejectsInvalid 验证业务日时区的默认值与
// fail-closed 校验：未配置取 Asia/Shanghai，非法 IANA 名返回错误而不是静默退回 UTC。
// 参数：t 为测试上下文。
// 返回值：无；默认值或拒绝行为不符时失败。
func TestLoadBusinessLocationDefaultsAndRejectsInvalid(t *testing.T) {
	for _, name := range []string{"", "   "} {
		location, err := LoadBusinessLocation(name)
		if err != nil {
			t.Fatalf("LoadBusinessLocation(%q) error = %v", name, err)
		}
		if location.String() != DefaultBusinessTimezone {
			t.Fatalf("LoadBusinessLocation(%q) = %v, want %s", name, location, DefaultBusinessTimezone)
		}
	}
	for _, name := range []string{"Mars/Olympus_Mons", "Asia/Shanghai "} {
		if _, err := LoadBusinessLocation(name); err == nil {
			t.Fatalf("LoadBusinessLocation(%q) error = nil, want invalid timezone rejection", name)
		}
	}
	if location, err := LoadBusinessLocation("America/New_York"); err != nil || location.String() != "America/New_York" {
		t.Fatalf("explicit timezone = %v, %v; want America/New_York", location, err)
	}
}

// TestFinanceDateBoundariesFollowBusinessTimezone 验证 as_of_date、90 天 cutoff 与
// 本月前缀都取自注入的业务日时区。
// 参数：t 为测试上下文。
// 返回值：无；任一日期边界与业务时区不符时失败。
func TestFinanceDateBoundariesFollowBusinessTimezone(t *testing.T) {
	shanghai, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DefaultBusinessTimezone, err)
	}
	// UTC 2026-01-01 16:30 在上海已是 2026-01-02 00:30，正是 UTC 业务日滞后的窗口。
	dayBoundary := time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)
	// UTC 2026-01-31 16:30 在上海已是 2026-02-01，跨月窗口。
	monthBoundary := time.Date(2026, 1, 31, 16, 30, 0, 0, time.UTC)
	cases := []struct {
		name       string
		runner     *Runner
		instant    time.Time
		wantAsOf   string
		wantCutoff string
		wantMonth  string
	}{
		{
			name:     "shanghai day boundary",
			runner:   &Runner{BusinessLocation: shanghai},
			instant:  dayBoundary,
			wantAsOf: "2026-01-02", wantCutoff: "2025-10-04", wantMonth: "2026-01",
		},
		{
			name:     "utc day boundary",
			runner:   &Runner{BusinessLocation: time.UTC},
			instant:  dayBoundary,
			wantAsOf: "2026-01-01", wantCutoff: "2025-10-03", wantMonth: "2026-01",
		},
		{
			name:     "nil location falls back to utc",
			runner:   &Runner{},
			instant:  dayBoundary,
			wantAsOf: "2026-01-01", wantCutoff: "2025-10-03", wantMonth: "2026-01",
		},
		{
			name:     "shanghai month boundary",
			runner:   &Runner{BusinessLocation: shanghai},
			instant:  monthBoundary,
			wantAsOf: "2026-02-01", wantCutoff: "2025-11-03", wantMonth: "2026-02",
		},
		{
			name:     "utc month boundary",
			runner:   &Runner{BusinessLocation: time.UTC},
			instant:  monthBoundary,
			wantAsOf: "2026-01-31", wantCutoff: "2025-11-02", wantMonth: "2026-01",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			location := testCase.runner.businessLocation()
			asOf, cutoff, monthStart := financeDateBoundaries(testCase.instant.In(location))
			if asOf != testCase.wantAsOf || cutoff != testCase.wantCutoff {
				t.Fatalf("as_of_date/cutoff = %q/%q, want %q/%q", asOf, cutoff, testCase.wantAsOf, testCase.wantCutoff)
			}
			if got := monthStart.Format("2006-01"); got != testCase.wantMonth {
				t.Fatalf("month = %q, want %q", got, testCase.wantMonth)
			}
			if monthStart.Location() != location {
				t.Fatalf("monthStart location = %v, want %v", monthStart.Location(), location)
			}
		})
	}
}

// financeFixture 建立财务快照的最小 SQLite 源表：两笔到期日跨 cutoff 的应收
// 和一笔本月内回款，用于区分业务时区与 UTC 的日期边界。
// 参数：t 为测试上下文。
// 返回值：已建表并写入夹具行的内存库连接。
func financeFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db := newScanTestDB(t)
	for _, sql := range []string{
		`CREATE TABLE settlement_receivable (tenant_id TEXT,currency TEXT,original_amount DECIMAL,received_allocated_amount DECIMAL,open_amount DECIMAL,due_date TEXT,recognition_status TEXT)`,
		`CREATE TABLE settlement_receipt (tenant_id TEXT,currency TEXT,amount DECIMAL,receipt_date TEXT,status TEXT)`,
		// 上海 cutoff 2025-10-04 时 2025-10-03 已逾期；UTC cutoff 2025-10-03 时两笔都不算逾期。
		`INSERT INTO settlement_receivable VALUES ('a','CNY',100,0,100,'2025-10-03','CONFIRMED'),('a','CNY',200,0,200,'2025-10-04','CONFIRMED')`,
		`INSERT INTO settlement_receipt VALUES ('a','CNY',70,'2026-01-15','AVAILABLE')`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// financeMetrics 是财务快照指标的反序列化视图。
type financeMetrics struct {
	Month      string `json:"month"`
	AsOfDate   string `json:"as_of_date"`
	Currencies []struct {
		Currency string `json:"currency"`
		Over90   int64  `json:"over_90_minor"`
		Receipts int64  `json:"month_receipts_minor"`
	} `json:"currencies"`
}

func parseFinanceMetrics(t *testing.T, snapshot domain.BusinessSnapshot) financeMetrics {
	t.Helper()
	raw, err := json.Marshal(snapshot.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	var metrics financeMetrics
	if err = json.Unmarshal(raw, &metrics); err != nil {
		t.Fatal(err)
	}
	return metrics
}

// TestFinanceStatisticsUsesBusinessTimezoneCutoff 验证 90 天账龄截止日按业务日时区：
// 同一时刻上海业务日 2026-01-02（cutoff 2025-10-04）比 UTC（cutoff 2025-10-03）多算一天逾期。
// 参数：t 为测试上下文。
// 返回值：无；as_of_date、over_90 余额或 Basis 文案与业务时区不符时失败。
func TestFinanceStatisticsUsesBusinessTimezoneCutoff(t *testing.T) {
	shanghai, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DefaultBusinessTimezone, err)
	}
	instant := time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)
	cases := []struct {
		name       string
		location   *time.Location
		wantAsOf   string
		wantOver90 int64
	}{
		{name: "shanghai business day", location: shanghai, wantAsOf: "2026-01-02", wantOver90: 10000},
		{name: "utc", location: time.UTC, wantAsOf: "2026-01-01", wantOver90: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, err := financeStatistics(context.Background(), financeFixture(t), "a", instant.In(testCase.location))
			if err != nil {
				t.Fatal(err)
			}
			metrics := parseFinanceMetrics(t, snapshot)
			if metrics.AsOfDate != testCase.wantAsOf || metrics.Month != "2026-01" {
				t.Fatalf("as_of_date/month = %q/%q, want %q/2026-01", metrics.AsOfDate, metrics.Month, testCase.wantAsOf)
			}
			if len(metrics.Currencies) != 1 || metrics.Currencies[0].Over90 != testCase.wantOver90 {
				t.Fatalf("over_90_minor = %+v, want %d", metrics.Currencies, testCase.wantOver90)
			}
			if strings.Contains(snapshot.Basis, "UTC业务日期") {
				t.Fatalf("basis still claims UTC business day: %q", snapshot.Basis)
			}
			if !strings.Contains(snapshot.Basis, "DASHBOARD_TIMEZONE") {
				t.Fatalf("basis must state the business day timezone: %q", snapshot.Basis)
			}
		})
	}
}

// TestFinanceStatisticsUsesBusinessTimezoneMonth 验证“本月”前缀与回款区间按业务日时区：
// UTC 2026-01-31 16:30 在上海已是 2026-02-01，本月回款区间随之进入 2 月。
// 参数：t 为测试上下文。
// 返回值：无；month 或 month_receipts_minor 与业务时区不符时失败。
func TestFinanceStatisticsUsesBusinessTimezoneMonth(t *testing.T) {
	shanghai, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DefaultBusinessTimezone, err)
	}
	instant := time.Date(2026, 1, 31, 16, 30, 0, 0, time.UTC)
	cases := []struct {
		name        string
		location    *time.Location
		wantMonth   string
		wantReceipt int64
	}{
		{name: "shanghai february", location: shanghai, wantMonth: "2026-02", wantReceipt: 0},
		{name: "utc january", location: time.UTC, wantMonth: "2026-01", wantReceipt: 7000},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, err := financeStatistics(context.Background(), financeFixture(t), "a", instant.In(testCase.location))
			if err != nil {
				t.Fatal(err)
			}
			metrics := parseFinanceMetrics(t, snapshot)
			if metrics.Month != testCase.wantMonth {
				t.Fatalf("month = %q, want %q", metrics.Month, testCase.wantMonth)
			}
			if len(metrics.Currencies) != 1 || metrics.Currencies[0].Receipts != testCase.wantReceipt {
				t.Fatalf("month_receipts_minor = %+v, want %d", metrics.Currencies, testCase.wantReceipt)
			}
		})
	}
}

// TestStoreBusinessKeepsUTCSnapshotTimestamp 验证业务日时区只影响日期边界，
// 快照时间戳仍以 UTC 落库（禁止把业务日时区写进存储口径）。
// 参数：t 为测试上下文。
// 返回值：无；snapshot_at 不是 UTC 时失败。
func TestStoreBusinessKeepsUTCSnapshotTimestamp(t *testing.T) {
	shanghai, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DefaultBusinessTimezone, err)
	}
	db := newScanTestDB(t)
	if err = db.AutoMigrate(&businessRow{}); err != nil {
		t.Fatal(err)
	}
	runner := Runner{aggDB: db, tenantID: "a", BusinessLocation: shanghai}
	before := time.Now().UTC().Add(-time.Minute)
	if err = runner.storeBusiness(context.Background(), "finance", domain.BusinessSnapshot{}); err != nil {
		t.Fatal(err)
	}
	var row businessRow
	if err = db.Take(&row, "tenant_id = ? AND section = ?", "a", "finance").Error; err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SnapshotAt string `json:"snapshot_at"`
	}
	if err = json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(payload.SnapshotAt, "Z") {
		t.Fatalf("snapshot_at must stay UTC RFC3339, got %q", payload.SnapshotAt)
	}
	stored, err := time.Parse(time.RFC3339Nano, payload.SnapshotAt)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Location() != time.UTC || stored.Before(before) {
		t.Fatalf("snapshot_at = %v (loc %v), want UTC not before %v", stored, stored.Location(), before)
	}
}
