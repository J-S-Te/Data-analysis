package aggregation

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestDimContractApprovalUpsertUsesSourceUpdatedAtGuard 验证审批投影 upsert 的形状：
// 业务列全部带来源时间守卫、updated_at 最后、占位符与插入列同序，且审批标志绝不写进
// dim_contract（合同维表的守卫版本是 con_contract.updated_at，审批状态变化不改合同行，
// 塞进 dim_contract 会被守卫直接拒绝更新）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDimContractApprovalUpsertUsesSourceUpdatedAtGuard(t *testing.T) {
	sql := dimContractApprovalUpsert

	if strings.Contains(sql, "in_approval") {
		t.Fatalf("审批中标志不得写进合同维表语句: %s", sql)
	}
	insertPart, updatePart, found := strings.Cut(sql, "ON DUPLICATE KEY UPDATE ")
	if !found {
		t.Fatalf("缺少冲突更新子句: %s", sql)
	}
	if !strings.HasPrefix(insertPart, "INSERT INTO dim_contract_approval\n") {
		t.Fatalf("审批投影必须写入独立维表: %s", insertPart)
	}
	if placeholders := strings.Count(insertPart, "?"); placeholders != 8 {
		t.Fatalf("占位符 %d，期望 8（tenant_id/contract_id/approval_id/kind/status/started_at/completed_at/updated_at）: %s", placeholders, insertPart)
	}
	// 插入列顺序与 execAggregation 的参数顺序必须一致，最后一项是守卫列。
	if !strings.Contains(insertPart, "(tenant_id, contract_id, approval_id, kind, status, started_at, completed_at, updated_at)") {
		t.Fatalf("插入列顺序不符合调用方参数顺序: %s", insertPart)
	}
	for _, column := range []string{"kind", "status", "started_at", "completed_at"} {
		guard := column + "=IF(VALUES(updated_at)>=updated_at,VALUES(" + column + ")," + column + ")"
		if !strings.Contains(updatePart, guard) {
			t.Fatalf("列 %s 缺少来源时间守卫: %s", column, updatePart)
		}
	}
	assignments := strings.Split(updatePart, ", ")
	last := assignments[len(assignments)-1]
	if last != "updated_at=IF(VALUES(updated_at)>=updated_at,VALUES(updated_at),updated_at)" {
		t.Fatalf("updated_at 必须最后并同样守卫，实际最后为 %q", last)
	}
	if len(assignments) != 5 {
		t.Fatalf("审批投影只允许更新 kind/status/started_at/completed_at + updated_at，实际 %d 项: %v", len(assignments), assignments)
	}
	// 守卫版本必须来自 con_approval_instance.updated_at：源查询列里必须有 updated_at，
	// 不得用 dim_contract 或合同行时间。
	if !strings.Contains(contractApprovalSourceColumns, "updated_at") {
		t.Fatalf("源列清单缺少来源守卫列 updated_at: %s", contractApprovalSourceColumns)
	}
	for _, forbidden := range []string{"nodes_json", "runtime_state_json", "applicant_user_id", "reason", "content_hash"} {
		if strings.Contains(contractApprovalSourceColumns, forbidden) {
			t.Fatalf("审批投影不得镜像 %s：%s", forbidden, contractApprovalSourceColumns)
		}
	}
}

// TestEnsureSyncSourcesRegistersApprovalChannel 验证审批通道只在合同只读 DSN 配置时登记，
// 且重复调用幂等（不产生重复行、不改写既有状态）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestEnsureSyncSourcesRegistersApprovalChannel(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncSource{}); err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-approval-sources"
	runner := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{"contract_management": "redacted"}}
	for i := 0; i < 2; i++ {
		if err := runner.EnsureSyncSources(context.Background()); err != nil {
			t.Fatalf("EnsureSyncSources() run %d error = %v", i, err)
		}
	}
	var rows []SyncSource
	if err := db.Where("tenant_id = ?", tenant).Order("subsystem_code").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("sync_source rows = %d, want 2 (contract_management + contract_approval)", len(rows))
	}
	statuses := map[string]string{}
	for _, row := range rows {
		statuses[row.SubsystemCode] = row.LastStatus
	}
	if statuses[ContractApprovalSource] != "PENDING" {
		t.Fatalf("approval channel status = %q, want PENDING: %v", statuses[ContractApprovalSource], statuses)
	}

	// 没有合同 DSN 时不登记审批通道：避免出现“从未有来源”的假通道。
	withoutContract := &Runner{aggDB: db, tenantID: "tenant-approval-nodsl", sources: map[string]string{"settlement": "redacted"}}
	if err := withoutContract.EnsureSyncSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&SyncSource{}).Where("tenant_id = ? AND subsystem_code = ?", "tenant-approval-nodsl", ContractApprovalSource).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("approval channel registered without contract DSN: count=%d", count)
	}
}

