package aggregation

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestBusinessUTCOffsetFormatsSignAndMinutes 验证业务时区偏移的 ±HH:MM 格式化。
// 参数：t 为测试上下文。
// 返回值：无；格式化不符合 MySQL CONVERT_TZ 目标时区语法时失败。
func TestBusinessUTCOffsetFormatsSignAndMinutes(t *testing.T) {
	shanghai, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, time.February, 1, 0, 30, 0, 0, shanghai)
	// 印度 +05:30 覆盖带分钟的半时区，卡死只打印小时、丢掉分钟的写法。
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		location *time.Location
		at       time.Time
		want     string
	}{
		{name: "shanghai", location: shanghai, at: instant, want: "+08:00"},
		{name: "utc", location: time.UTC, at: instant, want: "+00:00"},
		{name: "nil defaults to utc", location: nil, at: instant, want: "+00:00"},
		{name: "half hour offset", location: kolkata, at: instant, want: "+05:30"},
		{name: "negative offset", location: newYork, at: instant, want: "-05:00"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := BusinessUTCOffset(testCase.location, testCase.at); got != testCase.want {
				t.Fatalf("BusinessUTCOffset(%v) = %q, want %q", testCase.location, got, testCase.want)
			}
		})
	}
}

// TestConversionFactQueryBindsBusinessOffset 验证 period_month 的月前缀按业务时区
// 换算，且偏移是绑定参数而不是硬编码 +08:00。
// 参数：t 为测试上下文。
// 返回值：无；SQL 直接写死时区或缺少占位符时失败。
func TestConversionFactQueryBindsBusinessOffset(t *testing.T) {
	mysqlQuery := conversionFactQuery("mysql")
	if !strings.Contains(mysqlQuery, "DATE_FORMAT(CONVERT_TZ(o.created_at, '+00:00', ?), '%Y-%m')") {
		t.Fatalf("MySQL 月前缀未按业务时区换算或偏移不是绑定参数：\n%s", mysqlQuery)
	}
	if strings.Contains(mysqlQuery, "+08:00") {
		t.Fatalf("不得硬编码 +08:00：\n%s", mysqlQuery)
	}
	sqliteQuery := conversionFactQuery("sqlite")
	if !strings.Contains(sqliteQuery, "strftime('%Y-%m', o.created_at, ?)") {
		t.Fatalf("SQLite 月前缀未带业务时区偏移参数：\n%s", sqliteQuery)
	}
	// 分组复用 period_month 别名：月前缀表达式只出现一次，占位符与参数一一对应。
	for _, query := range []string{mysqlQuery, sqliteQuery} {
		if strings.Count(query, "DATE_FORMAT(CONVERT_TZ(o.created_at")+strings.Count(query, "strftime('%Y-%m', o.created_at, ?)") != 1 {
			t.Fatalf("月前缀表达式的占位符必须只出现一次：\n%s", query)
		}
		if !strings.Contains(query, "GROUP BY o.tenant_id, period_month") {
			t.Fatalf("分组应复用 period_month 别名：\n%s", query)
		}
	}
}

