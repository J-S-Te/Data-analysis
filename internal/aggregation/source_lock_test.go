package aggregation

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeScheduledSyncLocker 记录调用并按来源返回固定结果，用于验证 RunOnce 的锁编排。
type fakeScheduledSyncLocker struct {
	acquired map[string]bool
	err      error
	calls    []string
	releases int
}

func (locker *fakeScheduledSyncLocker) TryAcquire(_ context.Context, tenantID, source string) (func() error, bool, error) {
	locker.calls = append(locker.calls, tenantID+"/"+source)
	if locker.err != nil {
		return nil, false, locker.err
	}
	if !locker.acquired[source] {
		return nil, false, nil
	}
	return func() error {
		locker.releases++
		return nil
	}, true, nil
}

// TestScheduledSyncLockNameIsBoundedAndScoped 验证锁名有界、稳定且按租户/来源区分，
// 避免超长或含分隔符的标识改变锁粒度。
func TestScheduledSyncLockNameIsBoundedAndScoped(t *testing.T) {
	name := scheduledSyncLockName("tenant-1", "contract_management")
	if len(name) > 64 {
		t.Fatalf("lock name %q length=%d exceeds MySQL GET_LOCK limit", name, len(name))
	}
	if !strings.HasPrefix(name, scheduledSyncLockPrefix) {
		t.Fatalf("lock name %q missing prefix", name)
	}
	if name != scheduledSyncLockName("tenant-1", "contract_management") {
		t.Fatal("lock name is not deterministic")
	}
	if name == scheduledSyncLockName("tenant-2", "contract_management") ||
		name == scheduledSyncLockName("tenant-1", "settlement") {
		t.Fatal("lock name does not isolate tenant/source")
	}
	if strings.ContainsAny(strings.TrimPrefix(name, scheduledSyncLockPrefix), ": /\\") {
		t.Fatalf("lock name carries unsafe separators: %q", name)
	}
}

// TestProcessScheduledSyncLockerIsExclusive 验证单进程退路也保证同租户/来源互斥，
// 且不同来源互不阻塞、释放后可再次获取。
func TestProcessScheduledSyncLockerIsExclusive(t *testing.T) {
	locker := &processScheduledSyncLockerImpl{}
	ctx := context.Background()
	release, acquired, err := locker.TryAcquire(ctx, "tenant-1", "contract_management")
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}
	if _, second, _ := locker.TryAcquire(ctx, "tenant-1", "contract_management"); second {
		t.Fatal("same tenant/source acquired twice")
	}
	otherRelease, other, err := locker.TryAcquire(ctx, "tenant-1", "settlement")
	if err != nil || !other {
		t.Fatalf("different source blocked: acquired=%v err=%v", other, err)
	}
	if err := otherRelease(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, acquired, err := locker.TryAcquire(ctx, "tenant-1", "contract_management")
	if err != nil || !acquired {
		t.Fatalf("reacquire after release: acquired=%v err=%v", acquired, err)
	}
	_ = again()
}

// TestRunOnceSkipsSourceWhenAnotherProcessHoldsLock 验证锁被占用时本轮不同步、
// 不写同步状态或维表；跳过是正常结果而不是失败。
func TestRunOnceSkipsSourceWhenAnotherProcessHoldsLock(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncSource{}); err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-lock-skip"
	lastRun := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := db.Create(&SyncSource{TenantID: tenant, ID: "src-1", SubsystemCode: "settlement", LastStatus: statusSuccess, LastRunAt: &lastRun, CreatedAt: lastRun, UpdatedAt: lastRun}).Error; err != nil {
		t.Fatal(err)
	}
	locker := &fakeScheduledSyncLocker{acquired: map[string]bool{}}
	runner := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{"settlement": ""}}
	if err := runner.runScheduledSources(context.Background(), locker); err != nil {
		t.Fatalf("skipped source should not fail the round: %v", err)
	}
	if len(locker.calls) != 1 || locker.calls[0] != tenant+"/settlement" || locker.releases != 0 {
		t.Fatalf("lock calls=%v releases=%d", locker.calls, locker.releases)
	}
	var row SyncSource
	if err := db.Where("tenant_id=? AND subsystem_code=?", tenant, "settlement").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastStatus != statusSuccess || row.LastRunAt == nil || !row.LastRunAt.Equal(lastRun) {
		t.Fatalf("skipped source state changed: status=%q last_run=%v", row.LastStatus, row.LastRunAt)
	}
}

