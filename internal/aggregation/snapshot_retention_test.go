package aggregation

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
)

// fmtInt 把合成测试编号转换为稳定字符串。
// 参数：n 为测试编号。
// 返回值：十进制字符串。
func fmtInt(n int) string { return strconv.Itoa(n) }

// retentionFixture 创建隔离的API快照测试表。
// 参数：t 为测试上下文。
// 返回值：内存聚合库；建表失败终止测试。
func retentionFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db := newScanTestDB(t)
	for _, table := range []string{"api_contract_dashboard", "api_project_dashboard"} {
		if err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY AUTOINCREMENT,tenant_id TEXT,snapshot_at DATETIME)").Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestSnapshotRetentionKeepsBusinessMonthEndsAndRecentDetails 验证北京月界、近七日和最新快照保留。
// 参数：t 为测试上下文。
// 返回值：无；误删月末样本、近七日样本或其他租户时失败。
func TestSnapshotRetentionKeepsBusinessMonthEndsAndRecentDetails(t *testing.T) {
	for _, table := range []string{"api_contract_dashboard", "api_project_dashboard"} {
		t.Run(table, func(t *testing.T) {
			db := retentionFixture(t)
			loc := time.FixedZone("Asia/Shanghai", 8*3600)
			now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
			times := []string{"2024-10-31T15:59:59Z", "2024-11-02T00:00:00Z", "2024-12-30T00:00:00Z", "2024-12-31T15:59:59Z", "2024-12-31T16:00:00Z", "2026-11-05T00:00:00Z", "2026-11-10T00:00:00Z", "2026-11-10T00:00:00Z"}
			for _, raw := range times {
				at, _ := time.Parse(time.RFC3339, raw)
				if err := db.Exec("INSERT INTO "+table+"(tenant_id,snapshot_at) VALUES('a',?)", at).Error; err != nil {
					t.Fatal(err)
				}
			}
			db.Exec("INSERT INTO "+table+"(tenant_id,snapshot_at) VALUES('b',?)", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
			if err := PruneSnapshotHistory(context.Background(), db, "a", table, now, loc, SnapshotRetentionPolicy{}, nil); err != nil {
				t.Fatal(err)
			}
			var ids []int
			db.Table(table).Order("id").Pluck("id", &ids)
			want := []int{4, 5, 6, 7, 8, 9}
			if len(ids) != len(want) {
				t.Fatalf("ids=%v, want %v", ids, want)
			}
			for i := range want {
				if ids[i] != want[i] {
					t.Fatalf("ids=%v, want %v", ids, want)
				}
			}
		})
	}
}

// TestSnapshotRetentionBoundsAndFixedHighWatermark 验证分批上限、月末同时间排序和并发新增保护。
// 参数：t 为测试上下文。
// 返回值：无；超预算删除、误删新插入历史快照或错误选取同时间月末时失败。
func TestSnapshotRetentionBoundsAndFixedHighWatermark(t *testing.T) {
	db := retentionFixture(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		db.Exec(`INSERT INTO api_project_dashboard(tenant_id,snapshot_at) VALUES ('a',?)`, now.AddDate(0, -1, -1))
	}
	calls := 0
	guard := func(context.Context) error {
		calls++
		if calls == 2 {
			return db.Exec(`INSERT INTO api_project_dashboard(tenant_id,snapshot_at) VALUES ('a',?)`, now.AddDate(-5, 0, 0)).Error
		}
		return nil
	}
	policy := SnapshotRetentionPolicy{BatchSize: 2, MaxBatches: 2}
	if err := PruneSnapshotHistory(context.Background(), db, "a", "api_project_dashboard", now, time.UTC, policy, guard); err != nil {
		t.Fatal(err)
	}
	var ids []int
	db.Table("api_project_dashboard").Order("id").Pluck("id", &ids)
	want := []int{5, 6, 7, 8, 9}
	if len(ids) != len(want) {
		t.Fatalf("remaining=%v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("remaining=%v, want %v", ids, want)
		}
	}
}

// TestSnapshotRetentionRejectsUnsafeInputsAndGuardFailure 验证表白名单、策略边界及失败回滚。
// 参数：t 为测试上下文。
// 返回值：无；危险输入通过、许可失败吞错或已删除时失败。
func TestSnapshotRetentionRejectsUnsafeInputsAndGuardFailure(t *testing.T) {
	for _, table := range []string{"contract_import_history", "api_project_dashboard;DELETE", "dim_contract"} {
		if err := PruneSnapshotHistory(context.Background(), retentionFixture(t), "a", table, time.Now(), time.UTC, SnapshotRetentionPolicy{}, nil); err == nil {
			t.Fatalf("accepted table %q", table)
		}
	}
	db := retentionFixture(t)
	db.Exec(`INSERT INTO api_contract_dashboard(tenant_id,snapshot_at) VALUES ('a','2020-01-01'),('a','2020-01-02')`)
	denied := errors.New("license lost")
	calls := 0
	err := PruneSnapshotHistory(context.Background(), db, "a", "api_contract_dashboard", time.Now(), time.UTC, SnapshotRetentionPolicy{}, func(context.Context) error {
		calls++
		if calls >= 4 {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) {
		t.Fatalf("guard failure=%v", err)
	}
	var count int64
	db.Table("api_contract_dashboard").Count(&count)
	if count != 2 {
		t.Fatalf("guard rollback rows=%d", count)
	}
	for _, policy := range []SnapshotRetentionPolicy{{RecentDays: -1}, {TrendMonths: 121}, {BatchSize: 501}, {MaxBatches: 21}} {
		if err := PruneSnapshotHistory(context.Background(), db, "a", "api_contract_dashboard", time.Now(), time.UTC, policy, nil); err == nil {
			t.Fatalf("unsafe policy=%+v", policy)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PruneSnapshotHistory(ctx, db, "a", "api_contract_dashboard", time.Now(), time.UTC, SnapshotRetentionPolicy{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
}

// TestSnapshotRetentionPreservesTwentyFourMonthTrend 验证两个重复样本压缩后仍有24个业务月。
// 参数：t 为测试上下文。
// 返回值：无；项目趋势被压缩为最新一条或超过24月时失败。
func TestSnapshotRetentionPreservesTwentyFourMonthTrend(t *testing.T) {
	db := retentionFixture(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for offset := 0; offset < 25; offset++ {
		for day := 0; day < 2; day++ {
			at := month.AddDate(0, -offset, day)
			if err := db.Exec(`INSERT INTO api_project_dashboard(tenant_id,snapshot_at) VALUES ('a',?)`, at).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := PruneSnapshotHistory(context.Background(), db, "a", "api_project_dashboard", now, time.UTC, SnapshotRetentionPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	var at []time.Time
	if err := db.Table("api_project_dashboard").Order("snapshot_at ASC").Pluck("snapshot_at", &at).Error; err != nil {
		t.Fatal(err)
	}
	if len(at) != 24 || at[0].Format("2006-01-02") != "2024-12-02" || at[23].Format("2006-01-02") != "2026-11-02" {
		t.Fatalf("retained month checkpoints=%v", at)
	}
}

// TestSnapshotRetentionAlwaysKeepsGlobalLatest 验证来源长期停更仍保留全局最新快照。
// 参数：t 为测试上下文。
// 返回值：无；24月之外的最后成功快照被清空时失败。
func TestSnapshotRetentionAlwaysKeepsGlobalLatest(t *testing.T) {
	db := retentionFixture(t)
	db.Exec(`INSERT INTO api_contract_dashboard(tenant_id,snapshot_at) VALUES ('a','2020-01-01'),('a','2020-01-02'),('a','2020-01-02')`)
	if err := PruneSnapshotHistory(context.Background(), db, "a", "api_contract_dashboard", time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), time.UTC, SnapshotRetentionPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	db.Table("api_contract_dashboard").Pluck("id", &ids)
	if len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("global latest=%v", ids)
	}
}
