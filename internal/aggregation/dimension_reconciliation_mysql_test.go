package aggregation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestDimensionSnapshotRealMySQLRepeatableRead 验证真实MySQL的跨批快照、只读约束和删除对账。
// 参数：t 为测试上下文，必须显式提供回环隔离DSN文件，不发现或回退业务库。
// 返回值：无；中途来源变更泄漏入快照、跨租户删除或只读约束缺失时失败。
func TestDimensionSnapshotRealMySQLRepeatableRead(t *testing.T) {
	dsn := importPrivateDSN(t, "UIP_BI_DIM_SYNC_TEST_DSN_FILE")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open explicit isolated DB")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant := "reconcile-mysql-tenant"
	other := "reconcile-mysql-other"
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS dimension_reconciliation_fixture (tenant_id VARCHAR(64),id VARCHAR(64),PRIMARY KEY(tenant_id,id)) ENGINE=InnoDB`).Error; err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		for _, table := range []string{"dimension_reconciliation_fixture", "dimension_sync_key", "dim_contract_approval", "api_project_dashboard"} {
			if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id IN (?,?)", tenant, other).Error; err != nil {
				t.Error(err)
			}
		}
	}
	cleanup()
	defer cleanup()
	for start := 1; start <= 501; start += 100 {
		rows := make([]map[string]any, 0, 100)
		for i := start; i < start+100 && i <= 501; i++ {
			rows = append(rows, map[string]any{"tenant_id": tenant, "id": fmt.Sprintf("p%04d", i)})
		}
		if err := db.Table("dimension_reconciliation_fixture").Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for _, row := range []struct{ tenant, id string }{{tenant, "p0501"}, {tenant, "gone"}, {other, "gone"}} {
		if err := db.Exec(dimContractApprovalUpsert, row.tenant, "c", row.id, "contract_approval", "running", now, nil, now).Error; err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{aggDB: db, tenantID: tenant}
	batches := 0
	err = r.runDimensionSnapshot(ctx, db, ContractApprovalSource, func(src *gorm.DB, run string) error {
		if writeErr := src.Session(&gorm.Session{NewDB: true}).Exec(
			"UPDATE dimension_reconciliation_fixture SET id=id WHERE tenant_id=?", tenant,
		).Error; writeErr == nil {
			return fmt.Errorf("source snapshot transaction allowed write")
		}
		return scanSourceBatches(src, "dimension_reconciliation_fixture", "id", "id", func(row struct{ ID string }) string { return row.ID }, 500, func(rows []struct{ ID string }) error {
			batches++
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, approvalReconciliationKey("c", row.ID))
			}
			if err := r.stageDimensionKeys(ctx, run, "dim_contract_approval", keys); err != nil {
				return err
			}
			if batches == 1 {
				if err := db.Exec("DELETE FROM dimension_reconciliation_fixture WHERE tenant_id=? AND id='p0501'", tenant).Error; err != nil {
					return err
				}
				if err := db.Exec("INSERT INTO dimension_reconciliation_fixture VALUES (?,'p9999')", tenant).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := db.Table("dim_contract_approval").Where("tenant_id=?", tenant).Pluck("approval_id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if batches != 2 || len(ids) != 1 || ids[0] != "p0501" {
		t.Fatalf("RR snapshot batches=%d retained=%v", batches, ids)
	}
	var count int64
	db.Table("dim_contract_approval").Where("tenant_id=?", other).Count(&count)
	if count != 1 {
		t.Fatal("other tenant deleted")
	}
	for i := 0; i < 3; i++ {
		if err := db.Exec(`INSERT INTO api_project_dashboard
			(tenant_id,snapshot_at,project_count,in_flight_projects,risk_projects,service_items,status_counts_json,source)
			VALUES (?, ?, 0, 0, 0, 0, JSON_OBJECT(), 'retention-test')`, tenant, now.AddDate(-5, 0, 0)).Error; err != nil {
			t.Fatal(err)
		}
	}
	var latest uint64
	if err := db.Table("api_project_dashboard").Where("tenant_id=?", tenant).Select("MAX(id)").Scan(&latest).Error; err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshotHistory(ctx, db, tenant, "api_project_dashboard", now, time.UTC, SnapshotRetentionPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	var retained []uint64
	if err := db.Table("api_project_dashboard").Where("tenant_id=?", tenant).Pluck("id", &retained).Error; err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained[0] != latest {
		t.Fatalf("MySQL retention removed latest or retained redundant snapshots: %v", retained)
	}
}
