package migration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// Explicit private loopback _test DSN only; never discover or reuse a business DB.
// 本测试在同一个隔离实例里新建两个临时库（旧库=只跑到 000033，新库=全量），
// 分别验证“补列到既有数据”和“全新安装”，结束后只删除自己创建的库。
const reportStageMigrationTestDSNFile = "UIP_BI_MIGRATION_TEST_DSN_FILE"

var reportStageTestDatabaseName = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*_test$`)

func reportStageTestConfig(t *testing.T, database string) *driver.Config {
	t.Helper()
	file := os.Getenv(reportStageMigrationTestDSNFile)
	if file == "" {
		t.Skip("explicit isolated private DSN file required")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular DSN file required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("read private DSN")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("isolated loopback _test DSN required")
	}
	cfg.DBName = database
	return cfg
}

func reportStageTestOpen(t *testing.T, cfg *driver.Config) *sql.DB {
	t.Helper()
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open isolated MySQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// reportStageTestDatabase 建一个测试专用库；名字必须是本仓库测试固定前缀 + _test，
// 防止误连/误删业务库。
func reportStageTestDatabase(t *testing.T, server *driver.Config, name string) *driver.Config {
	t.Helper()
	if !reportStageTestDatabaseName.MatchString(name) {
		t.Fatalf("unsafe test database name %q", name)
	}
	serverCfg := *server
	serverCfg.DBName = ""
	serverCfg.MultiStatements = true
	serverDB, err := sql.Open("mysql", serverCfg.FormatDSN())
	if err != nil {
		t.Fatalf("open isolated server: %v", err)
	}
	defer serverDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"DROP DATABASE IF EXISTS `" + name + "`",
		"CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := serverDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare test database %s: %v", name, err)
		}
	}
	cfg := *server
	cfg.DBName = name
	t.Cleanup(func() {
		cleanup := *server
		cleanup.DBName = ""
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if db, err := sql.Open("mysql", cleanup.FormatDSN()); err == nil {
			_, _ = db.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS `"+name+"`")
			_ = db.Close()
		}
	})
	return &cfg
}

// reportStageOldSchema 只加载 000001..000033，模拟“迁移前已有数据的旧库”。
func reportStageOldSchema(t *testing.T) fstest.MapFS {
	t.Helper()
	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	old := fstest.MapFS{}
	for _, item := range items {
		if item.Version >= 34 {
			break
		}
		old[fmt.Sprintf("%06d_%s.sql", item.Version, item.Name)] = &fstest.MapFile{Data: []byte(item.SQL)}
	}
	return old
}

func reportStageAppliedVersions(applied []Applied) []uint64 {
	versions := make([]uint64, 0, len(applied))
	for _, item := range applied {
		versions = append(versions, item.Version)
	}
	return versions
}

func reportStageColumnFacts(t *testing.T, db *sql.DB, table, column string) (nullable, dataType string, maxLength int) {
	t.Helper()
	if err := db.QueryRow(`SELECT IS_NULLABLE, DATA_TYPE, COALESCE(CHARACTER_MAXIMUM_LENGTH, 0)
		FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, column).Scan(&nullable, &dataType, &maxLength); err != nil {
		t.Fatalf("read column %s.%s: %v", table, column, err)
	}
	return nullable, dataType, maxLength
}

func reportStageAssertSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"project_projection_stage", "dim_project"} {
		nullable, dataType, maxLength := reportStageColumnFacts(t, db, table, "report_stage")
		if nullable != "NO" || !strings.EqualFold(dataType, "varchar") || maxLength != 32 {
			t.Fatalf("%s.report_stage facts = nullable:%s type:%s len:%d, want NOT NULL VARCHAR(32)", table, nullable, dataType, maxLength)
		}
	}
	nullable, dataType, _ := reportStageColumnFacts(t, db, "api_project_dashboard", "report_stage_counts_json")
	if nullable != "YES" || !strings.EqualFold(dataType, "json") {
		t.Fatalf("api_project_dashboard.report_stage_counts_json facts = nullable:%s type:%s, want NULL JSON", nullable, dataType)
	}
	rows, err := db.Query(`SELECT SEQ_IN_INDEX, COLUMN_NAME FROM information_schema.statistics
		WHERE table_schema=DATABASE() AND table_name='dim_project' AND index_name='idx_dim_project_report_stage'
		ORDER BY SEQ_IN_INDEX`)
	if err != nil {
		t.Fatalf("read report stage index: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seq int
		var column string
		if err := rows.Scan(&seq, &column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(columns) != "[tenant_id report_stage]" {
		t.Fatalf("idx_dim_project_report_stage columns = %v", columns)
	}
}

// TestProjectReportStageMigrationOnExistingAndFreshDatabase 在真实 MySQL 上各跑一次：
// 旧库（已有数据）补列后历史行保持未采集，新库全量迁移后列可正常写入。
func TestProjectReportStageMigrationOnExistingAndFreshDatabase(t *testing.T) {
	cfg := reportStageTestConfig(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1) 旧库：先只应用到 000033，写入历史数据，再补 000034。
	oldCfg := reportStageTestDatabase(t, cfg, "da_project_report_stage_old_test")
	applied, err := Run(ctx, oldCfg.FormatDSN(), reportStageOldSchema(t))
	if err != nil {
		t.Fatalf("apply pre-000034 migrations: %v", err)
	}
	if len(applied) != 33 {
		t.Fatalf("old schema applied %v, want 33 migrations", reportStageAppliedVersions(applied))
	}
	oldDB := reportStageTestOpen(t, oldCfg)
	legacyStatements := []string{
		`INSERT INTO dim_project (tenant_id, project_id, name, customer, contract, status, created_at, updated_at)
		 VALUES ('legacy-tenant','P-legacy','历史项目','历史客户','HT-legacy','待实施',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO project_projection_stage
		 (tenant_id, run_id, project_id, snapshot_id, name, customer, customer_id, contract, contract_id,
		  category, team, manager, manager_identity_id, owner_org_id, owner_identity_id, status,
		  progress, risk, in_flight, service_items, created_at, updated_at, source_updated_at)
		 VALUES ('legacy-tenant','legacy-run','P-legacy','legacy-snapshot','历史项目','历史客户','c-1','HT-legacy','ht-1',
		  '类别','团队','经理','m-1','org-1','owner-1','待实施',
		  0,0,1,1,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO api_project_dashboard (tenant_id, snapshot_at, status_counts_json)
		 VALUES ('legacy-tenant', UTC_TIMESTAMP(3), '{"待实施":1}')`,
	}
	for _, statement := range legacyStatements {
		if _, err := oldDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed legacy rows: %v", err)
		}
	}
	applied, err = Run(ctx, oldCfg.FormatDSN(), migrations.Files)
	if err != nil {
		t.Fatalf("apply 000034 on existing database: %v", err)
	}
	if fmt.Sprint(reportStageAppliedVersions(applied)) != "[34]" {
		t.Fatalf("incremental applied versions = %v, want [34]", reportStageAppliedVersions(applied))
	}
	reportStageAssertSchema(t, oldDB)
	// 历史行保持“未采集”，不回填、不伪造。
	var dimStage, stagedStage string
	if err := oldDB.QueryRowContext(ctx, `SELECT report_stage FROM dim_project WHERE tenant_id='legacy-tenant'`).Scan(&dimStage); err != nil {
		t.Fatal(err)
	}
	if dimStage != "" {
		t.Fatalf("legacy dim_project.report_stage = %q, want empty", dimStage)
	}
	if err := oldDB.QueryRowContext(ctx, `SELECT report_stage FROM project_projection_stage WHERE tenant_id='legacy-tenant'`).Scan(&stagedStage); err != nil {
		t.Fatal(err)
	}
	if stagedStage != "" {
		t.Fatalf("legacy project_projection_stage.report_stage = %q, want empty", stagedStage)
	}
	var counts sql.NullString
	if err := oldDB.QueryRowContext(ctx, `SELECT report_stage_counts_json FROM api_project_dashboard WHERE tenant_id='legacy-tenant'`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	if counts.Valid {
		t.Fatalf("legacy report_stage_counts_json = %q, want SQL NULL (not collected)", counts.String)
	}
	// 迁移后新写入的行必须可带真实阶段，同时省略该列时仍取默认空串。
	if _, err := oldDB.ExecContext(ctx, `INSERT INTO dim_project (tenant_id, project_id, name, customer, contract, status, report_stage, created_at, updated_at)
		VALUES ('legacy-tenant','P-new','新项目','客户','HT-new','待实施','RECTIFYING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`); err != nil {
		t.Fatalf("write report_stage on migrated database: %v", err)
	}
	if _, err := oldDB.ExecContext(ctx, `INSERT INTO dim_project (tenant_id, project_id, name, customer, contract, status, created_at, updated_at)
		VALUES ('legacy-tenant','P-default','默认项目','客户','HT-default','待实施',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`); err != nil {
		t.Fatalf("write default report_stage: %v", err)
	}
	var newStage, defaultStage string
	if err := oldDB.QueryRowContext(ctx, `SELECT report_stage FROM dim_project WHERE tenant_id='legacy-tenant' AND project_id='P-default'`).Scan(&defaultStage); err != nil {
		t.Fatal(err)
	}
	if err := oldDB.QueryRowContext(ctx, `SELECT report_stage FROM dim_project WHERE tenant_id='legacy-tenant' AND project_id='P-new'`).Scan(&newStage); err != nil {
		t.Fatal(err)
	}
	if newStage != "RECTIFYING" || defaultStage != "" {
		t.Fatalf("migrated writes = %q / default %q", newStage, defaultStage)
	}
	// DDL 中断后重跑同一迁移（版本未登记场景）必须安全：守卫只在前置对象缺失时 ALTER。
	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	reportStageSQL := items[len(items)-1].SQL
	if _, err := oldDB.ExecContext(ctx, reportStageSQL); err != nil {
		t.Fatalf("rerun 000034 on migrated database: %v", err)
	}
	reportStageAssertSchema(t, oldDB)

	// 2) 新库：全量迁移一次，列直接可用。
	freshCfg := reportStageTestDatabase(t, cfg, "da_project_report_stage_new_test")
	applied, err = Run(ctx, freshCfg.FormatDSN(), migrations.Files)
	if err != nil {
		t.Fatalf("apply full migrations on fresh database: %v", err)
	}
	if len(applied) != 34 {
		t.Fatalf("fresh schema applied %v, want 34 migrations", reportStageAppliedVersions(applied))
	}
	freshDB := reportStageTestOpen(t, freshCfg)
	reportStageAssertSchema(t, freshDB)
	if _, err := freshDB.ExecContext(ctx, `INSERT INTO dim_project (tenant_id, project_id, name, customer, contract, status, report_stage, created_at, updated_at)
		VALUES ('fresh-tenant','P-fresh','新库项目','客户','HT-fresh','待实施','COMPILING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`); err != nil {
		t.Fatalf("write report_stage on fresh database: %v", err)
	}
	if _, err := freshDB.ExecContext(ctx, `INSERT INTO api_project_dashboard (tenant_id, snapshot_at, status_counts_json, report_stage_counts_json)
		VALUES ('fresh-tenant', UTC_TIMESTAMP(3), '{}', '{"RECTIFYING":2}')`); err != nil {
		t.Fatalf("write report_stage_counts_json on fresh database: %v", err)
	}
	var freshStage string
	if err := freshDB.QueryRowContext(ctx, `SELECT report_stage FROM dim_project WHERE tenant_id='fresh-tenant'`).Scan(&freshStage); err != nil {
		t.Fatal(err)
	}
	var freshCounts string
	if err := freshDB.QueryRowContext(ctx, `SELECT report_stage_counts_json FROM api_project_dashboard WHERE tenant_id='fresh-tenant'`).Scan(&freshCounts); err != nil {
		t.Fatal(err)
	}
	if freshStage != "COMPILING" || !strings.Contains(freshCounts, `"RECTIFYING": 2`) {
		t.Fatalf("fresh writes = %q / %q", freshStage, freshCounts)
	}
}
