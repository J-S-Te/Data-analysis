package aggregation

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestParseRunOverlapGuard 验证 AGG_RUN_OVERLAP_GUARD 的默认值与非法值处理。
// 参数：t 为测试上下文。
// 返回值：无；非法值必须报错，由启动装配失败关闭。
func TestParseRunOverlapGuard(t *testing.T) {
	cases := []struct {
		raw     string
		want    RunOverlapGuard
		wantErr bool
	}{
		{raw: "", want: RunOverlapGuardWarn},
		{raw: "  ", want: RunOverlapGuardWarn},
		{raw: "warn", want: RunOverlapGuardWarn},
		{raw: "ENFORCE", want: RunOverlapGuardEnforce},
		{raw: " enforce ", want: RunOverlapGuardEnforce},
		{raw: "off", wantErr: true},
		{raw: "1", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseRunOverlapGuard(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseRunOverlapGuard(%q) error = nil, want rejection", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ParseRunOverlapGuard(%q) = %q/%v, want %q", tc.raw, got, err, tc.want)
		}
	}
}

// newSyncRunTestDB 打开独立的 sqlite 内存库并建好 sync_run 表。
// 参数：t 为测试上下文。
// 返回值：GORM 连接。
func newSyncRunTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncRun{}); err != nil {
		t.Fatalf("migrate sync_run: %v", err)
	}
	return db
}

// TestSyncRunRoundRecordsStartAndFinish 验证一轮同步的起止记录：
// begin 写未结束行，finish 补齐 finished_at，且 owner 不含凭据。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSyncRunRoundRecordsStartAndFinish(t *testing.T) {
	db := newSyncRunTestDB(t)
	const tenant = "tenant-sync-run"
	runner := &Runner{aggDB: db, tenantID: tenant}
	start := time.Now().UTC()
	if !runner.beginSyncRun(context.Background(), start) {
		t.Fatal("beginSyncRun() = false, want recorded")
	}
	var row SyncRun
	if err := db.Where("tenant_id = ? AND subsystem_code = ?", tenant, AggregationRunSubsystem).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt != nil {
		t.Fatalf("finished_at = %v, want NULL while running", row.FinishedAt)
	}
	if row.Owner == "" || strings.Contains(row.Owner, "@") {
		t.Fatalf("owner = %q, want hostname:pid without credentials", row.Owner)
	}
	if !row.RunStartedAt.Equal(syncRunStart(start)) {
		t.Fatalf("run_started_at = %v, want %v", row.RunStartedAt, syncRunStart(start))
	}
	runner.finishSyncRun(context.Background(), start)
	if err := db.Where("tenant_id = ? AND subsystem_code = ?", tenant, AggregationRunSubsystem).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt == nil {
		t.Fatal("finished_at 未被补齐")
	}
}

