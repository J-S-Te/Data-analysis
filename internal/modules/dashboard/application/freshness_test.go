package application

import (
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// A12：source_updated_at、synced_at、computed_at 是三个独立事实。
// 查询时间不能被序列化成源数据更新时间，陈旧数据必须带原因标记。
func TestBuildFreshnessKeepsSourceSyncAndComputeTimesIndependent(t *testing.T) {
	source := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)
	synced := time.Date(2026, 10, 22, 6, 45, 0, 0, time.UTC)
	computed := time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)

	got := BuildFreshness(FreshnessInput{
		SourceUpdatedAt: &source,
		LastSyncAt:      &synced,
		SyncStatus:      domain.SyncStatusSuccess,
		SyncSourceKnown: true,
		SyncEnabled:     true,
	}, computed)

	if got.SourceUpdatedAt != "2026-10-19T08:30:00Z" {
		t.Fatalf("source_updated_at = %q, want source row time", got.SourceUpdatedAt)
	}
	if got.SyncedAt != "2026-10-22T06:45:00Z" {
		t.Fatalf("synced_at = %q, want last successful sync", got.SyncedAt)
	}
	if got.ComputedAt != "2026-10-22T09:45:00Z" {
		t.Fatalf("computed_at = %q, want query computation time", got.ComputedAt)
	}
	if got.SyncStatus != domain.SyncStatusSuccess || got.Stale || got.StaleReason != "" {
		t.Fatalf("freshness = %#v, want fresh success status", got)
	}
}

func TestBuildFreshnessMarksStaleWithoutFakingSyncTime(t *testing.T) {
	computed := time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)
	source := computed.Add(-72 * time.Hour)
	recent := computed.Add(-3 * time.Hour)
	aged := computed.Add(-49 * time.Hour)

	cases := []struct {
		name       string
		input      FreshnessInput
		wantStatus string
		wantSynced string
		wantReason string
	}{
		{
			name:       "failed last sync keeps old data but is stale",
			input:      FreshnessInput{SourceUpdatedAt: &source, LastSyncAt: &recent, SyncStatus: domain.SyncStatusFailed, SyncSourceKnown: true, SyncEnabled: true},
			wantStatus: domain.SyncStatusFailed,
			wantReason: "失败",
		},
		{
			name:       "disabled source stops freshness claims",
			input:      FreshnessInput{SourceUpdatedAt: &source, LastSyncAt: &recent, SyncStatus: domain.SyncStatusSuccess, SyncSourceKnown: true, SyncEnabled: false},
			wantStatus: domain.SyncStatusSuccess,
			wantSynced: "2026-10-22T06:45:00Z",
			wantReason: "停用",
		},
		{
			name:       "successful sync older than T+1 tolerance is stale",
			input:      FreshnessInput{SourceUpdatedAt: &source, LastSyncAt: &aged, SyncStatus: domain.SyncStatusSuccess, SyncSourceKnown: true, SyncEnabled: true},
			wantStatus: domain.SyncStatusSuccess,
			wantSynced: "2026-10-20T08:45:00Z",
			wantReason: "48",
		},
		{
			name:       "running sync is not a success watermark",
			input:      FreshnessInput{SourceUpdatedAt: &source, LastSyncAt: &recent, SyncStatus: domain.SyncStatusRunning, SyncSourceKnown: true, SyncEnabled: true},
			wantStatus: domain.SyncStatusRunning,
			wantReason: "进行中",
		},
		{
			name:       "unknown when no sync source is registered",
			input:      FreshnessInput{SourceUpdatedAt: &source, SyncSourceKnown: false},
			wantStatus: domain.SyncStatusUnknown,
			wantReason: "尚无源同步记录",
		},
		{
			name:       "success without a watermark is not trusted",
			input:      FreshnessInput{SourceUpdatedAt: &source, SyncStatus: domain.SyncStatusSuccess, SyncSourceKnown: true, SyncEnabled: true},
			wantStatus: domain.SyncStatusSuccess,
			wantReason: "水位",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := BuildFreshness(test.input, computed)
			if got.SyncStatus != test.wantStatus {
				t.Fatalf("sync_status = %q, want %q", got.SyncStatus, test.wantStatus)
			}
			if !got.Stale {
				t.Fatalf("stale = false, want stale data marker")
			}
			if !strings.Contains(got.StaleReason, test.wantReason) {
				t.Fatalf("stale_reason = %q, want it to contain %q", got.StaleReason, test.wantReason)
			}
			if test.wantSynced == "" {
				if got.SyncedAt != "" {
					t.Fatalf("synced_at = %q, want empty when no successful sync is known", got.SyncedAt)
				}
			} else if got.SyncedAt != test.wantSynced {
				t.Fatalf("synced_at = %q, want %q", got.SyncedAt, test.wantSynced)
			}
		})
	}
}

