package aggregation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const acceptanceTenant = "dashboard-acceptance-a"
const acceptanceOther = "dashboard-acceptance-b"

// dashboardAcceptanceDB 仅打开显式授权的私有回环MySQL测试库。
// 参数：t 为测试上下文，env 为私有DSN文件变量，migrate 决定是否应用真实DA迁移。
// 返回值：隔离数据库和连接串；缺配置跳过，危险配置或迁移失败则终止。
func dashboardAcceptanceDB(t *testing.T, env string, migrate bool) (*gorm.DB, string) {
	t.Helper()
	dsn := importPrivateDSN(t, env)
	if migrate {
		if _, err := migration.Run(context.Background(), dsn, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open explicitly isolated dashboard acceptance database")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("cannot access isolated connection pool")
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error("isolated connection pool cleanup failed")
		}
	})
	return db, dsn
}

// dashboardAcceptanceExec 执行仅影响本轮夹具的SQL并立即报告失败。
// 参数：t 为测试上下文，db 为隔离连接，query/args 为固定SQL与合成数据。
// 返回值：无；SQL失败终止当前用例。
func dashboardAcceptanceExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatal(err)
	}
}

// dashboardAcceptanceCleanup 清理本轮两个合成租户而不触碰其他租户。
// 参数：t 为测试上下文，db 为隔离连接，tables 为用例拥有的固定表名。
// 返回值：无；清理失败记为测试失败。
func dashboardAcceptanceCleanup(t *testing.T, db *gorm.DB, tables []string) {
	t.Helper()
	for _, table := range tables {
		if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id IN (?,?)",
			acceptanceTenant, acceptanceOther).Error; err != nil {
			t.Error(err)
		}
	}
}

// dashboardAcceptanceRead 读取真实持久化payload，检查可用标记与租户。
// 参数：t/db 为测试与隔离连接，tenant/section 为精确存储键。
// 返回值：已发布快照；缺行、损坏或租户不匹配时终止测试。
func dashboardAcceptanceRead(t *testing.T, db *gorm.DB, tenant, section string) domain.BusinessSnapshot {
	t.Helper()
	var row businessRow
	if err := db.Where("tenant_id=? AND section=?", tenant, section).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	var value domain.BusinessSnapshot
	if err := json.Unmarshal([]byte(row.Payload), &value); err != nil {
		t.Fatal(err)
	}
	if !value.Available || value.TenantID != tenant || value.SnapshotAt == "" {
		t.Fatalf("invalid persisted snapshot tenant=%s section=%s", tenant, section)
	}
	return value
}

