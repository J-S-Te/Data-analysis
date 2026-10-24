package infrastructure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestTrendKeepsOnlyLatestProjectSnapshotPerMonthOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DASHBOARD_TREND_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("DASHBOARD_TREND_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	tenant := "qa-trend-tenant"
	if err := db.Exec(`DELETE FROM api_project_dashboard WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM api_project_dashboard WHERE tenant_id=?`, tenant).Error })
	now := time.Now().UTC()
	for month := 0; month < 3; month++ {
		base := now.AddDate(0, -month, 0)
		for i := 0; i < 40; i++ {
			snapshot := base.Add(-time.Duration(i) * time.Hour)
			if err := db.Exec(`INSERT INTO api_project_dashboard(tenant_id,snapshot_at,project_count,status_counts_json) VALUES(?,?,?,?)`, tenant, snapshot, 100*(month+1)+i, "{}").Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := NewGORMRepository(db).ListTrend(context.Background(), tenant, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("trend rows=%d %+v, want exactly 2 bounded months", len(rows), rows)
	}
	for _, row := range rows {
		if row.ProjectCount == 0 {
			t.Fatalf("row=%+v missing latest project count", row)
		}
	}
	_ = fmt.Sprint(rows)
}

func TestTrendPreservesContractCurrenciesOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DASHBOARD_TREND_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("DASHBOARD_TREND_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	tenant := "qa-trend-currency"
	now := time.Now().UTC()
	period := now.Format("2006-01")
	for _, table := range []string{"fct_contract_signing", "api_project_dashboard"} {
		if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM fct_contract_signing WHERE tenant_id=?`, tenant).Error
		_ = db.Exec(`DELETE FROM api_project_dashboard WHERE tenant_id=?`, tenant).Error
	})
	for _, row := range []struct {
		currency      string
		amount, count int64
	}{{"CNY", 10000, 1}, {"USD", 20000, 2}} {
		if err := db.Exec(`INSERT INTO fct_contract_signing(tenant_id,period_month,region,customer_category,sales_director_id,sales_id,sign_amount_minor,currency,contract_count,opportunity_count,won_contract_count,discount_rate_bucket,metric_dict_version) VALUES(?,?,'未知','未分类','','sales',?,?,?,0,0,'未知','v1')`, tenant, period, row.amount, row.currency, row.count).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := NewGORMRepository(db).ListTrend(context.Background(), tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].CurrencyTotals) != 2 || rows[0].CurrencyTotals[0].Currency != "CNY" || rows[0].CurrencyTotals[1].AmountMinor != 20000 {
		t.Fatalf("trend=%+v", rows)
	}
}
