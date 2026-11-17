package aggregation

import (
	"context"
	"testing"

	"gorm.io/gorm"
)

// recordDimensionFixture 用途：准备明确隔离的来源与聚合库，只写入一份合成合同。
// 参数：t 为最小场景上下文，连接复用既有私有文件、回环地址及库名守卫。
// 返回值：真实聚合器、来源连接与连接串，不访问真实合同服务或业务库。
func recordDimensionFixture(t *testing.T) (*Runner, *gorm.DB, string) {
	t.Helper()
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_TEST_DSN_FILE", true)
	src, dsn := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_SOURCE_DSN_FILE", false)
	dashboardAcceptanceSourceTables(t, src)
	columns, err := src.Migrator().ColumnTypes("con_contract")
	if err != nil {
		t.Fatal(err)
	}
	hasMarker := false
	for _, column := range columns {
		if column.Name() == "deleted_at" {
			hasMarker = true
		}
	}
	if !hasMarker {
		dashboardAcceptanceExec(t, src, "ALTER TABLE con_contract ADD COLUMN deleted_at DATETIME(3) NULL")
	}
	dashboardAcceptanceCleanup(t, db, []string{"dim_contract", "dimension_sync_key"})
	dashboardAcceptanceCleanup(t, src, []string{"con_contract"})
	t.Cleanup(func() {
		dashboardAcceptanceCleanup(t, db, []string{"dim_contract", "dimension_sync_key"})
		dashboardAcceptanceCleanup(t, src, []string{"con_contract"})
	})
	dashboardAcceptanceExec(t, src, `INSERT INTO con_contract
	 (tenant_id,id,origin_kind,contract_number,title,contract_type,service_type,status,
	 amount_minor,currency,owner_user_id,owner_org_id,updated_at)
	 VALUES (?, 'record-source-1','NORMAL','TEST001','合成合同','直签','软件测试','draft',
	 10000,'CNY','test-owner','test-org',UTC_TIMESTAMP(3))`, acceptanceTenant)
	return &Runner{aggDB: db, tenantID: acceptanceTenant}, src, dsn
}

// recordDimensionCount 用途：核对当前测试租户在已发布维表中的真实行数。
// 参数：t/r 为场景与真实聚合器，want 为本步骤精确预期。
// 返回值：无；读取失败或对账没有发布时立即失败。
func recordDimensionCount(t *testing.T, r *Runner, want int64) {
	t.Helper()
	var count int64
	if err := r.aggDB.Table("dim_contract").Where("tenant_id=?", acceptanceTenant).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("published contract rows=%d want=%d", count, want)
	}
}

// TestContractRecordDimensionRoundTripMySQL 用途：验证删除最后一项完整清空，恢复后重新发布。
// 参数：t 为一个合同生命周期的隔离 MySQL 最小场景。
// 返回值：无；stage载荷和keys不一致、空集合未对账或恢复缺项时失败。
func TestContractRecordDimensionRoundTripMySQL(t *testing.T) {
	r, src, dsn := recordDimensionFixture(t)
	ctx := context.Background()
	if err := r.syncContract(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	recordDimensionCount(t, r, 1)
	dashboardAcceptanceExec(t, src, "UPDATE con_contract SET deleted_at=UTC_TIMESTAMP(3) WHERE tenant_id=?", acceptanceTenant)
	if err := r.syncContract(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	recordDimensionCount(t, r, 0)
	dashboardAcceptanceExec(t, src, "UPDATE con_contract SET deleted_at=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=?", acceptanceTenant)
	if err := r.syncContract(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	recordDimensionCount(t, r, 1)
}

// TestContractRecordDimensionSchemaFailureMySQL 用途：验证来源结构探测失败保留最近成功发布。
// 参数：t 为独立的隔离 MySQL 最小故障场景，临时改名仅影响专用来源测试表。
// 返回值：无；错误被吞成空集或旧维表被清理时失败。
func TestContractRecordDimensionSchemaFailureMySQL(t *testing.T) {
	r, src, dsn := recordDimensionFixture(t)
	if err := r.syncContract(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	dashboardAcceptanceExec(t, src, "RENAME TABLE con_contract TO con_contract_record_hidden")
	t.Cleanup(func() { dashboardAcceptanceExec(t, src, "RENAME TABLE con_contract_record_hidden TO con_contract") })
	if err := r.syncContract(context.Background(), dsn); err == nil {
		t.Fatal("source schema failure was swallowed")
	}
	recordDimensionCount(t, r, 1)
}
