package aggregation

import (
	"context"
	"testing"
)

// TestImportedHistoryMetricsPolicy 验证历史和未知来源不会成为普通赢单依据。
// 参数：t 为测试上下文。
// 返回值：无，普通生效合同可去重计入，历史/未知/未签约关联不得计入。
func TestImportedHistoryMetricsPolicy(t *testing.T) {
	db := newScanTestDB(t)
	for _, q := range []string{
		`CREATE TABLE dim_opportunity(tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE dim_contract(tenant_id TEXT,contract_id TEXT,opportunity_id TEXT,status TEXT,origin_kind TEXT)`,
		`CREATE TABLE fct_opportunity_conversion(tenant_id TEXT,period_month TEXT,opportunity_count INTEGER,won_contract_count INTEGER,snapshot_at DATETIME)`,
		`INSERT INTO dim_opportunity VALUES ('t',1,'c1','已签约','ACTIVE','2020-01-01',NULL),('t',2,'c2','已签约','ACTIVE','2020-01-01',NULL),('t',3,'c3','已签约','ACTIVE','2020-01-01',NULL),('t',4,'c4','谈判','ACTIVE','2020-01-01',NULL)`,
		`INSERT INTO dim_contract VALUES ('t','c1','1','active','NORMAL'),('t','c11','1','completed','NORMAL'),('t','c2','2','completed','IMPORTED'),('t','c3','3','active','UNKNOWN'),('t','c4','4','active','NORMAL')`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{aggDB: db, tenantID: "t"}
	if err := r.PrecomputeConversion(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row struct {
		OpportunityCount int
		WonContractCount int
	}
	if err := db.Table("fct_opportunity_conversion").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.OpportunityCount != 4 || row.WonContractCount != 1 {
		t.Fatalf("普通商机队列应4项/1赢单，实际 %+v", row)
	}
}
