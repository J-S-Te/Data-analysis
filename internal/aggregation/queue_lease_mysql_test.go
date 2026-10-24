//go:build qa_p1_bi_db

package aggregation

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// 显式标签及必需的私有 DSN 文件使此夹具排除在普通
// 测试之外。调用方必须提供本轮新建且归本轮所有的数据库。
func TestMySQLSyncQueueUpgradeOwnershipAndFencedWrites(t *testing.T) {
	path := os.Getenv("UIP_BI_TEST_MYSQL_DSN_FILE")
	if path == "" {
		t.Fatal("dedicated private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read dedicated DSN file")
	}
	dsn := strings.TrimSpace(string(raw))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open dedicated MySQL fixture")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("fixture pool cleanup failed: %v", err)
		}
	})
	var count int64
	if err = db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("fixture refuses a nonempty database; supply newly created schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := fstest.MapFS{}
	items, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasSuffix(item.Name(), ".sql") && !strings.HasPrefix(item.Name(), "000015_") {
			b, e := fs.ReadFile(migrations.Files, item.Name())
			if e != nil {
				t.Fatal(e)
			}
			before[item.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, err = migration.Run(ctx, dsn, before); err != nil {
		t.Fatal(err)
	}
	const tenant = "00000000000000000000000001"
	const legacyID = "00000000000000000000000002"
	if err = db.Exec("INSERT INTO sync_job (id,tenant_id,source_id,subsystem_code,status,requested_at,active_key) VALUES (?,?,?,'settlement','RUNNING',UTC_TIMESTAMP(3),'ACTIVE')", legacyID, tenant, "00000000000000000000000003").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var legacy SyncJob
	if err = db.First(&legacy, "id=?", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.Status != "RUNNING" || legacy.LeaseOwner != nil || legacy.LeaseGeneration != 0 || legacy.LeaseExpiresAt != nil {
		t.Fatal("migration guessed legacy task liveness or lost data")
	}
	r := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{}}
	if err = r.RecoverLegacySyncJobs(ctx, LegacyRecoveryInput{JobIDs: []string{legacyID}, Operator: "qa-isolated", VerifyStopped: func(context.Context) ([]byte, error) {
		return []byte(`{"fixture":"new-isolated-schema","no_legacy_worker":true}`), nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err = db.First(&legacy, "id=?", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	var owners sync.WaitGroup
	won := make(chan queueLease, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		owners.Add(1)
		go func() {
			defer owners.Done()
			l, claimed, e := r.claimQueueJob(ctx, legacy, time.Second)
			if e != nil {
				errs <- e
			}
			if claimed {
				won <- l
			}
		}()
	}
	owners.Wait()
	close(won)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if len(won) != 1 {
		t.Fatalf("MySQL CAS owners=%d", len(won))
	}
	old := <-won
	// 允许过期租约被接管，不依赖宿主机时钟休眠。
	if err = db.Model(&SyncJob{}).Where("id=?", legacyID).Update("lease_expires_at", gorm.Expr("DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 SECOND)")).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.First(&legacy, "id=?", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	current, ok, err := r.claimQueueJob(ctx, legacy, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	oldCtx := context.WithValue(ctx, queueLeaseKey{}, old)
	if err = r.storeBusiness(oldCtx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"stale": true}}); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("MySQL stale write: %v", err)
	}
	if err = r.finishQueueJob(ctx, old, nil, false); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("MySQL stale finalize: %v", err)
	}
	currentCtx := context.WithValue(ctx, queueLeaseKey{}, current)
	if err = r.storeBusiness(currentCtx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"current": true}}); err != nil {
		t.Fatal(err)
	}
	if err = r.finishQueueJob(ctx, current, nil, false); err != nil {
		t.Fatal(err)
	}
	if err = db.First(&legacy, "id=?", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.Status != "SUCCESS" || legacy.ActiveKey != nil || legacy.LeaseGeneration != 2 {
		t.Fatal("MySQL final state/active slot mismatch")
	}
	var audits int64
	db.Model(&syncRecoveryAudit{}).Count(&audits)
	if audits != 1 {
		t.Fatal("missing controlled legacy recovery audit")
	}
}

func TestMySQLSyncQueueNormalizesNonUTCDSN(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("UIP_BI_TEST_MYSQL_DSN_FILE"))
	if err != nil {
		t.Fatal("dedicated private DSN file required")
	}
	config, err := mysqldriver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	config.ParseTime = false
	config.Loc, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	const tenant = "00000000000000000000000001"
	r, err := NewRunner(config.FormatDSN(), tenant, nil)
	if err != nil {
		t.Fatal("non-UTC configured fixture runner failed")
	}
	pool, err := r.aggDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var schema string
	if err = r.aggDB.Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil || schema != "dashboard_p1_test_20261013" {
		t.Fatal("requires root-created exclusive schema")
	}
	identity, err := queueOwner()
	if err != nil {
		t.Fatal(err)
	}
	id := identity[:26]
	active := "ACTIVE"
	job := SyncJob{ID: id, TenantID: tenant, SourceID: id, SubsystemCode: "settlement", Status: "QUEUED", ActiveKey: &active, RequestedAt: time.Now().UTC()}
	if err = r.aggDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.aggDB.Where("tenant_id=? AND id=?", tenant, id).Delete(&SyncJob{}).Error; err != nil {
			t.Error(err)
		}
	})
	l, ok, err := r.claimQueueJob(context.Background(), job, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = r.aggDB.First(&job, "id=?", id).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now) || job.LeaseExpiresAt.After(now.Add(2*time.Minute)) {
		t.Fatal("lease did not preserve absolute database UTC time")
	}
	if err = r.finishQueueJob(context.Background(), l, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSyncQueueRowLocksUseFreshDatabaseClock(t *testing.T) {
	path := os.Getenv("UIP_BI_TEST_MYSQL_DSN_FILE")
	if path == "" {
		t.Fatal("dedicated private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read dedicated DSN file")
	}
	db, err := gorm.Open(mysql.Open(strings.TrimSpace(string(raw))), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open dedicated MySQL fixture")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("fixture pool cleanup failed: %v", err)
		}
	})
	var schema string
	if err = db.Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil || schema != "dashboard_p1_test_20261013" {
		t.Fatal("fixture requires its root-created dedicated schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity, err := queueOwner()
	if err != nil {
		t.Fatal(err)
	}
	id := identity[:26]
	const tenant = "00000000000000000000000001"
	active := "ACTIVE"
	job := SyncJob{ID: id, TenantID: tenant, SourceID: "00000000000000000000000005", SubsystemCode: "settlement", Status: "QUEUED", ActiveKey: &active, RequestedAt: time.Now().UTC()}
	if err = db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Where("tenant_id=? AND id=?", tenant, id).Delete(&SyncJob{}).Error; err != nil {
			t.Errorf("owned row cleanup failed: %v", err)
		}
	})
	r := &Runner{aggDB: db, tenantID: tenant}
	l, ok, err := r.claimQueueJob(ctx, job, 300*time.Millisecond)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var locked SyncJob
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "tenant_id=? AND id=?", tenant, id).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.renewQueueLease(ctx, l, time.Minute) }()
	// 真实 InnoDB 行锁等待跨越旧租约截止时间。若在
	// 等待前读取时钟，锁释放后会错误复活该租约。
	select {
	case err = <-done:
		t.Fatalf("renewal did not wait for row lock: %v", err)
	case <-time.After(600 * time.Millisecond):
	}
	if err = tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, ErrSyncLeaseLost) {
			t.Fatalf("waiting heartbeat revived expired owner: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = db.First(&job, "id=?", id).Error; err != nil {
		t.Fatal(err)
	}
	current, ok, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = r.finishQueueJob(ctx, current, nil, false); err != nil {
		t.Fatal(err)
	}
}