// TestDashboardAcceptanceHTTPMySQLQueue 验证独立HTTP契约经真实聚合与手工队列发布。
// 参数：t 为验收上下文，仅接受本轮隔离MySQL；HTTP是合成消费者契约，非真实上游。
// 返回值：无；源错误覆盖旧值、跨租户、伪零、队列重复发布或状态丢失时失败。
func TestDashboardAcceptanceHTTPMySQLQueue(t *testing.T) {
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_TEST_DSN_FILE", true)
	tables := []string{"business_dashboard_snapshot", "sync_job", "sync_source"}
	dashboardAcceptanceCleanup(t, db, tables)
	t.Cleanup(func() { dashboardAcceptanceCleanup(t, db, tables) })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stamp := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	report := fmt.Sprintf(`{"available":true,"tenant_id":%q,"snapshot_at":%q,`+
		`"metrics":{"active_reports":3,"void_reports":2,"status_counts":{"ISSUED":2,"DRAFT":1},`+
		`"cycle_p50_days":2.5,"cycle_samples":2,"reviewed_reports":2},`+
		`"missing_metrics":["first_pass_rate","open_complaints"],"basis":"independent ledger fixture"}`,
		acceptanceTenant, stamp)
	finance := fmt.Sprintf(`{"available":true,"tenant_id":%q,"snapshot_at":%q,`+
		`"metrics":{"as_of_date":"2026-11-11","month":"2026-11","currencies":[`+
		`{"currency":"CNY","original_minor":10000,"allocated_minor":2500,"open_minor":6500,`+
		`"over_90_minor":1000,"month_receipts_minor":2500,"collection_rate":25},`+
		`{"currency":"USD","original_minor":0,"allocated_minor":0,"open_minor":0,`+
		`"over_90_minor":0,"month_receipts_minor":900,"collection_rate":null}]},`+
		`"missing_metrics":["confirmed_revenue"],"basis":"independent finance fixture"}`,
		acceptanceTenant, stamp)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-DA-Tenant-ID") != acceptanceTenant ||
			req.Header.Get("Authorization") != "Bearer synthetic-dashboard-token" {
			t.Error("missing tenant-bound machine credential")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if req.URL.Path == "/reports" {
			_, _ = fmt.Fprint(w, report)
		} else {
			_, _ = fmt.Fprint(w, finance)
		}
	}))
	defer server.Close()
	api := NewAPISyncRunner(db, APISyncOptions{TenantID: acceptanceTenant,
		ProjectReportURL: server.URL + "/reports", SettlementFinanceURL: server.URL + "/finance",
		ProjectCredential:    MachineCredential{ClientID: "qa-project", Scope: "dashboard.project.read"},
		SettlementCredential: MachineCredential{ClientID: "qa-settlement", Scope: "dashboard.settlement.read"}})
	for _, key := range []string{"qa-project", "qa-settlement"} {
		api.tokens[key] = cachedMachineToken{value: "synthetic-dashboard-token", expiresAt: time.Now().Add(time.Hour)}
	}
	r := &Runner{aggDB: db, tenantID: acceptanceTenant, sources: map[string]string{}, ProjectAPI: api}
	if err := r.EnsureSyncSources(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSyncSources(ctx); err != nil {
		t.Fatal(err)
	}
	var sources []SyncSource
	if err := db.Where("tenant_id=?", acceptanceTenant).Order("subsystem_code").Find(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].SubsystemCode != "project_management" || sources[1].SubsystemCode != "settlement" {
		t.Fatalf("configured source registry=%v", sources)
	}
	for _, source := range sources {
		active := "ACTIVE"
		job := SyncJob{ID: ulid(), TenantID: acceptanceTenant, SourceID: source.ID,
			SubsystemCode: source.SubsystemCode, Status: "QUEUED", ActiveKey: &active,
			RequestedAt: time.Now().UTC()}
		if err := db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	var completed int64
	if err := db.Model(&SyncJob{}).Where("tenant_id=? AND status='SUCCESS' AND active_key IS NULL", acceptanceTenant).Count(&completed).Error; err != nil || completed != 2 {
		t.Fatalf("manual jobs completed=%d err=%v", completed, err)
	}
	actual := dashboardAcceptanceRead(t, db, acceptanceTenant, "report")
	if actual.Metrics["active_reports"] != float64(3) || actual.Metrics["void_reports"] != float64(2) ||
		actual.Metrics["cycle_p50_days"] != 2.5 || actual.SourceSnapshotAt != stamp {
		t.Fatalf("report differs from independent fixture: %+v", actual)
	}
	actualFinance := dashboardAcceptanceRead(t, db, acceptanceTenant, "finance")
	currencies := actualFinance.Metrics["currencies"].([]any)
	if len(currencies) != 2 || currencies[0].(map[string]any)["open_minor"] != float64(6500) ||
		currencies[1].(map[string]any)["collection_rate"] != nil {
		t.Fatalf("finance lost net balance/currency/null denominator: %+v", actualFinance)
	}
	dashboardAcceptanceFailedQueue(t, r, db, &status)
	dashboardAcceptanceRejectsSourceErrors(t, r, db, &report, &status, stamp)
}

// dashboardAcceptanceFailedQueue 验证来源失败的队列终态与最后成功凭据。
// 参数：t/r/db 为测试、执行器及隔离库，status 为合成服务器响应码。
// 返回值：无；失败误报成功、旧财务快照或成功水位丢失时失败。
func dashboardAcceptanceFailedQueue(t *testing.T, r *Runner, db *gorm.DB, status *int) {
	t.Helper()
	var before SyncSource
	if err := db.Where("tenant_id=? AND subsystem_code='settlement'", acceptanceTenant).Take(&before).Error; err != nil || before.LastSuccessAt == nil || before.LastStatus != "SUCCESS" {
		t.Fatalf("successful source missing: %+v err=%v", before, err)
	}
	var snapshot businessRow
	if err := db.Where("tenant_id=? AND section='finance'", acceptanceTenant).Take(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	active := "ACTIVE"
	job := SyncJob{ID: ulid(), TenantID: acceptanceTenant, SourceID: before.ID,
		SubsystemCode: "settlement", Status: "QUEUED", ActiveKey: &active, RequestedAt: time.Now().UTC()}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	*status = http.StatusServiceUnavailable
	defer func() { *status = http.StatusOK }()
	if err := r.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id=?", job.ID).Take(&job).Error; err != nil || job.Status != "FAILED" || job.ActiveKey != nil {
		t.Fatalf("failed queue status=%+v err=%v", job, err)
	}
	var after SyncSource
	var retained businessRow
	if err := db.Where("id=?", before.ID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id=? AND section='finance'", acceptanceTenant).Take(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if after.LastStatus != "FAILED" || after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*before.LastSuccessAt) || retained.Payload != snapshot.Payload {
		t.Fatal("failed source replaced financial snapshot or last success watermark")
	}
}

// dashboardAcceptanceRejectsSourceErrors 验证拒绝错误来源时真实快照逐字保留。
// 参数：t/r/db 为上下文和聚合器，report/status 为合成服务器响应，stamp 为独立来源水位。
// 返回值：无；拒绝失败、旧快照改变或未配置与真零混淆时失败。
func dashboardAcceptanceRejectsSourceErrors(
	t *testing.T, r *Runner, db *gorm.DB, report *string, status *int, stamp string,
) {
	t.Helper()
	ctx := context.Background()
	var baseline businessRow
	if err := db.Where("tenant_id=? AND section='report'", acceptanceTenant).Take(&baseline).Error; err != nil {
		t.Fatal(err)
	}
	valid := *report
	for _, mode := range []string{"unavailable", "other-tenant", "malformed", "missing-field", "old-watermark"} {
		*report, *status = valid, http.StatusOK
		switch mode {
		case "unavailable":
			*status = http.StatusServiceUnavailable
		case "other-tenant":
			*report = strings.Replace(valid, acceptanceTenant, acceptanceOther, 1)
		case "malformed":
			*report = `{"available":`
		case "missing-field":
			*report = strings.Replace(valid, `"active_reports":3,`, "", 1)
		case "old-watermark":
			*report = strings.Replace(valid, stamp, "2020-01-01T00:00:00Z", 1)
		}
		if err := r.runSource(ctx, "project_management", ""); err == nil {
			t.Fatalf("accepted invalid source: %s", mode)
		}
		var retained businessRow
		if err := db.Where("tenant_id=? AND section='report'", acceptanceTenant).Take(&retained).Error; err != nil || retained.Payload != baseline.Payload {
			t.Fatalf("failure replaced previous snapshot: %s err=%v", mode, err)
		}
	}
	*status = http.StatusOK
	*report = fmt.Sprintf(`{"available":true,"tenant_id":%q,"snapshot_at":%q,`+
		`"metrics":{"active_reports":0,"void_reports":0,"status_counts":{},`+
		`"cycle_p50_days":null,"cycle_samples":0,"reviewed_reports":0},`+
		`"missing_metrics":["first_pass_rate","open_complaints"],"basis":"successful empty ledger"}`,
		acceptanceTenant, time.Now().UTC().Format(time.RFC3339Nano))
	if err := r.runSource(ctx, "project_management", ""); err != nil {
		t.Fatal(err)
	}
	zero := dashboardAcceptanceRead(t, db, acceptanceTenant, "report")
	if zero.Metrics["active_reports"] != float64(0) || zero.Metrics["cycle_p50_days"] != nil {
		t.Fatalf("successful zero converted into unavailable=%+v", zero)
	}
	var leaked int64
	if err := db.Model(&businessRow{}).Where("tenant_id=?", acceptanceOther).Count(&leaked).Error; err != nil || leaked != 0 {
		t.Fatalf("cross-tenant publication=%d err=%v", leaked, err)
	}
	unconfigured := &Runner{aggDB: db, tenantID: acceptanceOther, sources: map[string]string{}}
	if err := unconfigured.runSource(ctx, "project_management", ""); err == nil {
		t.Fatal("unconfigured source treated as successful zero")
	}
}

// TestDashboardAcceptanceSourceReconciliationMySQL 验证真实来源扫描成功删除及失败保留。
// 参数：t 为本轮隔离MySQL上下文；源表为显式合成契约，不代表真实业务服务。
// 返回值：无；合同/CRM/审批硬删未对账、失败误删或跨租户删除时失败。
func TestDashboardAcceptanceSourceReconciliationMySQL(t *testing.T) {
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_TEST_DSN_FILE", true)
	src, dsn := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_SOURCE_DSN_FILE", false)
	dashboardAcceptanceSourceTables(t, src)
	targets := []string{"dim_contract", "dim_customer", "dim_opportunity", "dim_contract_approval", "dimension_sync_key"}
	sourceTables := []string{"con_contract", "crm_customers", "crm_opportunities", "con_approval_instance"}
	dashboardAcceptanceCleanup(t, db, targets)
	dashboardAcceptanceCleanup(t, src, sourceTables)
	t.Cleanup(func() {
		dashboardAcceptanceCleanup(t, db, targets)
		dashboardAcceptanceCleanup(t, src, sourceTables)
	})
	dashboardAcceptanceSeedSources(t, src)
	ctx := context.Background()
	for _, tenant := range []string{acceptanceTenant, acceptanceOther} {
		r := &Runner{aggDB: db, tenantID: tenant}
		for _, sync := range []func(context.Context, string) error{r.syncContract, r.syncCRM, r.syncContractApprovalFromSource} {
			if err := sync(ctx, dsn); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, table := range sourceTables {
		dashboardAcceptanceExec(t, src, "DELETE FROM "+table+" WHERE tenant_id=?", acceptanceTenant)
	}
	r := &Runner{aggDB: db, tenantID: acceptanceTenant}
	for _, sync := range []func(context.Context, string) error{r.syncContract, r.syncCRM, r.syncContractApprovalFromSource} {
		r.LicenseCheck = func(context.Context) error { return errors.New("synthetic license denial") }
		if err := sync(ctx, dsn); err == nil {
			t.Fatal("source failure ignored")
		}
		r.LicenseCheck = nil
	}
	dashboardAcceptanceBrokenSourceReads(t, r, src, dsn)
	for _, table := range targets[:4] {
		var count int64
		if err := db.Table(table).Where("tenant_id=?", acceptanceTenant).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("failed source erased %s count=%d err=%v", table, count, err)
		}
	}
	for _, sync := range []func(context.Context, string) error{r.syncContract, r.syncCRM, r.syncContractApprovalFromSource} {
		if err := sync(ctx, dsn); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range targets[:4] {
		var own, other int64
		if err := db.Table(table).Where("tenant_id=?", acceptanceTenant).Count(&own).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table(table).Where("tenant_id=?", acceptanceOther).Count(&other).Error; err != nil || own != 0 || other != 1 {
			t.Fatalf("reconciliation %s own=%d other=%d err=%v", table, own, other, err)
		}
	}
}

// dashboardAcceptanceBrokenSourceReads 用隔离源表暂时缺失验证真实SQL错误关闭删除。
// 参数：t/r 为测试与执行器，src/dsn 为本轮独立合成源库。
// 返回值：无；源错误被吞没或表名恢复失败时失败。
func dashboardAcceptanceBrokenSourceReads(t *testing.T, r *Runner, src *gorm.DB, dsn string) {
	t.Helper()
	for _, item := range []struct {
		table string
		run   func(context.Context, string) error
	}{{"con_contract", r.syncContract}, {"crm_opportunities", r.syncCRM},
		{"con_approval_instance", r.syncContractApprovalFromSource}} {
		func() {
			missing := item.table + "_qa_unavailable"
			dashboardAcceptanceExec(t, src, "RENAME TABLE "+item.table+" TO "+missing)
			defer func() { dashboardAcceptanceExec(t, src, "RENAME TABLE "+missing+" TO "+item.table) }()
			if err := item.run(context.Background(), dsn); err == nil {
				t.Fatalf("source SQL failure ignored: %s", item.table)
			}
		}()
	}
}

// dashboardAcceptanceSourceTables 建立只用于本轮回环测试的源契约表。
// 参数：t 为测试上下文，db 为单独的合成来源schema。
// 返回值：无；建表失败终止测试。
func dashboardAcceptanceSourceTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	queries := []string{
		`CREATE TABLE IF NOT EXISTS con_contract (tenant_id VARCHAR(26),id VARCHAR(26),
		origin_kind VARCHAR(32),contract_number VARCHAR(64),title VARCHAR(255),contract_type VARCHAR(64),
		service_type VARCHAR(64),status VARCHAR(32),start_date DATE,end_date DATE,crm_customer_id BIGINT,
		opportunity_id VARCHAR(64),amount_minor BIGINT,currency CHAR(3),owner_user_id VARCHAR(26),
		owner_org_id VARCHAR(64),owner_identity_id VARCHAR(128),project_id VARCHAR(64),updated_at DATETIME(3),
		PRIMARY KEY(tenant_id,id)) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS crm_customers (tenant_id VARCHAR(26),id BIGINT,
		customer_no VARCHAR(32),name VARCHAR(200),customer_type VARCHAR(64),industry VARCHAR(64),
		region VARCHAR(64),status VARCHAR(32),owner_user_id VARCHAR(64),owner_org_id VARCHAR(64),
		deleted_at DATETIME(3),updated_at DATETIME(3),PRIMARY KEY(tenant_id,id)) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS crm_opportunities (tenant_id VARCHAR(26),id BIGINT,
		opportunity_no VARCHAR(32),name VARCHAR(200),customer_id BIGINT,type VARCHAR(64),source VARCHAR(64),
		expected_amount DECIMAL(18,2),expected_sign_date DATE,owner_user_id VARCHAR(64),owner_org_id VARCHAR(64),
		current_stage VARCHAR(32),opp_status VARCHAR(32),contract_ref VARCHAR(64),contract_id VARCHAR(64),
		created_at DATETIME(3),lost_reason VARCHAR(64),stage_changed_at DATETIME(3),deleted_at DATETIME(3),
		updated_at DATETIME(3),PRIMARY KEY(tenant_id,id)) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS con_approval_instance (tenant_id VARCHAR(26),id VARCHAR(26),
		contract_id VARCHAR(26),kind VARCHAR(32),status VARCHAR(32),created_at DATETIME(3),
		completed_at DATETIME(3),updated_at DATETIME(3),PRIMARY KEY(tenant_id,id)) ENGINE=InnoDB`,
	}
	for _, query := range queries {
		dashboardAcceptanceExec(t, db, query)
	}
}

// dashboardAcceptanceSeedSources 写入独立预期的两租户最小来源样本。
// 参数：t 为测试上下文，db 为本轮独立源schema。
// 返回值：无；插入失败终止测试。
func dashboardAcceptanceSeedSources(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, tenant := range []string{acceptanceTenant, acceptanceOther} {
		dashboardAcceptanceExec(t, db, `INSERT INTO con_contract VALUES
		(?, 'contract-a','NORMAL','C001','合成合同','service','consulting','active',
		'2026-01-01',NULL,1,'1',12000,'CNY','owner','org',NULL,NULL,UTC_TIMESTAMP(3))`, tenant)
		dashboardAcceptanceExec(t, db, `INSERT INTO crm_customers VALUES
		(?,1,'CUS001','合成客户','enterprise','IT','CN','active','owner','org',NULL,UTC_TIMESTAMP(3))`, tenant)
		dashboardAcceptanceExec(t, db, `INSERT INTO crm_opportunities VALUES
		(?,1,'OPP001','合成商机',1,'service','direct',120.00,'2026-01-01','owner','org',
		'won','won',NULL,'contract-a','2026-01-01',NULL,UTC_TIMESTAMP(3),NULL,UTC_TIMESTAMP(3))`, tenant)
		dashboardAcceptanceExec(t, db, `INSERT INTO con_approval_instance VALUES
		(?,'approval-a','contract-a','contract_approval','running',UTC_TIMESTAMP(3),NULL,UTC_TIMESTAMP(3))`, tenant)
	}
}

// TestDashboardAcceptanceRetentionMySQL 验证真实MySQL清理不丢24个月API趋势检查点。
// 参数：t 为显式隔离MySQL上下文，使用固定北京时间业务月预期。
// 返回值：无；月末检查点丢失、重复样本未压缩或其他租户被清理时失败。
func TestDashboardAcceptanceRetentionMySQL(t *testing.T) {
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_ACCEPTANCE_TEST_DSN_FILE", true)
	tables := []string{"api_project_dashboard", "api_contract_dashboard"}
	dashboardAcceptanceCleanup(t, db, tables)
	t.Cleanup(func() { dashboardAcceptanceCleanup(t, db, tables) })
	location := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, location)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, location)
	for _, table := range tables {
		for offset := 0; offset < 25; offset++ {
			for day := 0; day < 2; day++ {
				at := month.AddDate(0, -offset, day).UTC()
				if table == "api_project_dashboard" {
					dashboardAcceptanceExec(t, db, `INSERT INTO api_project_dashboard
					(tenant_id,snapshot_at,project_count,status_counts_json) VALUES (?,?,?,JSON_OBJECT())`,
						acceptanceTenant, at, 100+offset)
				} else {
					dashboardAcceptanceExec(t, db, `INSERT INTO api_contract_dashboard
					(tenant_id,snapshot_at,total_contracts) VALUES (?,?,?)`, acceptanceTenant, at, 100+offset)
				}
			}
		}
		row := map[string]any{"tenant_id": acceptanceOther,
			"snapshot_at": month.AddDate(-5, 0, 0).UTC()}
		if table == "api_project_dashboard" {
			row["status_counts_json"] = "{}"
		}
		if err := db.Table(table).Create(row).Error; err != nil {
			t.Fatal(err)
		}
		if err := PruneSnapshotHistory(context.Background(), db, acceptanceTenant, table,
			now, location, SnapshotRetentionPolicy{}, nil); err != nil {
			t.Fatal(err)
		}
		var times []time.Time
		if err := db.Table(table).Where("tenant_id=?", acceptanceTenant).
			Order("snapshot_at").Pluck("snapshot_at", &times).Error; err != nil {
			t.Fatal(err)
		}
		if len(times) != 24 {
			t.Fatalf("%s monthly checkpoints=%d want=24", table, len(times))
		}
		for i, at := range times {
			want := month.AddDate(0, -(23 - i), 1)
			if !at.Equal(want.UTC()) {
				t.Fatalf("%s month[%d]=%s want=%s", table, i, at, want)
			}
		}
		var other int64
		if err := db.Table(table).Where("tenant_id=?", acceptanceOther).Count(&other).Error; err != nil || other != 1 {
			t.Fatalf("cross-tenant retention changed %s count=%d err=%v", table, other, err)
		}
	}
}