// TestPrecomputeOverlapGuardSkipsDerivedFactsButKeepsSourceWork 验证重叠检测与策略差异：
// 未结束且开始更早的轮次会让 precompute 跳过；warn 只 WARN 不返回错误，
// enforce 返回错误；已结束或超过陈旧阈值的轮次不算重叠。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPrecomputeOverlapGuardSkipsDerivedFactsButKeepsSourceWork(t *testing.T) {
	const tenant = "tenant-overlap"
	start := time.Now().UTC()

	openEarlier := func(t *testing.T, db *gorm.DB, age time.Duration, finished bool) {
		t.Helper()
		row := SyncRun{
			TenantID: tenant, SubsystemCode: AggregationRunSubsystem,
			RunStartedAt: syncRunStart(start.Add(-age)), Owner: "other-instance:1",
		}
		if finished {
			at := syncRunStart(start.Add(-age + time.Minute))
			row.FinishedAt = &at
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name       string
		guard      RunOverlapGuard
		insert     func(t *testing.T, db *gorm.DB)
		wantSkip   bool
		wantErr    bool
		wantCalled bool
	}{
		{
			name: "warn skips precompute without failing the round", guard: RunOverlapGuardWarn,
			insert: func(t *testing.T, db *gorm.DB) { openEarlier(t, db, 5*time.Minute, false) },
			wantSkip: true, wantCalled: false,
		},
		{
			name: "enforce skips precompute and reports the overlap", guard: RunOverlapGuardEnforce,
			insert: func(t *testing.T, db *gorm.DB) { openEarlier(t, db, 5*time.Minute, false) },
			wantSkip: true, wantErr: true, wantCalled: false,
		},
		{
			name: "finished earlier run is not an overlap", guard: RunOverlapGuardEnforce,
			insert: func(t *testing.T, db *gorm.DB) { openEarlier(t, db, 5*time.Minute, true) },
			wantSkip: false, wantCalled: true,
		},
		{
			name: "stale unfinished run no longer blocks precompute", guard: RunOverlapGuardEnforce,
			insert: func(t *testing.T, db *gorm.DB) { openEarlier(t, db, syncRunStaleAfter+time.Hour, false) },
			wantSkip: false, wantCalled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newSyncRunTestDB(t)
			tc.insert(t, db)
			runner := &Runner{aggDB: db, tenantID: tenant, RunOverlapGuard: tc.guard}
			called := 0
			runner.precomputeSigning = func(context.Context) error { called++; return nil }
			err := runner.runPrecompute(context.Background(),
				map[string]string{"contract_management": "redacted"},
				map[string]bool{"contract_management": true}, start, true)
			if tc.wantErr {
				if err == nil || (!strings.Contains(err.Error(), "overlap") && !strings.Contains(err.Error(), "precompute skipped")) {
					t.Fatalf("runPrecompute() error = %v, want overlap rejection", err)
				}
			} else if err != nil {
				t.Fatalf("runPrecompute() error = %v, want nil", err)
			}
			if tc.wantSkip != (called == 0) {
				t.Fatalf("precompute signing called %d times, wantSkip=%v", called, tc.wantSkip)
			}
		})
	}
}

// TestPrecomputeOverlapGuardWithoutRunRecordContinues 验证 sync_run 不可用（表未迁移/写入失败）
// 时不阻断 precompute：守卫是可观测性增强，不能因为审计表缺失冻结派生事实。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPrecomputeOverlapGuardWithoutRunRecordContinues(t *testing.T) {
	db := newScanTestDB(t) // 刻意不建 sync_run
	runner := &Runner{aggDB: db, tenantID: "tenant-norun", RunOverlapGuard: RunOverlapGuardEnforce}
	called := 0
	runner.precomputeSigning = func(context.Context) error { called++; return nil }
	err := runner.runPrecompute(context.Background(),
		map[string]string{"contract_management": "redacted"},
		map[string]bool{"contract_management": true}, time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("runPrecompute() error = %v, want nil without run record", err)
	}
	if called != 1 {
		t.Fatalf("precompute signing called %d times, want 1", called)
	}
}

// TestRunScheduledSourcesRecordsRoundEvenWhenSourceSkipped 验证来源被锁跳过时本轮仍记录起止：
// 运维可据此区分“这一轮跑过”与“这一轮根本没启动”。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRunScheduledSourcesRecordsRoundEvenWhenSourceSkipped(t *testing.T) {
	db := newSyncRunTestDB(t)
	if err := db.AutoMigrate(&SyncSource{}); err != nil {
		t.Fatal(err)
	}
	const tenant = "tenant-round-record"
	locker := &fakeScheduledSyncLocker{acquired: map[string]bool{}}
	runner := &Runner{aggDB: db, tenantID: tenant, sources: map[string]string{"settlement": ""}}
	if err := runner.runScheduledSources(context.Background(), locker); err != nil {
		t.Fatalf("runScheduledSources() error = %v", err)
	}
	var row SyncRun
	if err := db.Where("tenant_id = ? AND subsystem_code = ?", tenant, AggregationRunSubsystem).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt == nil {
		t.Fatal("skipped round did not record finished_at")
	}
	if _, err := runner.overlappingSyncRuns(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 同一轮结束后不再被判为重叠。
	overlaps, err := runner.overlappingSyncRuns(context.Background(), row.RunStartedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if overlaps != 0 {
		t.Fatalf("finished round counted as overlap: %d", overlaps)
	}
}
