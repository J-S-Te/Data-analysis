package aggregation

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// importPrivateDSN 读取显式配置的本任务隔离MySQL凭据。
// 参数：t 为测试上下文，env 为私有DSN文件环境名。
// 返回值：受命名/地址/文件权限守卫的测试连接，不发现或回退业务库。
func importPrivateDSN(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skip("explicit isolated import-report DSN file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private DSN")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("dedicated loopback data_analysis_test_*_test schema required")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	return cfg.FormatDSN()
}

// TestImportReportRealMySQLPublicationAndSource 验证真实迁移、源事实、事务回滚和逐版本覆盖。
// 参数：t 为测试上下文，只能使用显式隔离的两个测试schema。
// 返回值：无，测试不连接现行业务库、不构造外部完成回执。
func TestImportReportRealMySQLPublicationAndSource(t *testing.T) {
	aggDSN := importPrivateDSN(t, "UIP_IMPORT_REPORT_TEST_DSN_FILE")
	sourceDSN := importPrivateDSN(t, "UIP_IMPORT_SOURCE_TEST_DSN_FILE")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := migration.Run(ctx, aggDSN, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(aggDSN), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated aggregation DB")
	}
	src, err := gorm.Open(mysql.Open(sourceDSN), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated source DB")
	}
	for _, conn := range []*gorm.DB{db, src} {
		pool, _ := conn.DB()
		defer pool.Close()
	}
	r := &Runner{aggDB: db, tenantID: "t", sources: map[string]string{"contract_management": sourceDSN}}
	for _, table := range []string{"contract_import_history", "contract_import_report_status", "contract_import_report_run"} {
		if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id = ?", r.tenantID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Exec(`CREATE TABLE IF NOT EXISTS con_contract (
 tenant_id VARCHAR(64),id VARCHAR(64),version BIGINT,origin_kind VARCHAR(32),origin_import_item_id VARCHAR(64),
 import_source_revision BIGINT,import_report_release_version BIGINT,import_report_required_channels VARCHAR(32),
 import_mode VARCHAR(32),import_policy_json JSON,status VARCHAR(32),start_date DATE,end_date DATE,
 owner_user_id VARCHAR(64),owner_org_id VARCHAR(64),crm_customer_id BIGINT NULL,opportunity_id VARCHAR(64) NULL,
 amount_minor BIGINT,currency CHAR(3),created_at DATETIME(6),PRIMARY KEY(tenant_id,id))`).Error; err != nil {
		t.Fatal(err)
	}
	if err := src.Exec("DELETE FROM con_contract").Error; err != nil {
		t.Fatal(err)
	}
	if err := src.Exec(`INSERT INTO con_contract VALUES ('t','c2',5,'IMPORTED','i2',2,3,'API_AND_SOURCE_FACTS','HISTORICAL_CLOSED','{"report_mode":"HISTORICAL_SEPARATE"}','completed','2020-01-31',NULL,'u','o',NULL,NULL,200,'USD','2020-01-01 00:00:00')`).Error; err != nil {
		t.Fatal(err)
	}
	s := reportFixture()
	date := "2020-01-31"
	s.Items[1].StartDate = &date
	facts, code := r.readImportSourceFacts(ctx, s)
	if code != "" || len(facts) != 1 || facts["i2"].StartDate == nil || *facts["i2"].StartDate != date {
		t.Fatalf("actual original facts %d code=%s", len(facts), code)
	}
	gen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, facts, code); err != nil {
		t.Fatal(err)
	}
	status, err := ReadImportReportStatus(ctx, db, "t", "i2", 2, 3)
	if err != nil || status.State != "APPLIED" || len(status.AppliedChannels) != 2 {
		t.Fatalf("actual publication %+v %v", status, err)
	}
	// 同轮源数据故障不得沿用上轮SOURCE_FACTS_APPLIED冒充新代次完成。
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, nil, "SOURCE_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}
	status, _ = ReadImportReportStatus(ctx, db, "t", "i2", 2, 3)
	if status.State != "BLOCKED" || status.SourceFactsApplied {
		t.Fatal("previous-round source evidence reused")
	}
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, facts, ""); err != nil {
		t.Fatal(err)
	}
	before := status.AggregationRunID
	if err = db.Exec("CREATE TRIGGER t10_import_abort BEFORE INSERT ON contract_import_report_status FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic publication abort'").Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER IF EXISTS t10_import_abort")
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, facts, ""); err == nil {
		t.Fatal("database abort ignored")
	}
	var count int64
	if err = db.Table("contract_import_history").Count(&count).Error; err != nil || count != 2 {
		t.Fatal("delete/insert publication was not rolled back")
	}
	status, _ = ReadImportReportStatus(ctx, db, "t", "i2", 2, 3)
	if status.State != "APPLIED" || status.AggregationRunID == before {
		t.Fatal("rollback lost last valid proof")
	}
	if err = db.Exec("DROP TRIGGER t10_import_abort").Error; err != nil {
		t.Fatal(err)
	}
	// 源事实版本不匹配是部分覆盖，不能因最大版本较新而完成。
	if err = src.Exec("UPDATE con_contract SET import_source_revision=3 WHERE tenant_id='t'").Error; err != nil {
		t.Fatal(err)
	}
	facts, code = r.readImportSourceFacts(ctx, s)
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, facts, code); err != nil {
		t.Fatal(err)
	}
	status, _ = ReadImportReportStatus(ctx, db, "t", "i2", 2, 3)
	if status.State != "WAITING_SOURCE" {
		t.Fatalf("version mismatch accepted %+v", status)
	}
	// 所有迁移已运行；幂等再次执行不重复应用。
	applied, err := migration.Run(ctx, aggDSN, migrations.Files)
	if err != nil || len(applied) != 0 {
		t.Fatalf("migration replay %d %v", len(applied), err)
	}
}

// TestImportReportMigrationInterruptedReplayPreservesCaches 模拟DDL后元数据登记中断的扩展迁移重入。
// 参数：t 为测试上下文，仅使用显式隔离schema。
// 返回值：无，重入不得删除普通API缓存或历史逐项证明。
func TestImportReportMigrationInterruptedReplayPreservesCaches(t *testing.T) {
	dsn := importPrivateDSN(t, "UIP_IMPORT_REPORT_TEST_DSN_FILE")
	ctx := context.Background()
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated migration DB")
	}
	pool, _ := db.DB()
	defer pool.Close()
	tenant := "migration-replay-normal"
	defer db.Exec("DELETE FROM api_contract_dashboard WHERE tenant_id=?", tenant)
	if err = db.Exec(`INSERT INTO api_contract_dashboard(tenant_id,snapshot_at,total_amount_minor,total_contracts,approval_contracts,active_contracts,expired_contracts,source) VALUES (?,UTC_TIMESTAMP(6),123,1,0,1,0,'synthetic')`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	// 仅在受守卫的本任务schema模拟最终登记丢失，不修改迁移checksum。
	if err = db.Exec("DELETE FROM da_schema_migration WHERE version=23").Error; err != nil {
		t.Fatal(err)
	}
	applied, err := migration.Run(ctx, dsn, migrations.Files)
	if err != nil || len(applied) != 1 {
		t.Fatalf("interrupted replay %d %v", len(applied), err)
	}
	var count int64
	if err = db.Table("api_contract_dashboard").Where("tenant_id=?", tenant).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("ordinary cache deleted by replay")
	}
}