// TestSyncContractApprovalFromRealMySQL 在隔离测试库上验证审批投影的租户过滤、
// running/approved 并存与 updated_at 守卫（旧状态不得回退）。
// 参数：t 为测试上下文；未提供显式隔离 DSN 文件时跳过，不连接任何业务库。
// 返回值：无。
func TestSyncContractApprovalFromRealMySQL(t *testing.T) {
	file := os.Getenv("UIP_BI_DIM_SYNC_TEST_DSN_FILE")
	if file == "" {
		t.Skip("explicit isolated dim-sync DSN file required")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular DSN file required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("read private DSN")
	}
	cfg, err := mysqldriver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("dedicated loopback data_analysis_test_*_test schema required")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dsn := cfg.FormatDSN()
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated aggregation DB")
	}
	if pool, e := db.DB(); e == nil {
		defer pool.Close()
	}
	// 源表夹具：只建投影需要的列，模拟合同库 con_approval_instance（同库隔离 schema）。
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS con_approval_instance (
		tenant_id VARCHAR(26), id VARCHAR(26), contract_id VARCHAR(26), kind VARCHAR(32),
		status VARCHAR(32), created_at DATETIME(3), completed_at DATETIME(3) NULL,
		updated_at DATETIME(3), PRIMARY KEY (id))`).Error; err != nil {
		t.Fatal(err)
	}
	const tenant, otherTenant = "synthetic-approval-tenant1", "synthetic-approval-tenant2"
	cleanup := func() {
		if e := db.Exec("DELETE FROM con_approval_instance").Error; e != nil {
			t.Error(e)
		}
		if e := db.Exec("DELETE FROM dim_contract_approval").Error; e != nil {
			t.Error(e)
		}
	}
	cleanup()
	defer cleanup()
	base := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	if err := db.Exec(`INSERT INTO con_approval_instance
		(tenant_id,id,contract_id,kind,status,created_at,completed_at,updated_at) VALUES
		(?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?),(?,?,?,?,?,?,NULL,?)`,
		tenant, "appr-running1", "contract-1", "status_change", "running", base, nil, base,
		tenant, "appr-approved1", "contract-1", "contract_approval", "approved", base, base, base,
		otherTenant, "appr-other1", "contract-1", "contract_approval", "running", base, base).Error; err != nil {
		t.Fatal(err)
	}
	r := &Runner{aggDB: db, tenantID: tenant}
	src := db.WithContext(ctx).Where("tenant_id = ?", tenant)
	if err := r.syncContractApprovalFrom(ctx, src); err != nil {
		t.Fatal(err)
	}
	type approval struct {
		ApprovalID string `gorm:"column:approval_id"`
		Status     string
		Kind       string
	}
	var rows []approval
	if err := db.Table("dim_contract_approval").Where("tenant_id = ?", tenant).Order("approval_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("tenant approval rows = %d, want 2 (跨租户行不得写入): %+v", len(rows), rows)
	}
	statusByID := map[string]string{}
	kindByID := map[string]string{}
	for _, row := range rows {
		statusByID[row.ApprovalID] = row.Status
		kindByID[row.ApprovalID] = row.Kind
	}
	if statusByID["appr-running1"] != "running" || kindByID["appr-running1"] != "status_change" {
		t.Fatalf("变更审批投影错误: %+v", rows)
	}
	if statusByID["appr-approved1"] != "approved" {
		t.Fatalf("已通过审批投影错误: %+v", rows)
	}
	var otherCount int64
	if err := db.Table("dim_contract_approval").Where("tenant_id = ?", otherTenant).Count(&otherCount).Error; err != nil {
		t.Fatal(err)
	}
	if otherCount != 0 {
		t.Fatalf("跨租户审批行被写入: %d", otherCount)
	}
	// 审批结束后用更新的 updated_at 落库：running -> approved 必须生效。
	newer := base.Add(time.Hour)
	if err := db.Exec("UPDATE con_approval_instance SET status='approved', completed_at=?, updated_at=? WHERE id=?", newer, newer, "appr-running1").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.syncContractApprovalFrom(ctx, src); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Status    string
		UpdatedAt time.Time
	}
	if err := db.Table("dim_contract_approval").Select("status, updated_at").Where("tenant_id = ? AND approval_id = ?", tenant, "appr-running1").Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != "approved" || !after.UpdatedAt.Equal(newer) {
		t.Fatalf("更新后的审批状态未落库: %+v", after)
	}
	// 迟到的旧读取（更早 updated_at）不得把已完成的审批回退成 running。
	if err := db.Exec("UPDATE con_approval_instance SET status='running', completed_at=NULL, updated_at=? WHERE id=?", base, "appr-running1").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.syncContractApprovalFrom(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("dim_contract_approval").Select("status, updated_at").Where("tenant_id = ? AND approval_id = ?", tenant, "appr-running1").Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != "approved" || !after.UpdatedAt.Equal(newer) {
		t.Fatalf("旧的来源版本回退了审批状态: %+v", after)
	}
}
