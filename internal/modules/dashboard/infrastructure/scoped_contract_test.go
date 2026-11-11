package infrastructure

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// scopedContractFixture 建立只属于本用例的内存维表夹具。
// 参数：t 为测试上下文。
// 返回值：与生产列名一致的 GORM 连接。
func scopedContractFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, ddl := range []string{
		`CREATE TABLE dim_contract (origin_kind TEXT DEFAULT 'NORMAL',
			tenant_id TEXT, contract_id TEXT, contract_number TEXT, title TEXT, status TEXT,
			amount_minor INTEGER, currency TEXT, end_date DATE, start_date DATE,
			owner_user_id TEXT, owner_org_id TEXT, owner_identity_id TEXT, crm_customer_id INTEGER,
			opportunity_id TEXT, project_id TEXT, updated_at DATETIME)`,
		`CREATE TABLE sync_source (
			id TEXT, tenant_id TEXT, subsystem_code TEXT, db_schema TEXT, enabled BOOLEAN,
			last_success_at DATETIME, last_watermark DATETIME, last_run_at DATETIME, last_status TEXT, last_error TEXT, updated_at DATETIME)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// insertScopedContract 写入一条维表合同行。
// 参数：db 为夹具连接，origin 为来源类型，tenant 为租户，contract 为合同标识，status 为源状态词，endDate 为到期日（空串表示 NULL），owner 为责任人，org 为组织。
// 返回值：无，写入失败终止用例。
func insertScopedContract(t *testing.T, db *gorm.DB, origin, tenant, contract, status, endDate, owner, org string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO sync_source(id,tenant_id,subsystem_code,enabled,last_status,last_success_at,updated_at)
 SELECT ?,?,'contract_management',1,'SUCCESS',?,? WHERE NOT EXISTS(SELECT 1 FROM sync_source WHERE tenant_id=? AND subsystem_code='contract_management')`, "contract-"+tenant, tenant, time.Now().UTC(), time.Now().UTC(), tenant).Error; err != nil {
		t.Fatal(err)
	}
	var end any
	if endDate != "" {
		end = endDate
	}
	if err := db.Exec(`INSERT INTO dim_contract(origin_kind,tenant_id,contract_id,contract_number,title,status,amount_minor,currency,end_date,start_date,owner_user_id,owner_org_id,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		origin, tenant, contract, contract, "合成合同", status, 100, "CNY", end, "2026-01-01", owner, org, time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
}

// scopedContractBusinessLocation 返回与生产默认一致的业务日时区。
// 参数：t 为测试上下文。
// 返回值：Asia/Shanghai 时区对象；缺失 tzdata 时终止用例。
func scopedContractBusinessLocation(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(DashboardTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", DashboardTimezone, err)
	}
	return location
}

// TestScopedContractUsesSourceStatusVocabularyAndExpiry 验证组织范围看板按源状态词表与到期派生计数。
// 参数：t 测试上下文。
// 返回值：无。
func TestScopedContractUsesSourceStatusVocabularyAndExpiry(t *testing.T) {
	db := scopedContractFixture(t)
	const tenant, org = "tenant-scope", "org-1"
	// 业务日与看板仓储同源时区：跨日窗口内用例不会因为 UTC 与 Asia/Shanghai 差一天而 flaky。
	location := scopedContractBusinessLocation(t)
	now := time.Now().In(location)
	today := now.Format("2006-01-02")
	future := now.AddDate(1, 0, 0).Format("2006-01-02")
	previousDay := now.AddDate(0, 0, -1).Format("2006-01-02")
	past := "2020-01-01"

	insertScopedContract(t, db, "NORMAL", tenant, "c-pending", "pending", "", "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-approved", "approved", future, "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-active-open", "active", "", "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-active-expired", "active", past, "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-in-progress", "in_progress", future, "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-pending-pay-today", "pending_pay", today, "u1", org)
	// 到期日等于业务日前一天的履行中合同必须计入已到期。
	insertScopedContract(t, db, "NORMAL", tenant, "c-active-yesterday", "active", previousDay, "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-archived", "archived", past, "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-completed", "completed", past, "u1", org)
	// 范围外组织、历史来源与个人范围行都不得混入组织口径。
	insertScopedContract(t, db, "NORMAL", tenant, "c-other-org", "active", "", "u2", "org-2")
	insertScopedContract(t, db, "IMPORTED", tenant, "c-imported", "active", "", "u1", org)
	insertScopedContract(t, db, "NORMAL", tenant, "c-self", "active", "", "u-self", "org-2")

	repository := NewGORMRepositoryWithLocation(db, location)
	ctx := context.Background()
	snapshot, available, err := repository.scopedContract(ctx, queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{org}})
	if err != nil || !available {
		t.Fatalf("scoped contract read failed: available=%v err=%v", available, err)
	}
	if snapshot.TotalContracts != 9 {
		t.Fatalf("total contracts=%d, want 9", snapshot.TotalContracts)
	}
	// 审批中：只有源状态 pending。
	if snapshot.ApprovalContracts != 1 {
		t.Fatalf("approval contracts=%d, want 1 (status=pending)", snapshot.ApprovalContracts)
	}
	// 履行中：active/in_progress/pending_pay 且未到期，另加终态 archived（与合同系统全租户看板一致）。
	if snapshot.ActiveContracts != 4 {
		t.Fatalf("active contracts=%d, want 4 (open active + in_progress + pending_pay today + archived)", snapshot.ActiveContracts)
	}
	// 已到期：履行中状态且 end_date 早于业务日，archived 永不计入。
	if snapshot.ExpiredContracts != 2 {
		t.Fatalf("expired contracts=%d, want 2 (past + business day minus one)", snapshot.ExpiredContracts)
	}
	if snapshot.TotalAmountMinor != 900 {
		t.Fatalf("total amount minor=%d, want 900", snapshot.TotalAmountMinor)
	}

	self, available, err := repository.scopedContract(ctx, queryScope{mode: "SELF", tenantID: tenant, userID: "u-self"})
	if err != nil || !available {
		t.Fatalf("self scope read failed: available=%v err=%v", available, err)
	}
	if self.TotalContracts != 1 || self.ActiveContracts != 1 || self.ApprovalContracts != 0 || self.ExpiredContracts != 0 {
		t.Fatalf("self scope totals=%+v, want only one unexpired active contract", self)
	}
}

// TestScopedContractIgnoresDerivedStatusWords 验证派生状态词不再产生虚假计数。
// 参数：t 测试上下文。
// 返回值：无。
func TestScopedContractIgnoresDerivedStatusWords(t *testing.T) {
	db := scopedContractFixture(t)
	const tenant, org = "tenant-derived", "org-1"
	for _, status := range []string{"APPROVING", "PENDING_APPROVAL", "ACTIVE", "EFFECTIVE", "SIGNED", "EXPIRED"} {
		insertScopedContract(t, db, "NORMAL", tenant, "c-"+status, status, "", "u1", org)
	}
	repository := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
	snapshot, available, err := repository.scopedContract(context.Background(), queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{org}})
	if err != nil || !available {
		t.Fatalf("scoped contract read failed: available=%v err=%v", available, err)
	}
	if snapshot.TotalContracts != 6 {
		t.Fatalf("total contracts=%d, want 6", snapshot.TotalContracts)
	}
	if snapshot.ApprovalContracts != 0 || snapshot.ActiveContracts != 0 || snapshot.ExpiredContracts != 0 {
		t.Fatalf("derived status words must not be counted: approval=%d active=%d expired=%d", snapshot.ApprovalContracts, snapshot.ActiveContracts, snapshot.ExpiredContracts)
	}
}