// TestRunOnceReleasesLockAndRecordsFailure 验证来源执行失败时仍释放锁，
// 并把失败限定在同步水位，不清空既有维表数据。
func TestRunOnceReleasesLockAndRecordsFailure(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncSource{}, &SyncJob{}); err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-lock-failure"
	if err := db.Create(&SyncSource{TenantID: tenant, ID: "src-2", SubsystemCode: "settlement", LastStatus: statusSuccess, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	locker := &fakeScheduledSyncLocker{acquired: map[string]bool{"settlement": true}}
	runner := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{"settlement": ""}}
	err := runner.runScheduledSources(context.Background(), locker)
	if err == nil || !strings.Contains(err.Error(), "settlement") {
		t.Fatalf("source failure not surfaced: %v", err)
	}
	if locker.releases != 1 {
		t.Fatalf("lock released %d times, want 1", locker.releases)
	}
	var row SyncSource
	if err := db.Where("tenant_id=? AND subsystem_code=?", tenant, "settlement").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastStatus != statusFailed || row.LastError == nil || *row.LastError == "" {
		t.Fatalf("failure watermark missing: %+v", row)
	}
}

// TestRunOnceLockFailureDoesNotTouchSource 验证锁获取失败时不执行来源同步，
// 也不把来源状态写成 RUNNING/FAILED（另一个进程的状态不能被覆盖）。
func TestRunOnceLockFailureDoesNotTouchSource(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncSource{}); err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-lock-error"
	lastRun := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := db.Create(&SyncSource{TenantID: tenant, ID: "src-3", SubsystemCode: "settlement", LastStatus: statusSuccess, LastRunAt: &lastRun, CreatedAt: lastRun, UpdatedAt: lastRun}).Error; err != nil {
		t.Fatal(err)
	}
	locker := &fakeScheduledSyncLocker{err: errors.New("lock backend unavailable")}
	runner := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{"settlement": ""}}
	err := runner.runScheduledSources(context.Background(), locker)
	if err == nil || !strings.Contains(err.Error(), "acquire sync lock") {
		t.Fatalf("lock error not surfaced: %v", err)
	}
	var row SyncSource
	if err := db.Where("tenant_id=? AND subsystem_code=?", tenant, "settlement").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastStatus != statusSuccess || row.LastRunAt == nil || !row.LastRunAt.Equal(lastRun) {
		t.Fatalf("lock failure changed source state: %+v", row)
	}
}

// TestRunQueuedWaitsForScheduledSourceLock 验证手工队列与定时同步共用来源锁：
// 定时同步持锁时队列任务保持 QUEUED、不领取、不执行；释放后才正常进入执行。
func TestRunQueuedWaitsForScheduledSourceLock(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncJob{}, &SyncSource{}); err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-queue-lock"
	active := "ACTIVE"
	job := SyncJob{TenantID: tenant, ID: "queued-lock", SourceID: "src-queue", SubsystemCode: "settlement", Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	release, acquired, err := processScheduledSyncLocker.TryAcquire(context.Background(), tenant, "settlement")
	if err != nil || !acquired {
		t.Fatalf("hold source lock: acquired=%v err=%v", acquired, err)
	}
	runner := &Runner{aggDB: db, tenantID: tenant}
	if err := runner.RunQueued(context.Background()); err != nil {
		t.Fatalf("queued run while source locked: %v", err)
	}
	var got SyncJob
	if err := db.First(&got, "id=?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "QUEUED" || got.StartedAt != nil || got.LeaseOwner != nil {
		t.Fatalf("locked source was claimed: %+v", got)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	// 释放后可领取；本用例来源 DSN 为空，执行失败应在任务行留下 FAILED，而不是仍被锁挡住。
	if err := runner.RunQueued(context.Background()); err != nil {
		t.Fatalf("queued run after release: %v", err)
	}
	if err := db.First(&got, "id=?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != statusFailed || got.StartedAt == nil {
		t.Fatalf("queued job did not execute after release: %+v", got)
	}
}

// TestMySQLScheduledSyncLockExcludesSecondHolder 在隔离测试库上验证 GET_LOCK 互斥。
// 参数：t 为测试上下文；未提供显式隔离 DSN 文件时跳过，不连接业务库。
func TestMySQLScheduledSyncLockExcludesSecondHolder(t *testing.T) {
	file := strings.TrimSpace(os.Getenv("UIP_BI_DIM_SYNC_TEST_DSN_FILE"))
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated lock DB")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = pool.Close() })
	locker := mysqlScheduledSyncLocker{db: db}
	const tenant = "synthetic-lock-tenant1"
	release, acquired, err := locker.TryAcquire(ctx, tenant, "contract_management")
	if err != nil || !acquired {
		t.Fatalf("first holder: acquired=%v err=%v", acquired, err)
	}
	if _, second, err := locker.TryAcquire(ctx, tenant, "contract_management"); err != nil || second {
		t.Fatalf("second holder: acquired=%v err=%v", second, err)
	}
	otherRelease, other, err := locker.TryAcquire(ctx, tenant, "settlement")
	if err != nil || !other {
		t.Fatalf("different source blocked: acquired=%v err=%v", other, err)
	}
	if err := otherRelease(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, acquired, err := locker.TryAcquire(ctx, tenant, "contract_management")
	if err != nil || !acquired {
		t.Fatalf("reacquire after release: acquired=%v err=%v", acquired, err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}
