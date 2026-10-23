package aggregation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestFinanceStatisticsActualBalancesCurrencyAndMonth(t *testing.T) {
	db := newScanTestDB(t)
	for _, sql := range []string{
		`CREATE TABLE settlement_receivable (tenant_id TEXT,currency TEXT,original_amount DECIMAL,received_allocated_amount DECIMAL,open_amount DECIMAL,due_date TEXT,recognition_status TEXT)`,
		`CREATE TABLE settlement_receipt (tenant_id TEXT,currency TEXT,amount DECIMAL,receipt_date TEXT,status TEXT)`,
		`INSERT INTO settlement_receivable VALUES ('a','CNY',1000,200,800,'2026-01-01','CONFIRMED'),('a','USD',200,100,100,'2026-10-09','CONFIRMED'),('other','CNY',9999,9999,0,'2026-01-01','CONFIRMED'),('a','CNY',9999,0,9999,'2026-01-01','PENDING')`,
		`INSERT INTO settlement_receipt VALUES ('a','CNY',10.01,'2026-10-01','AVAILABLE'),('a','CNY',20,'2026-10-05','FULLY_ALLOCATED'),('a','CNY',100,'2026-09-30','AVAILABLE'),('a','CNY',9999,'2026-10-05','VOID'),('other','CNY',9999,'2026-10-05','AVAILABLE')`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	s, err := financeStatistics(context.Background(), db, "a", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s.Metrics)
	var value struct {
		Currencies []struct {
			Currency  string   `json:"currency"`
			Original  int64    `json:"original_minor"`
			Allocated int64    `json:"allocated_minor"`
			Over90    int64    `json:"over_90_minor"`
			Receipts  int64    `json:"month_receipts_minor"`
			Rate      *float64 `json:"collection_rate"`
		} `json:"currencies"`
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Currencies) != 2 {
		t.Fatalf("currency separation: %s", raw)
	}
	cny, usd := value.Currencies[0], value.Currencies[1]
	if cny.Original != 100000 || cny.Allocated != 20000 || cny.Over90 != 80000 || cny.Receipts != 3001 || cny.Rate == nil || *cny.Rate != 20 || usd.Over90 != 0 || *usd.Rate != 50 {
		t.Fatalf("balances/month/tenant: %s", raw)
	}
	if len(s.MissingMetrics) != 2 {
		t.Fatal("must not invent recognized income or cost")
	}
}

func TestReportStatisticsMedianVoidNullAndTenant(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.Exec(`CREATE TABLE pm_report_revision (id INTEGER,tenant_id TEXT,status TEXT,validity_status TEXT,prepared_at DATETIME,issued_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO pm_report_revision VALUES (1,'a','ISSUED','ACTIVE','2026-10-01','2026-10-02'),(2,'a','ARCHIVED','ACTIVE','2026-10-01','2026-10-04'),(3,'a','ISSUED','VOID','2026-10-01','2026-10-10'),(4,'a','COMPILING','ACTIVE',NULL,NULL),(5,'other','ISSUED','ACTIVE','2026-10-01','2026-10-30'),(6,'a','ISSUED','ACTIVE',NULL,'2026-10-10'),(7,'a','ISSUED','ACTIVE','2026-10-10','2026-10-09')`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	s, err := reportStatistics(context.Background(), db, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Metrics["cycle_samples"] != int64(2) || *s.Metrics["cycle_p50_days"].(*float64) != 2 || s.Metrics["active_reports"] != int64(5) || s.Metrics["void_reports"] != int64(1) {
		t.Fatalf("median/status: %#v", s.Metrics)
	}
	empty, err := reportStatistics(context.Background(), db, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Metrics["cycle_p50_days"].(*float64) != nil {
		t.Fatal("no samples must remain null")
	}
	if err = db.Exec("DROP TABLE pm_report_revision").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = reportStatistics(context.Background(), db, "a"); err == nil {
		t.Fatal("missing source must fail, never publish success")
	}
}

func TestBusinessSnapshotPersistenceLicenseAndAtomicFailure(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&businessRow{}); err != nil {
		t.Fatal(err)
	}
	r := Runner{aggDB: db, tenantID: "a"}
	value, err := reportStatistics(context.Background(), db, "a")
	if err == nil {
		t.Fatal("source table should not exist")
	}
	_ = value
	r.LicenseCheck = func(context.Context) error { return errors.New("expired") }
	if err = r.storeBusiness(context.Background(), "report", value); err == nil {
		t.Fatal("expired license must not persist")
	}
	var count int64
	if err = db.Model(&businessRow{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("failed source must not create snapshot")
	}
}

func TestConversionDistinctAuthoritativeLinkAndReplacement(t *testing.T) {
	db := newScanTestDB(t)
	for _, sql := range []string{
		`CREATE TABLE dim_opportunity (tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE dim_contract (tenant_id TEXT,contract_id TEXT,opportunity_id TEXT,status TEXT)`,
		`CREATE TABLE fct_opportunity_conversion (tenant_id TEXT,period_month TEXT,opportunity_count INTEGER,won_contract_count INTEGER,snapshot_at DATETIME,PRIMARY KEY(tenant_id,period_month))`,
		`INSERT INTO dim_opportunity VALUES ('a',1,'c1','已签约','CLOSED','2026-10-01',NULL),('a',2,NULL,'投标','FOLLOWING','2026-10-02',NULL),('a',3,'c3','已签约','CLOSED','2026-10-03',NULL),('a',4,NULL,'已签约','VOID','2026-10-04',NULL),('a',5,NULL,'报价','FOLLOWING','2026-10-04','2026-10-05'),('other',1,'c1','已签约','CLOSED','2026-10-01',NULL)`,
		`INSERT INTO dim_contract VALUES ('a','c1','1','active'),('a','c2','1','completed'),('a','c3','3','draft'),('other','c3','3','active')`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	r := Runner{aggDB: db, tenantID: "a"}
	for i := 0; i < 2; i++ {
		if err := r.PrecomputeConversion(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var row struct {
		OpportunityCount int64
		WonContractCount int64
	}
	if err := db.Table("fct_opportunity_conversion").Where("tenant_id='a'").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.OpportunityCount != 3 || row.WonContractCount != 1 {
		t.Fatalf("must count opportunities, not contracts: %+v", row)
	}
	if err := db.Exec("UPDATE dim_opportunity SET opp_status='VOID' WHERE tenant_id='a'").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.PrecomputeConversion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("fct_opportunity_conversion").Where("tenant_id='a'").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.OpportunityCount != 0 || row.WonContractCount != 0 {
		t.Fatal("old nonzero fact must be replaced")
	}
}
