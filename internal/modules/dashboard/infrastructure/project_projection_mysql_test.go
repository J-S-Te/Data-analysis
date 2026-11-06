package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Publish between the consumer's marker and dimension reads using another real
// connection. A non-snapshot reader would combine the old marker with new rows.
func TestProjectProjectionConsumerRealMySQLConsistentRead(t *testing.T) {
	file := os.Getenv("UIP_BI_PROJECTION_TEST_DSN_FILE")
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
		t.Fatal("isolated loopback _test schema required")
	}
	open := func() *gorm.DB {
		t.Helper()
		db, e := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
		if e != nil {
			t.Fatal("open isolated MySQL")
		}
		pool, _ := db.DB()
		t.Cleanup(func() { pool.Close() })
		return db
	}
	db, writer := open(), open()
	tenant := fmt.Sprintf("qa-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range []string{"dim_project", "project_projection_state"} {
			if e := writer.Table(table).Where("tenant_id=?", tenant).Delete(nil).Error; e != nil {
				t.Error(e)
			}
		}
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := aggregation.ProjectProjectionState{TenantID: tenant, ProjectionCode: aggregation.ProjectProjectionCode, RunID: "old-run", Generation: 1, Status: "SUCCESS", LastSuccessAt: &now, PublishedRunID: "old-run", PublishedSnapshotID: "old", PublishedSourceSnapshotAt: &now, PublishedChecksumSHA256: strings.Repeat("a", 64), PublishedTotal: 1}
	if err = db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	row := aggregation.ProjectProjection{TenantID: tenant, SnapshotID: "old", ID: "P-A", Name: "synthetic", OwnerOrgID: "qa-org", Status: "old-status", Risk: true, InFlight: false, ServiceItems: 2, CreatedAt: now, UpdatedAt: now, SourceUpdatedAt: now}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	var switched atomic.Bool
	var callbackErr error
	key := "qa:project-consumer-snapshot"
	if err = db.Callback().Query().After("gorm:query").Register(key, func(tx *gorm.DB) {
		if tx.Statement.Table != "project_projection_state" || !switched.CompareAndSwap(false, true) {
			return
		}
		// Session defaults do not describe per-transaction READ ONLY; assert the
		// server's actual write refusal instead. The publish interleaving below
		// independently proves repeatable snapshot behavior.
		probe := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE dim_project SET name=name WHERE tenant_id=?", tenant).Error
		var mysqlError *driver.MySQLError
		if !errors.As(probe, &mysqlError) || mysqlError.Number != 1792 {
			callbackErr = fmt.Errorf("consumer transaction did not enforce read-only")
			return
		}
		callbackErr = writer.Transaction(func(w *gorm.DB) error {
			if e := w.Where("tenant_id=?", tenant).Delete(&aggregation.ProjectProjection{}).Error; e != nil {
				return e
			}
			fresh := row
			fresh.SnapshotID = "new"
			fresh.Status = "new-status"
			fresh.Risk = false
			fresh.InFlight = true
			fresh.ServiceItems = 3
			other := fresh
			other.ID = "P-B"
			if e := w.Create(&[]aggregation.ProjectProjection{fresh, other}).Error; e != nil {
				return e
			}
			return w.Model(&aggregation.ProjectProjectionState{}).Where("tenant_id=? AND projection_code=?", tenant, aggregation.ProjectProjectionCode).Updates(map[string]any{"published_snapshot_id": "new", "published_run_id": "new-run", "published_total": 2}).Error
		})
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(key)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, UserID: "qa-user", ScopeMode: "ORG", OrganizationIDs: []string{"qa-org"}})
	repo := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
	old, available, err := repo.LatestProject(ctx, tenant)
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	if err != nil || !available || old.ProjectCount != 1 || old.RiskProjects != 1 || old.InFlightProjects != 0 || old.ServiceItems != 2 || old.StatusCounts["old-status"] != 1 {
		t.Fatalf("mixed concurrent publication: %+v / %v", old, err)
	}
	fresh, available, err := repo.LatestProject(ctx, tenant)
	if err != nil || !available || fresh.ProjectCount != 2 || fresh.RiskProjects != 0 || fresh.InFlightProjects != 2 || fresh.ServiceItems != 6 || fresh.StatusCounts["new-status"] != 2 {
		t.Fatalf("next snapshot did not observe publication: %+v / %v", fresh, err)
	}
	t.Log("real read-only RR consumer retained old marker+rows across concurrent publication; next read observed new marker+rows")
}

// TestTenantProjectReportStageCountsRealMySQL 在真实 MySQL JSON 列上固定 TENANT
// 读路径的两态：SQL NULL=未采集（nil），JSON=已采集（含空对象）。MySQL 会拒绝
// 非法 JSON，非法文本的解析失败断言在 sqlite 用例中覆盖。
func TestTenantProjectReportStageCountsRealMySQL(t *testing.T) {
	file := os.Getenv("UIP_BI_PROJECTION_TEST_DSN_FILE")
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
		t.Fatal("isolated loopback _test schema required")
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	pool, _ := db.DB()
	t.Cleanup(func() { pool.Close() })
	tenant := fmt.Sprintf("qa-report-stage-%d", time.Now().UnixNano())
	if err = db.Exec("DELETE FROM api_project_dashboard WHERE tenant_id=?", tenant).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DELETE FROM api_project_dashboard WHERE tenant_id=?", tenant).Error })
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "qa-boss", ScopeMode: "TENANT"})
	repo := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
	// 旧快照（NULL）先写入，最新快照带 JSON，TENANT 路径只读最新一行。
	if err = db.Exec(`INSERT INTO api_project_dashboard (tenant_id, snapshot_at, project_count, status_counts_json, report_stage_counts_json)
		VALUES (?, UTC_TIMESTAMP(3) - INTERVAL 1 HOUR, 1, '{}', NULL)`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	legacy, available, err := repo.LatestProject(ctx, tenant)
	if err != nil || !available || legacy.ReportStageCounts != nil {
		t.Fatalf("legacy snapshot = %+v available=%v err=%v, want nil report stage counts", legacy, available, err)
	}
	if err = db.Exec(`INSERT INTO api_project_dashboard (tenant_id, snapshot_at, project_count, status_counts_json, report_stage_counts_json)
		VALUES (?, UTC_TIMESTAMP(3), 2, '{"待实施":2}', '{"RECTIFYING":1,"":1}')`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	fresh, available, err := repo.LatestProject(ctx, tenant)
	if err != nil || !available || fresh.ReportStageCounts["RECTIFYING"] != 1 || fresh.ReportStageCounts[""] != 1 || len(fresh.ReportStageCounts) != 2 {
		t.Fatalf("fresh snapshot = %+v available=%v err=%v", fresh, available, err)
	}
	if err = db.Exec(`UPDATE api_project_dashboard SET report_stage_counts_json='{}' WHERE tenant_id=? AND snapshot_at > UTC_TIMESTAMP(3) - INTERVAL 1 MINUTE`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	collected, available, err := repo.LatestProject(ctx, tenant)
	if err != nil || !available || collected.ReportStageCounts == nil || len(collected.ReportStageCounts) != 0 {
		t.Fatalf("collected empty snapshot = %+v available=%v err=%v, want empty non-nil map", collected, available, err)
	}
}