func TestBuildFreshnessKeepsFreshSuccessInsideTolerance(t *testing.T) {
	computed := time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)
	synced := computed.Add(-47 * time.Hour)
	got := BuildFreshness(FreshnessInput{
		LastSyncAt:      &synced,
		SyncStatus:      domain.SyncStatusSuccess,
		SyncSourceKnown: true,
		SyncEnabled:     true,
	}, computed)
	if got.Stale {
		t.Fatalf("freshness = %#v, want not stale inside 48h tolerance", got)
	}
}

// 全租户分支读取持久快照：保留既有 snapshot_at 语义，同时给出独立的查询时间。
func TestBuildFreshnessPersistedSnapshotDoesNotRegressTenantBranch(t *testing.T) {
	computed := time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)
	got := BuildFreshness(FreshnessInput{
		PersistedSnapshot:   true,
		PersistedSnapshotAt: "2026-10-21 03:00:00.000",
	}, computed)
	if got.SyncStatus != domain.SyncStatusSnapshot {
		t.Fatalf("sync_status = %q, want persisted snapshot status", got.SyncStatus)
	}
	if got.SyncedAt != "2026-10-21 03:00:00.000" {
		t.Fatalf("synced_at = %q, want stored snapshot time", got.SyncedAt)
	}
	if got.ComputedAt != "2026-10-22T09:45:00Z" {
		t.Fatalf("computed_at = %q, want independent query time", got.ComputedAt)
	}
	if got.SourceUpdatedAt != "" || got.Stale {
		t.Fatalf("persisted snapshot = %#v, want no fabricated source time and no stale flag", got)
	}
}

// 持久快照也必须按写入水位判定陈旧；缺失、非法或未来水位不能被当成新数据。
func TestBuildFreshnessPersistedSnapshotAgeAndInvalidTimes(t *testing.T) {
	computed := time.Date(2026, 10, 22, 9, 45, 0, 0, time.UTC)
	cases := []struct {
		name       string
		storedAt   string
		wantStale  bool
		wantReason string
	}{
		{name: "recent snapshot", storedAt: "2026-10-21T09:45:00Z"},
		{name: "exactly 48 hours", storedAt: "2026-10-20T09:45:00Z"},
		{name: "MySQL timestamp at boundary", storedAt: "2026-10-20 09:45:00.000"},
		{name: "offset timestamp at boundary", storedAt: "2026-10-20T17:45:00+08:00"},
		{name: "older than 48 hours", storedAt: "2026-10-20T09:44:59.999999999Z", wantStale: true, wantReason: "48"},
		{name: "future timestamp", storedAt: "2026-10-22T09:45:01Z", wantStale: true, wantReason: "晚于"},
		{name: "missing timestamp", storedAt: "", wantStale: true, wantReason: "水位"},
		{name: "blank timestamp", storedAt: "  ", wantStale: true, wantReason: "水位"},
		{name: "invalid timestamp", storedAt: "not-a-time", wantStale: true, wantReason: "水位"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := BuildFreshness(FreshnessInput{PersistedSnapshot: true, PersistedSnapshotAt: test.storedAt}, computed)
			if got.Stale != test.wantStale {
				t.Fatalf("freshness=%+v, want stale=%v", got, test.wantStale)
			}
			if test.wantReason == "" && got.StaleReason != "" || test.wantReason != "" && !strings.Contains(got.StaleReason, test.wantReason) {
				t.Fatalf("stale_reason=%q, want reason containing %q", got.StaleReason, test.wantReason)
			}
			if got.SyncStatus != domain.SyncStatusSnapshot || got.SyncedAt != strings.TrimSpace(test.storedAt) {
				t.Fatalf("freshness=%+v, want original persisted snapshot status and time", got)
			}
			if got.SourceUpdatedAt != "" || got.ComputedAt != "2026-10-22T09:45:00Z" {
				t.Fatalf("freshness=%+v, want no fabricated source time and independent query time", got)
			}
		})
	}
}

func TestProjectionFailurePreservesPreviousSuccessTime(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	success := now.Add(-48 * time.Hour)
	attempt := now.Add(-time.Minute)
	for _, status := range []string{"SUCCESS", "RUNNING", "FAILED"} {
		for _, enabled := range []bool{true, false} {
			got := BuildFreshness(FreshnessInput{LastSuccessAt: &success, LastSyncAt: &attempt, SyncStatus: status, SyncSourceKnown: true, SyncEnabled: enabled}, now)
			if got.SyncedAt != FormatDashboardTimestamp(&success) || got.Stale != (status != "SUCCESS" || !enabled) {
				t.Fatalf("status=%s enabled=%v got=%+v", status, enabled, got)
			}
		}
	}
	old := success.Add(-time.Second)
	got := BuildFreshness(FreshnessInput{LastSuccessAt: &old, SyncStatus: "SUCCESS", SyncSourceKnown: true, SyncEnabled: true}, now)
	if !got.Stale {
		t.Fatal("48h boundary exceeded must be stale")
	}
}
