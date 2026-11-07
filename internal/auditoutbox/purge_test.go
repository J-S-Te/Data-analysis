package auditoutbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// purgeFakeStore 记录清理调用，不连接数据库：用于验证保留期与分批循环。
type purgeFakeStore struct {
	Store
	purgeCalls int
	batches    []int
	cutoffs    []time.Time
	remaining  int64
	deleted    int64
	err        error
}

func (s *purgeFakeStore) PurgeDelivered(_ context.Context, cutoff time.Time, batchSize int) (int64, error) {
	s.purgeCalls++
	s.batches = append(s.batches, batchSize)
	s.cutoffs = append(s.cutoffs, cutoff)
	if s.err != nil {
		return 0, s.err
	}
	if s.remaining <= 0 {
		return 0, nil
	}
	removed := int64(batchSize)
	if removed > s.remaining {
		removed = s.remaining
	}
	s.remaining -= removed
	s.deleted += removed
	return removed, nil
}

// noPurgeStore 只实现 Store：用来确认投递器不会对没有清理能力的实现做假设。
type noPurgeStore struct{ Store }

// purgeRows 造 rows 行指定状态的数据；deliveredAt 为 nil 表示未投递。
func purgeRows(t *testing.T, store *GORMStore, tenant, status string, rows int, deliveredAt *time.Time) {
	t.Helper()
	base := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Millisecond)
	for i := 0; i < rows; i++ {
		// 同一测试会多次造同租户同状态的行，ID/event_id 必须唯一，避免撞主键。
		seq := fmt.Sprintf("%s-%s-%d-%02d", tenant, status, time.Now().UnixNano(), i)
		row := outboxRow{
			ID: seq, TenantID: tenant,
			EventID: "event-" + seq, Action: "AUDIT", Result: "SUCCESS",
			DeliveryStatus: status, Attempts: 1, NextAttemptAt: base, DeliveredAt: deliveredAt, CreatedAt: base,
		}
		if err := store.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// 只有"已投递且 delivered_at 早于 cutoff"的行会被删除；
// pending/retry/processing/failed 与未过期的 delivered 行全部保留。
func TestPurgeDeliveredRemovesOnlyExpiredDeliveredRows(t *testing.T) {
	ctx := context.Background()
	store := newReclaimTestStore(t, 3)
	now := time.Now().UTC()
	expired := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-1 * 24 * time.Hour)
	purgeRows(t, store, "tenant-purge", "delivered", 2, &expired)
	purgeRows(t, store, "tenant-purge", "delivered", 1, &recent)
	purgeRows(t, store, "tenant-purge", "pending", 1, nil)
	purgeRows(t, store, "tenant-purge", "retry", 1, nil)
	purgeRows(t, store, "tenant-purge", "processing", 1, nil)
	purgeRows(t, store, "tenant-purge", "failed", 1, nil)

	deleted, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), purgeBatchLimit)
	if err != nil {
		t.Fatalf("PurgeDelivered: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2（只有过期的 delivered 行可删）", deleted)
	}
	var counts []struct {
		DeliveryStatus string
		Total          int64
	}
	if err := store.DB.Model(&outboxRow{}).Select("delivery_status, COUNT(*) AS total").
		Where("tenant_id = ?", "tenant-purge").Group("delivery_status").Scan(&counts).Error; err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, row := range counts {
		got[row.DeliveryStatus] = row.Total
	}
	for status, want := range map[string]int64{"delivered": 1, "pending": 1, "retry": 1, "processing": 1, "failed": 1} {
		if got[status] != want {
			t.Fatalf("%s = %d, want %d（剩余：%+v）", status, got[status], want, got)
		}
	}
}

