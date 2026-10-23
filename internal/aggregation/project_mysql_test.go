package aggregation

import (
	"bytes"
	"context"
	"errors"
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

// Explicit private loopback _test DSN only; never discover or reuse a business DB.
func TestProjectProjectionRealMySQLPublicationAndFencing(t *testing.T) {
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
		t.Fatal("isolated loopback _test DSN required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err = migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated DB")
	}
	pool, _ := db.DB()
	defer pool.Close()
	r := &Runner{aggDB: db, tenantID: "synthetic-tenant"}
	cleanup := func() {
		for _, table := range []string{"project_projection_stage", "project_projection_state", "dim_project", "sync_job"} {
			if e := db.Table(table).Where("tenant_id IN ?", []string{r.tenantID, "qa-foreign"}).Delete(nil).Error; e != nil {
				t.Error(e)
			}
		}
	}
	cleanup()
	defer cleanup()
	initial := importProject(t, r, projectFixture(t))
	state := projectState(t, r)
	if state.LastSuccessAt == nil || state.PublishedRunID != initial.RunID || projectCount(t, r, r.tenantID) != 2 {
		t.Fatal("real publication missing")
	}
	foreign := &Runner{aggDB: db, tenantID: "qa-foreign"}
	if e := foreign.aggDB.Exec("INSERT INTO dim_project (tenant_id,project_id,name,customer,contract,category,team,manager,status,progress,created_at,updated_at) VALUES ('qa-foreign','P-foreign','','','','','','','',0,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))").Error; e != nil {
		t.Fatal(e)
	}
	attempt, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	summary, e := readProjectSnapshot(ctx, bytes.NewReader(projectWire(t, projectMap(t))), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, attempt})
	if e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 2 || projectState(t, r).PublishedRunID != initial.RunID {
		t.Fatal("staging visible before publish")
	}
	if e = r.publishProjectProjection(ctx, attempt, summary); e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 1 {
		t.Fatal("replacement did not remove deleted project")
	}
	old, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	current, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.publishProjectProjection(ctx, old, summary); !errors.Is(e, ErrProjectProjectionSuperseded) {
		t.Fatalf("old publication accepted: %v", e)
	}
	if e = r.failProjectProjection(ctx, old, "INVALID_STREAM"); !errors.Is(e, ErrProjectProjectionSuperseded) {
		t.Fatalf("old failure accepted: %v", e)
	}
	before := projectState(t, r)
	gate := errors.New("fixture expired")
	r.LicenseCheck = func(context.Context) error { return gate }
	if e = r.publishProjectProjection(ctx, current, summary); !errors.Is(e, gate) {
		t.Fatal("license did not fence publication")
	}
	r.LicenseCheck = nil
	if got := projectState(t, r); got.PublishedRunID != before.PublishedRunID || !got.LastSuccessAt.Equal(*before.LastSuccessAt) {
		t.Fatal("denial altered published state")
	}
	if e = r.failProjectProjection(ctx, current, "INVALID_STREAM"); e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 1 {
		t.Fatal("failure removed old publication")
	}
	// An actual trigger aborts INSERT after the publication DELETE, proving rollback.
	trigger := "qa_project_projection_abort"
	if e = db.Exec("CREATE TRIGGER " + trigger + " BEFORE INSERT ON dim_project FOR EACH ROW BEGIN IF NEW.tenant_id='synthetic-tenant' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='qa rollback'; END IF; END").Error; e != nil {
		t.Fatal(e)
	}
	defer db.Exec("DROP TRIGGER IF EXISTS " + trigger)
	a, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s, e := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, a})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.publishProjectProjection(ctx, a, s); e == nil {
		t.Fatal("trigger did not reject publication")
	}
	if projectCount(t, r, r.tenantID) != 1 || projectState(t, r).PublishedRunID != before.PublishedRunID {
		t.Fatal("failed transaction lost publication")
	}
	if e = db.Exec("DROP TRIGGER " + trigger).Error; e != nil {
		t.Fatal(e)
	}
	// Expired queue owner is rejected by real row locking/DB time before staging.
	expired := time.Now().UTC().Add(-time.Minute)
	owner := "qa-owner"
	job := SyncJob{TenantID: r.tenantID, ID: "qa-projection-job", SourceID: "qa-source", SubsystemCode: "project", Status: "RUNNING", RequestedAt: time.Now().UTC(), LeaseOwner: &owner, LeaseGeneration: 1, LeaseExpiresAt: &expired}
	if e = db.Create(&job).Error; e != nil {
		t.Fatal(e)
	}
	leaseCtx := context.WithValue(ctx, queueLeaseKey{}, queueLease{tenant: r.tenantID, job: job.ID, owner: owner, generation: 1})
	if e = r.stageProjectProjection(leaseCtx, a, nil); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatalf("expired lease stage: %v", e)
	}
	if e = r.publishProjectProjection(leaseCtx, a, s); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatalf("expired lease publication: %v", e)
	}
	// Let a previously valid real DB lease expire while an actual write holds its row lock.
	if e = db.Model(&SyncJob{}).Where("id=?", job.ID).Update("lease_expires_at", gorm.Expr("UTC_TIMESTAMP(3) + INTERVAL 150000 MICROSECOND")).Error; e != nil {
		t.Fatal(e)
	}
	if e = r.aggregationWrite(leaseCtx, func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE dim_project SET name='must-rollback' WHERE tenant_id=?", r.tenantID).Error; err != nil {
			return err
		}
		return tx.Exec("SELECT SLEEP(0.25)").Error
	}); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatalf("lease expired before commit accepted: %v", e)
	}
	var changed int64
	if e = db.Model(&ProjectProjection{}).Where("tenant_id=? AND name='must-rollback'", r.tenantID).Count(&changed).Error; e != nil || changed != 0 {
		t.Fatal("expired lease committed rows")
	}
	// A gate closing between write and commit also rolls back the full publication.
	checks := 0
	r.LicenseCheck = func(context.Context) error {
		checks++
		if checks >= 2 {
			return gate
		}
		return nil
	}
	if e = r.publishProjectProjection(ctx, a, s); !errors.Is(e, gate) {
		t.Fatalf("commit gate not rechecked: %v", e)
	}
	r.LicenseCheck = nil
	if projectCount(t, r, r.tenantID) != 1 || projectState(t, r).PublishedRunID != before.PublishedRunID {
		t.Fatal("late license denial committed publication")
	}
	importProject(t, r, projectWire(t))
	if projectCount(t, r, r.tenantID) != 0 || projectCount(t, r, "qa-foreign") != 1 || projectState(t, r).PublishedTotal != 0 {
		t.Fatal("verified empty cleared foreign tenant or lacks success")
	}
	t.Log("real migrations, invisible staging, atomic replacement/rollback, generation/license/queue fencing and tenant-scoped verified empty passed")
}
