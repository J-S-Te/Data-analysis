package importreport

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestHistoryRealMySQLOriginalDatesCurrenciesAndSelf 验证历史分栏真实MySQL的日期、币种及个人边界。
// 参数：t 为测试上下文，测试库必须预先运行实际迁移。
// 返回值：无，日期、币种及范围均依据真实SQL结果，不采用模拟数据库结论。
func TestHistoryRealMySQLOriginalDatesCurrenciesAndSelf(t *testing.T) {
	file := os.Getenv("UIP_IMPORT_REPORT_TEST_DSN_FILE")
	if file == "" {
		t.Skip("explicit isolated DSN required")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("read private DSN")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("dedicated loopback test schema required")
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated history DB")
	}
	pool, _ := db.DB()
	defer pool.Close()
	tenant := "history-scope-qa"
	db.Where("tenant_id=?", tenant).Delete(&aggregation.ImportHistory{})
	db.Where("tenant_id=?", tenant).Delete(&aggregation.ImportReportRun{})
	defer db.Where("tenant_id=?", tenant).Delete(&aggregation.ImportHistory{})
	defer db.Where("tenant_id=?", tenant).Delete(&aggregation.ImportReportRun{})
	date := "2020-01-31"
	for _, row := range []aggregation.ImportHistory{
		{TenantID: tenant, ItemID: "a", StartDate: &date, Currency: "CNY", AmountMinor: 100, OwnerUserID: "u", OwnerOrgID: "o", ImportMode: "ARCHIVE_ONLY", ReportMode: "ARCHIVE_SEPARATE", Status: "completed", PayloadJSON: "{}"},
		{TenantID: tenant, ItemID: "b", StartDate: &date, Currency: "USD", AmountMinor: 250, OwnerUserID: "u", OwnerOrgID: "o", ImportMode: "ARCHIVE_ONLY", ReportMode: "ARCHIVE_SEPARATE", Status: "completed", PayloadJSON: "{}"},
		{TenantID: tenant, ItemID: "c", Currency: "USD", AmountMinor: 300, OwnerUserID: "v", OwnerOrgID: "p", ImportMode: "HISTORICAL_CLOSED", ReportMode: "HISTORICAL_SEPARATE", Status: "completed", PayloadJSON: "{}"},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := db.Create(&aggregation.ImportReportRun{TenantID: tenant, LastStatus: "SUCCESS", LastSuccessAt: &now, PublishedRunID: "import-1"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"TENANT", "ORG", "TEAM", "SELF"} {
		p := auth.Principal{TenantID: tenant, UserID: "u", ScopeMode: mode, OrganizationIDs: []string{"o"}, TeamIDs: []string{"o"}}
		result, err := History(auth.WithPrincipal(context.Background(), p), db)
		want := 2
		if mode == "TENANT" {
			want = 3
		}
		if err != nil || result.Stale || len(result.Periods) != want {
			t.Fatalf("%s history %+v %v", mode, result, err)
		}
		for _, row := range result.Periods {
			if row.Period != "2020-01" && row.Period != "UNASSIGNED" {
				t.Fatal("original month changed")
			}
			if row.AmountMinor != 100 && row.AmountMinor != 250 && row.AmountMinor != 300 {
				t.Fatal("currencies mixed")
			}
		}
	}
}