// 单批删除不超过 batchSize；batchSize 非法时按上限收敛而不是退化为无界删除。
func TestPurgeDeliveredRespectsBatchSize(t *testing.T) {
	ctx := context.Background()
	store := newReclaimTestStore(t, 3)
	now := time.Now().UTC()
	expired := now.Add(-40 * 24 * time.Hour)
	purgeRows(t, store, "tenant-batch", "delivered", 5, &expired)

	// batchSize=0 属于非法输入：必须收敛为上限并真实删除，不能静默变成"永不清理"。
	deleted, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("PurgeDelivered: %v", err)
	}
	if deleted != 5 {
		t.Fatalf("deleted = %d, want 5（batchSize 非法时按上限收敛）", deleted)
	}

	purgeRows(t, store, "tenant-batch-2", "delivered", 5, &expired)
	deletedPerBatch := []int64{}
	for batch := 0; batch < 10; batch++ {
		removed, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), 2)
		if err != nil {
			t.Fatalf("PurgeDelivered: %v", err)
		}
		if removed > 2 {
			t.Fatalf("单批删除 %d 行，超过 batchSize=2", removed)
		}
		if removed == 0 {
			break
		}
		deletedPerBatch = append(deletedPerBatch, removed)
	}
	if len(deletedPerBatch) != 3 || deletedPerBatch[0] != 2 || deletedPerBatch[1] != 2 || deletedPerBatch[2] != 1 {
		t.Fatalf("分批删除序列 = %v, want [2 2 1]", deletedPerBatch)
	}
	// cutoff 早于 delivered_at 时不得删除：保留期语义不能被批次参数带偏。
	purgeRows(t, store, "tenant-recent", "delivered", 2, ptrTime(now.Add(-time.Hour)))
	if removed, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), purgeBatchLimit); err != nil || removed != 0 {
		t.Fatalf("removed = %d err = %v，未过保留期的 delivered 行不得删除", removed, err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

// 调度判定：保留期关闭或未到间隔时不清理。
func TestPurgeDueHonoursRetentionAndInterval(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	// 启用状态用一个显式配置的正保留期验证，不再借用缺省值（缺省=关闭）。
	enabled := 30 * 24 * time.Hour
	if !purgeDue(enabled, time.Time{}, now) {
		t.Fatal("首次轮次必须清理")
	}
	if purgeDue(enabled, now.Add(-purgeInterval+time.Second), now) {
		t.Fatal("间隔内不得重复清理")
	}
	if !purgeDue(enabled, now.Add(-purgeInterval), now) {
		t.Fatal("满一个间隔后必须清理")
	}
	if defaultDeliveredRetention != 0 {
		t.Fatalf("defaultDeliveredRetention = %s, want 0（缺省必须关闭清理）", defaultDeliveredRetention)
	}
	for _, retention := range []time.Duration{0, -time.Second, defaultDeliveredRetention} {
		if purgeDue(retention, time.Time{}, now) {
			t.Fatalf("retention = %s 必须关闭清理", retention)
		}
	}
}

// 缺省、非法取值与 <=0 一律关闭清理（返回 0）；只有正整数天才启用。
// 缺省不得影响启动，也不得删除仅本地审计证据。
func TestDeliveredRetentionFromEnv(t *testing.T) {
	if defaultDeliveredRetention != 0 {
		t.Fatalf("defaultDeliveredRetention = %s, want 0", defaultDeliveredRetention)
	}
	for name, value := range map[string]string{"unset": "", "invalid": "30d", "spaces": "  "} {
		t.Setenv(deliveredRetentionEnv, value)
		if retention := DeliveredRetentionFromEnv(); retention != defaultDeliveredRetention {
			t.Fatalf("%s: retention = %s, want %s（缺省/非法值关闭清理）", name, retention, defaultDeliveredRetention)
		}
		if retention := DeliveredRetentionFromEnv(); retention != 0 {
			t.Fatalf("%s: retention = %s, want 0（关闭清理）", name, retention)
		}
	}
	for name, value := range map[string]string{"zero": "0", "negative": "-5"} {
		t.Setenv(deliveredRetentionEnv, value)
		if retention := DeliveredRetentionFromEnv(); retention > 0 {
			t.Fatalf("%s: retention = %s, want 关闭清理", name, retention)
		}
		if retention := DeliveredRetentionFromEnv(); retention != 0 {
			t.Fatalf("%s: retention = %s, want 0", name, retention)
		}
	}
	t.Setenv(deliveredRetentionEnv, "7")
	if retention := DeliveredRetentionFromEnv(); retention != 7*24*time.Hour {
		t.Fatalf("retention = %s, want 168h", retention)
	}
}

// 单轮清理分批追平，每批都用同一 cutoff 且不超过上限。
func TestPurgeDeliveredBatchesUntilEmpty(t *testing.T) {
	store := &purgeFakeStore{remaining: 2500}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	dispatcher.purgeDelivered(context.Background(), cutoff)

	// 3 次有删除的调用 + 1 次返回 0 收尾。
	if store.purgeCalls != 4 || store.deleted != 2500 {
		t.Fatalf("purgeCalls = %d deleted = %d，want 4/2500", store.purgeCalls, store.deleted)
	}
	for i, batch := range store.batches {
		if batch != purgeBatchSize {
			t.Fatalf("batch[%d] = %d，单批不得超过 %d", i, batch, purgeBatchSize)
		}
		if !store.cutoffs[i].Equal(cutoff) {
			t.Fatalf("cutoff[%d] = %s, want %s", i, store.cutoffs[i], cutoff)
		}
	}
}

// 清理失败只记日志并结束本轮，不热循环重试。
func TestPurgeDeliveredStopsOnError(t *testing.T) {
	store := &purgeFakeStore{err: errors.New("mysql: connection refused")}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	dispatcher.purgeDelivered(context.Background(), time.Now().UTC())
	if store.purgeCalls != 1 {
		t.Fatalf("purgeCalls = %d，失败后必须停止本轮清理", store.purgeCalls)
	}
}

// ctx 取消后立即退出，不再发起新批次。
func TestPurgeDeliveredStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &purgeFakeStore{remaining: 10}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	dispatcher.purgeDelivered(ctx, time.Now().UTC())
	if store.purgeCalls != 0 {
		t.Fatalf("purgeCalls = %d，ctx 已取消时不得再发起删除", store.purgeCalls)
	}
}

// Store 没有清理能力时直接跳过，不 panic。
func TestPurgeDeliveredSkipsStoreWithoutPurger(t *testing.T) {
	dispatcher := &Dispatcher{Store: noPurgeStore{}, WorkerID: "w1"}
	dispatcher.purgeDelivered(context.Background(), time.Now().UTC())
}

// 缺少 DB 时必须返回错误而不是静默成功。
func TestPurgeDeliveredRequiresDB(t *testing.T) {
	if _, err := (&GORMStore{}).PurgeDelivered(context.Background(), time.Now().UTC(), 10); err == nil {
		t.Fatal("缺少 DB 时必须返回错误")
	}
}

// 保留期=0（缺省）时 Run 主循环绝不触发清理：已投递行是仅本地审计证据，
// 未经显式配置不得删除。Reporter 为 nil 时 deliverOnce 直接返回，循环空转，
// 正好用于只观察清理调度。
func TestRunDoesNotPurgeWhenRetentionDisabled(t *testing.T) {
	t.Setenv(deliveredRetentionEnv, "0")
	store := &purgeFakeStore{remaining: 10}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1", Poll: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	dispatcher.Run(ctx)
	if store.purgeCalls != 0 || store.deleted != 0 {
		t.Fatalf("purgeCalls = %d deleted = %d，保留期=0 时不得清理", store.purgeCalls, store.deleted)
	}
}

// 缺省（环境变量缺失）同样不清理：Run 读到的保留期必须是 0。
func TestRunDoesNotPurgeWhenRetentionUnset(t *testing.T) {
	t.Setenv(deliveredRetentionEnv, "")
	store := &purgeFakeStore{remaining: 10}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1", Poll: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	dispatcher.Run(ctx)
	if store.purgeCalls != 0 {
		t.Fatalf("purgeCalls = %d，缺省保留期必须关闭清理", store.purgeCalls)
	}
}

// 显式配置正整数后 Run 主循环启用清理（对照组，证明开关真的接上了）。
func TestRunPurgesWhenRetentionConfigured(t *testing.T) {
	t.Setenv(deliveredRetentionEnv, "7")
	store := &purgeFakeStore{remaining: 10}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1", Poll: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	dispatcher.Run(ctx)
	if store.purgeCalls == 0 || store.deleted != 10 {
		t.Fatalf("purgeCalls = %d deleted = %d，显式配置正整数后必须清理", store.purgeCalls, store.deleted)
	}
}
