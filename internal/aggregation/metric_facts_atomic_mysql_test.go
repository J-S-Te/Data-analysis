package aggregation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
	"gorm.io/gorm"
)

// metricAtomicFixture 创建仅属于本用例的合成来源与首份真实隔离发布。
// 参数：t为测试上下文，suffix为隔离租户后缀。
// 返回值：运行器及切换下一份来源快照的函数，不连接业务库。
func metricAtomicFixture(t *testing.T, suffix string) (*Runner, func()) {
	t.Helper()
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_METRIC_FACTS_TEST_DSN_FILE", true)
	tenant := "mf-atomic-" + suffix
	tables := []string{"dashboard_metric_snapshot", "sync_job", "sync_source", "api_contract_dashboard", "sync_run"}
	for _, table := range tables {
		dashboardAcceptanceExec(t, db, "DELETE FROM "+table+" WHERE tenant_id=?", tenant)
	}
	t.Cleanup(func() {
		for _, table := range tables {
			dashboardAcceptanceExec(t, db, "DELETE FROM "+table+" WHERE tenant_id=?", tenant)
		}
	})
	bundle := metricfacts.Bundle{SchemaVersion: 1, TenantID: tenant, SourceCode: "contract_management", SnapshotAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Complete: true, Basis: "atomic A", Rows: []metricfacts.Row{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/internal/v1/dashboard/metrics" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": bundle})
			return
		}
		_, _ = fmt.Fprintf(w, `{"code":"OK","data":{"tenant_id":%q,"total_amount_minor":0,"total_contracts":0,"currency_totals":[],"approval_contracts":0,"active_contracts":0,"expired_contracts":0}}`, tenant)
	}))
	t.Cleanup(server.Close)
	api := NewAPISyncRunner(db, APISyncOptions{TenantID: tenant, MetricFactsEnabled: true, ContractInternalURL: server.URL + "/internal/v1/dashboard", ContractCredential: MachineCredential{ClientID: "metrics-atomic", Scope: "dashboard.contract.read"}})
	api.tokens["metrics-atomic"] = cachedMachineToken{value: "synthetic-token", expiresAt: time.Now().Add(time.Hour)}
	r := &Runner{aggDB: db, tenantID: tenant, ProjectAPI: api}
	if err := r.EnsureSyncSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, func() { bundle.Basis = "atomic B"; bundle.SnapshotAt = time.Now().UTC().Format(time.RFC3339Nano) }
}

// metricAtomicState 读取来源当前payload与成功水位。
// 参数：t为测试上下文，r为仅连接隔离schema的运行器。
// 返回值：精确持久化行，用于检查失败回滚。
func metricAtomicState(t *testing.T, r *Runner) (metricfacts.SnapshotRow, SyncSource) {
	t.Helper()
	var snapshot metricfacts.SnapshotRow
	var source SyncSource
	if err := r.aggDB.Where("tenant_id=? AND source_code=?", r.tenantID, "contract_management").Take(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.aggDB.Where("tenant_id=? AND subsystem_code=?", r.tenantID, "contract_metrics").Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	return snapshot, source
}

// TestMetricFactsMySQLSuccessWatermarkAtomic 验证SUCCESS写入失败时定时和队列均不发布新payload。
// 参数：t为隔离MySQL上下文，更新回调仅对新指标SUCCESS执行数据库SIGNAL。
// 返回值：无；错误吞掉、成功水位漂移或旧快照被覆盖时失败。
func TestMetricFactsMySQLSuccessWatermarkAtomic(t *testing.T) {
	for _, mode := range []string{"scheduled", "queued"} {
		t.Run(mode, func(t *testing.T) {
			r, next := metricAtomicFixture(t, mode)
			before, sourceBefore := metricAtomicState(t, r)
			next()
			callback := "metrics-success-failure"
			if err := r.aggDB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				updates, ok := tx.Statement.Dest.(map[string]any)
				if ok && tx.Statement.Table == "sync_source" && updates["last_status"] == "SUCCESS" &&
					strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"]), "contract_metrics") {
					tx.AddError(tx.Exec("SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='metrics success injection'").Error)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.aggDB.Callback().Update().Remove(callback) })
			var err error
			if mode == "queued" {
				var source SyncSource
				r.aggDB.Where("tenant_id=? AND subsystem_code=?", r.tenantID, "contract_metrics").Take(&source)
				active := "ACTIVE"
				job := SyncJob{TenantID: r.tenantID, ID: ulid(), SourceID: source.ID, SubsystemCode: "contract_metrics", Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
				if e := r.aggDB.Create(&job).Error; e != nil {
					t.Fatal(e)
				}
				err = r.RunQueued(context.Background())
			} else {
				err = r.RunOnce(context.Background())
			}
			after, sourceAfter := metricAtomicState(t, r)
			if err == nil || !strings.Contains(err.Error(), "metrics success injection") {
				t.Errorf("SUCCESS write failure concealed: %v", err)
			}
			if after.PayloadJSON != before.PayloadJSON || !after.CollectedAt.Equal(before.CollectedAt) {
				t.Error("failed SUCCESS write published new payload")
			}
			if sourceBefore.LastSuccessAt == nil || sourceAfter.LastSuccessAt == nil || !sourceAfter.LastSuccessAt.Equal(*sourceBefore.LastSuccessAt) {
				t.Error("failed SUCCESS write advanced watermark")
			}
			if err := r.aggDB.Callback().Update().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if err := r.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			recovered, recoveredSource := metricAtomicState(t, r)
			if recovered.PayloadJSON == before.PayloadJSON || recoveredSource.LastStatus != "SUCCESS" {
				t.Fatal("recovery failed")
			}
		})
	}
}

