package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestLatestContractReadsCurrencyTotalsOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DASHBOARD_CURRENCY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("DASHBOARD_CURRENCY_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	tenant := "qa-contract-currency"
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM api_contract_dashboard WHERE tenant_id=?`, tenant).Error })
	if err := db.Exec(`DELETE FROM api_contract_dashboard WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO api_contract_dashboard(tenant_id,snapshot_at,total_amount_minor,currency_totals_json,total_contracts,approval_contracts,active_contracts,expired_contracts) VALUES(?,?,?,?,?,?,?,?)`,
		tenant, time.Now().UTC(), 880000, `[{"currency":"CNY","amount_minor":100000,"contracts":1},{"currency":"USD","amount_minor":780000,"contracts":7}]`, 8, 2, 4, 1).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, available, err := NewGORMRepository(db).LatestContract(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(snapshot.CurrencyTotals) != 2 || snapshot.CurrencyTotals[0].Currency != "CNY" || snapshot.CurrencyTotals[1].AmountMinor != 780000 {
		t.Fatalf("snapshot=%+v available=%v", snapshot, available)
	}
}