// TestConversionPeriodMonthUsesBusinessMonth 验证 period_month 按业务月归集：
// UTC 月末/月初的两条边界数据都落在北京时间 2026-02，UTC 边界前一刻数据落在 2026-01。
// 参数：t 为测试上下文。
// 返回值：无；归月口径与业务时区不一致时失败。
func TestConversionPeriodMonthUsesBusinessMonth(t *testing.T) {
	db := newScanTestDB(t)
	for _, statement := range []string{
		`CREATE TABLE dim_opportunity (tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE dim_contract (origin_kind TEXT DEFAULT 'NORMAL',tenant_id TEXT,contract_id TEXT,opportunity_id TEXT,status TEXT)`,
		`CREATE TABLE fct_opportunity_conversion (tenant_id TEXT,period_month TEXT,opportunity_count INTEGER,won_contract_count INTEGER,snapshot_at DATETIME,PRIMARY KEY(tenant_id,period_month))`,
		// 2026-01-31 15:30Z = 北京 2026-01-31 23:30 → 2026-01
		// 2026-01-31 16:30Z = 北京 2026-02-01 00:30 → 2026-02（UTC 归月会错到 2026-01）
		// 2026-02-01 00:30Z = 北京 2026-02-01 08:30 → 2026-02
		`INSERT INTO dim_opportunity VALUES
			('t',1,NULL,'投标','FOLLOWING','2026-01-31 15:30:00',NULL),
			('t',2,NULL,'投标','FOLLOWING','2026-01-31 16:30:00',NULL),
			('t',3,NULL,'投标','FOLLOWING','2026-02-01 00:30:00',NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	location, err := LoadBusinessLocation(DefaultBusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{aggDB: db, tenantID: "t", BusinessLocation: location}

	// 劫持实际执行的 SQL 与绑定变量，确认偏移作为第一个参数传入。
	var mu sync.Mutex
	var capturedSQL string
	var capturedVars []any
	if err := db.Callback().Raw().After("gorm:raw").Register("capture_conversion_fact", func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(tx.Statement.SQL.String(), "fct_opportunity_conversion") {
			capturedSQL = tx.Statement.SQL.String()
			capturedVars = append([]any(nil), tx.Statement.Vars...)
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.PrecomputeConversion(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sql, vars := capturedSQL, capturedVars
	mu.Unlock()
	if !strings.Contains(sql, "CONVERT_TZ") && !strings.Contains(sql, "strftime('%Y-%m', o.created_at, ?)") {
		t.Fatalf("转化事实写入未按业务时区换算月份：%s", sql)
	}
	if len(vars) != 3 || vars[0] != "+08:00" {
		t.Fatalf("绑定参数应为 [偏移, 快照时间, 租户]，实际 %#v", vars)
	}
	if _, ok := vars[1].(time.Time); !ok {
		t.Fatalf("第二个绑定参数应为快照时间，实际 %#v", vars[1])
	}

	var rows []struct {
		PeriodMonth string `gorm:"column:period_month"`
		Count       int64  `gorm:"column:opportunity_count"`
	}
	if err := db.Table("fct_opportunity_conversion").Order("period_month").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"2026-01": 1, "2026-02": 2}
	if len(rows) != len(want) {
		t.Fatalf("业务月分组结果 = %+v, want %v", rows, want)
	}
	for _, row := range rows {
		if want[row.PeriodMonth] != row.Count {
			t.Fatalf("period_month %q 计数 = %d, want %d", row.PeriodMonth, row.Count, want[row.PeriodMonth])
		}
	}
}

// TestConversionPeriodMonthFallsBackToUTCWithoutLocation 验证未注入业务时区时
// 偏移为 +00:00，既有调用方（含测试）的归月语义保持不变。
// 参数：t 为测试上下文。
// 返回值：无。
func TestConversionPeriodMonthFallsBackToUTCWithoutLocation(t *testing.T) {
	db := newScanTestDB(t)
	for _, statement := range []string{
		`CREATE TABLE dim_opportunity (tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE dim_contract (origin_kind TEXT DEFAULT 'NORMAL',tenant_id TEXT,contract_id TEXT,opportunity_id TEXT,status TEXT)`,
		`CREATE TABLE fct_opportunity_conversion (tenant_id TEXT,period_month TEXT,opportunity_count INTEGER,won_contract_count INTEGER,snapshot_at DATETIME,PRIMARY KEY(tenant_id,period_month))`,
		`INSERT INTO dim_opportunity VALUES ('t',1,NULL,'投标','FOLLOWING','2026-01-31 16:30:00',NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	var vars []any
	if err := db.Callback().Raw().After("gorm:raw").Register("capture_conversion_fact_utc", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "fct_opportunity_conversion") {
			vars = append([]any(nil), tx.Statement.Vars...)
		}
	}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{aggDB: db, tenantID: "t"}
	if err := runner.PrecomputeConversion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(vars) != 3 || vars[0] != "+00:00" {
		t.Fatalf("未注入业务时区时偏移应为 +00:00，实际 %#v", vars)
	}
}