// TestMetricFactsMySQLLeaseFenceRollback 验证发布事务中租约失效会同时回滚payload与成功标记。
// 参数：t为隔离MySQL上下文，仅修改本用例队列租约。
// 返回值：无；过期或跨代租约能发布时失败。
func TestMetricFactsMySQLLeaseFenceRollback(t *testing.T) {
	r, next := metricAtomicFixture(t, "lease")
	before, sourceBefore := metricAtomicState(t, r)
	next()
	active := "ACTIVE"
	job := SyncJob{TenantID: r.tenantID, ID: ulid(), SourceID: sourceBefore.ID, SubsystemCode: "contract_metrics", Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
	if err := r.aggDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	lease, ok, err := r.claimQueueJob(context.Background(), job, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	ctx := context.WithValue(context.Background(), queueLeaseKey{}, lease)
	count := 0
	r.LicenseCheck = func(ctx context.Context) error {
		count++
		if count == 2 {
			return errors.New("synthetic license revoked before commit")
		}
		return nil
	}
	// 聚合事务在写入前后各检查一次许可，第二次拒绝必须回滚。
	if err := r.syncMetricFacts(ctx, "contract_metrics"); err == nil {
		t.Error("late license revocation accepted")
	}
	r.LicenseCheck = nil
	callback := "metrics-expire-during-publish"
	if err := r.aggDB.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "dashboard_metric_snapshot" {
			result := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE sync_job SET lease_expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE tenant_id=? AND id=?", r.tenantID, job.ID)
			tx.AddError(result.Error)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.syncMetricFacts(ctx, "contract_metrics"); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("mid-publication expiry accepted: %v", err)
	}
	if err := r.aggDB.Callback().Update().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if err := r.aggDB.Model(&SyncJob{}).Where("tenant_id=? AND id=?", r.tenantID, job.ID).Update("lease_generation", gorm.Expr("lease_generation+1")).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.syncMetricFacts(ctx, "contract_metrics"); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	after, sourceAfter := metricAtomicState(t, r)
	if after.PayloadJSON != before.PayloadJSON || !sourceAfter.LastSuccessAt.Equal(*sourceBefore.LastSuccessAt) {
		t.Fatal("lease/license failure changed publication")
	}
}

// TestMetricFactsMySQLQueuedPublicationSuccess 验证队列成功仅在payload事务推进一次成功水位。
// 参数：t为显式隔离MySQL上下文，回调只观察本用例的SUCCESS更新。
// 返回值：无；快照未更新、终态错误或重复提交成功水位时失败。
func TestMetricFactsMySQLQueuedPublicationSuccess(t *testing.T) {
	r, next := metricAtomicFixture(t, "queue-success")
	before, source := metricAtomicState(t, r)
	next()
	active := "ACTIVE"
	job := SyncJob{TenantID: r.tenantID, ID: ulid(), SourceID: source.ID, SubsystemCode: "contract_metrics", Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
	if err := r.aggDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	count := 0
	callback := "metrics-observe-success"
	if err := r.aggDB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if ok && tx.Statement.Table == "sync_source" && updates["last_status"] == "SUCCESS" {
			count++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.aggDB.Callback().Update().Remove(callback) })
	if err := r.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, current := metricAtomicState(t, r)
	if after.PayloadJSON == before.PayloadJSON || current.LastStatus != "SUCCESS" || count != 1 {
		t.Fatalf("queue publication failed or success marked twice: %s count=%d", current.LastStatus, count)
	}
	if err := r.aggDB.Where("tenant_id=? AND id=?", r.tenantID, job.ID).Take(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "SUCCESS" || job.ActiveKey != nil {
		t.Fatalf("invalid queue terminal state: %s", job.Status)
	}
}
