package aggregation

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
)

// reconciliationFixture 创建仅用于对账规则的隔离内存维表。
// 参数：t 为测试上下文。
// 返回值：执行器及其聚合数据库；建表失败终止测试。
func reconciliationFixture(t *testing.T) (*Runner, *gorm.DB) {
	t.Helper()
	db := newScanTestDB(t)
	for _, query := range []string{
		`CREATE TABLE dim_contract(tenant_id TEXT,contract_id TEXT, PRIMARY KEY(tenant_id,contract_id))`,
		`CREATE TABLE dim_customer(tenant_id TEXT,customer_id INTEGER, PRIMARY KEY(tenant_id,customer_id))`,
		`CREATE TABLE dim_opportunity(tenant_id TEXT,opportunity_id INTEGER, PRIMARY KEY(tenant_id,opportunity_id))`,
		`CREATE TABLE dim_contract_approval(tenant_id TEXT,contract_id TEXT,approval_id TEXT, PRIMARY KEY(tenant_id,contract_id,approval_id))`,
	} {
		if err := db.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&dimensionSyncKey{}); err != nil {
		t.Fatal(err)
	}
	return &Runner{aggDB: db, tenantID: "a"}, db
}

// TestDimensionReconciliationDeletesOnlySuccessfulTenantSnapshot 验证完整成功后才按租户删除。
// 参数：t 为测试上下文。
// 返回值：无；误删其他租户或保留缺失项时失败。
func TestDimensionReconciliationDeletesOnlySuccessfulTenantSnapshot(t *testing.T) {
	r, db := reconciliationFixture(t)
	if err := db.Exec(`INSERT INTO dim_contract VALUES ('a','kept'),('a','gone'),('b','gone')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.stageDimensionKeys(context.Background(), "run", "dim_contract", []string{"kept"}); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDimensions(context.Background(), "contract_management", "run"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := db.Table("dim_contract").Select("tenant_id || ':' || contract_id").Order("tenant_id,contract_id").Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a:kept" || ids[1] != "b:gone" {
		t.Fatalf("remaining=%v", ids)
	}
	var count int64
	db.Model(&dimensionSyncKey{}).Count(&count)
	if count != 0 {
		t.Fatalf("staging remaining=%d", count)
	}
}

// TestDimensionSnapshotFailureNeverDeletes 验证扫描失败、取消和许可失效不删除原维表。
// 参数：t 为测试上下文。
// 返回值：无；任一失败路径清库或吞错时失败。
func TestDimensionSnapshotFailureNeverDeletes(t *testing.T) {
	for _, mode := range []string{"read", "cancel", "license"} {
		t.Run(mode, func(t *testing.T) {
			r, db := reconciliationFixture(t)
			db.Exec(`INSERT INTO dim_contract VALUES ('a','original')`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			denied := errors.New("denied")
			err := r.runDimensionSnapshot(ctx, newScanTestDB(t), "contract_management", func(_ *gorm.DB, run string) error {
				if err := r.stageDimensionKeys(ctx, run, "dim_contract", []string{"new"}); err != nil {
					return err
				}
				switch mode {
				case "read":
					return denied
				case "cancel":
					cancel()
				case "license":
					r.LicenseCheck = func(context.Context) error { return denied }
				}
				return nil
			})
			if err == nil {
				t.Fatal("failure swallowed")
			}
			var count int64
			db.Table("dim_contract").Where("tenant_id='a' AND contract_id='original'").Count(&count)
			if count != 1 {
				t.Fatal("failure removed original")
			}
		})
	}
}

// TestDimensionCRMReconciliationIsAtomic 验证任一CRM删除失败回滚全部删除。
// 参数：t 为测试上下文。
// 返回值：无；部分提交或错误吞没时失败。
func TestDimensionCRMReconciliationIsAtomic(t *testing.T) {
	r, db := reconciliationFixture(t)
	db.Exec(`INSERT INTO dim_customer VALUES ('a',1)`)
	db.Exec(`DROP TABLE dim_opportunity`)
	if err := r.reconcileDimensions(context.Background(), "customer_and_opportunity", "empty"); err == nil {
		t.Fatal("missing table accepted")
	}
	var count int64
	db.Table("dim_customer").Count(&count)
	if count != 1 {
		t.Fatal("customer deletion partially committed")
	}
}

// TestDimensionReconciliationEmptyAndCompositeKeys 验证成功空源及审批复合键的精确对账。
// 参数：t 为测试上下文。
// 返回值：无；空源未清理或审批跨合同误保留时失败。
func TestDimensionReconciliationEmptyAndCompositeKeys(t *testing.T) {
	r, db := reconciliationFixture(t)
	db.Exec(`INSERT INTO dim_contract_approval VALUES ('a','c1','p1'),('a','c2','p1'),('b','c2','p1')`)
	if err := r.stageDimensionKeys(context.Background(), "approval", "dim_contract_approval", []string{approvalReconciliationKey("c1", "p1")}); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDimensions(context.Background(), ContractApprovalSource, "approval"); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Table("dim_contract_approval").Count(&count)
	if count != 2 {
		t.Fatalf("approval rows=%d", count)
	}
	db.Exec(`INSERT INTO dim_customer VALUES ('a',1),('b',1)`)
	db.Exec(`INSERT INTO dim_opportunity VALUES ('a',2),('b',2)`)
	if err := r.runDimensionSnapshot(context.Background(), newScanTestDB(t), "customer_and_opportunity", func(*gorm.DB, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	db.Table("dim_customer").Where("tenant_id='a'").Count(&count)
	if count != 0 {
		t.Fatal("successful empty source retained customer")
	}
	db.Table("dim_opportunity").Where("tenant_id='b'").Count(&count)
	if count != 1 {
		t.Fatal("other tenant removed")
	}
}

// TestDimensionStageCleanupIsBounded 验证只回收本租户旧轮次且每轮回收有界。
// 参数：t 为测试上下文。
// 返回值：无；回收当前轮次、其他租户或超量时失败。
func TestDimensionStageCleanupIsBounded(t *testing.T) {
	r, db := reconciliationFixture(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	keys := []dimensionSyncKey{}
	for i := 0; i < 503; i++ {
		keys = append(keys, dimensionSyncKey{TenantID: "a", RunID: "old", Dimension: "dim_contract", SourceKey: fmtInt(i), CreatedAt: old})
	}
	keys = append(keys, dimensionSyncKey{TenantID: "b", RunID: "old", Dimension: "dim_contract", SourceKey: "other", CreatedAt: old})
	if err := db.Create(&keys).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupDimensionKeys(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&dimensionSyncKey{}).Where("tenant_id='a'").Count(&count)
	if count != 3 {
		t.Fatalf("old staging=%d, want 3 after bounded batch", count)
	}
	db.Model(&dimensionSyncKey{}).Where("tenant_id='b'").Count(&count)
	if count != 1 {
		t.Fatal("other tenant staging removed")
	}
}

// TestDimensionDeleteFencesLicenseAtCommit 验证删除之后许可失效会回滚删除与暂存清理。
// 参数：t 为测试上下文。
// 返回值：无；提交前许可未拦截或暂存未回滚时失败。
func TestDimensionDeleteFencesLicenseAtCommit(t *testing.T) {
	r, db := reconciliationFixture(t)
	db.Exec(`INSERT INTO dim_contract VALUES ('a','original')`)
	if err := r.stageDimensionKeys(context.Background(), "run", "dim_contract", []string{"new"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	denied := errors.New("license expired before commit")
	r.LicenseCheck = func(context.Context) error {
		calls++
		if calls >= 2 {
			return denied
		}
		return nil
	}
	if err := r.reconcileDimensions(context.Background(), "contract_management", "run"); !errors.Is(err, denied) {
		t.Fatalf("delete=%v", err)
	}
	var count int64
	db.Table("dim_contract").Count(&count)
	if count != 1 {
		t.Fatal("delete survived license rollback")
	}
	db.Model(&dimensionSyncKey{}).Count(&count)
	if count != 1 {
		t.Fatal("staging cleanup survived license rollback")
	}
}

// TestDimensionDeleteRejectsStaleQueueLease 验证队列旧代次不得发布删除。
// 参数：t 为测试上下文。
// 返回值：无；陈旧持有者删除本租户维表时失败。
func TestDimensionDeleteRejectsStaleQueueLease(t *testing.T) {
	r, db := reconciliationFixture(t)
	if err := db.AutoMigrate(&SyncJob{}); err != nil {
		t.Fatal(err)
	}
	owner := "new-owner"
	expiry := time.Now().UTC().Add(time.Hour)
	if err := db.Create(&SyncJob{TenantID: "a", ID: "job", Status: "RUNNING", LeaseOwner: &owner, LeaseGeneration: 2, LeaseExpiresAt: &expiry}).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO dim_contract VALUES ('a','original')`)
	ctx := context.WithValue(context.Background(), queueLeaseKey{}, queueLease{tenant: "a", job: "job", owner: "old-owner", generation: 1})
	if err := r.reconcileDimensions(ctx, "contract_management", "run"); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("stale lease=%v", err)
	}
	var count int64
	db.Table("dim_contract").Count(&count)
	if count != 1 {
		t.Fatal("stale queue holder deleted dimension")
	}
}

// TestDimensionSnapshotCommitFailureNeverDeletes 验证源快照提交失败禁止删除。
// 参数：t 为测试上下文。
// 返回值：无；已回滚源事务被视为完整成功时失败。
func TestDimensionSnapshotCommitFailureNeverDeletes(t *testing.T) {
	r, db := reconciliationFixture(t)
	db.Exec(`INSERT INTO dim_contract VALUES ('a','original')`)
	err := r.runDimensionSnapshot(context.Background(), newScanTestDB(t), "contract_management", func(src *gorm.DB, _ string) error { return src.Rollback().Error })
	if err == nil {
		t.Fatal("source commit failure swallowed")
	}
	var count int64
	db.Table("dim_contract").Count(&count)
	if count != 1 {
		t.Fatal("failed source commit deleted original")
	}
}

// TestDimensionSuccessMarkerSurvivesLaterFailure 验证真实同步成功凭据不会被失败或陈旧轮次清空。
// 参数：t 为测试上下文。
// 返回值：无；成功凭据缺失、失败刷新凭据或陈旧轮次覆盖时失败。
func TestDimensionSuccessMarkerSurvivesLaterFailure(t *testing.T) {
	r, db := reconciliationFixture(t)
	if err := db.AutoMigrate(&SyncSource{}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := db.Create(&SyncSource{TenantID: "a", ID: "source", SubsystemCode: "customer_and_opportunity", LastStatus: "PENDING", CreatedAt: at, UpdatedAt: at}).Error; err != nil {
		t.Fatal(err)
	}
	r.mark(context.Background(), "customer_and_opportunity", statusSuccess, at, nil)
	var source SyncSource
	db.First(&source, "id='source'")
	if source.LastSuccessAt == nil {
		t.Fatal("success credential absent")
	}
	success := *source.LastSuccessAt
	failure := "source incomplete"
	r.mark(context.Background(), "customer_and_opportunity", statusFailed, at.Add(time.Second), &failure)
	r.mark(context.Background(), "customer_and_opportunity", statusSuccess, at.Add(-time.Second), nil)
	source = SyncSource{}
	db.First(&source, "id='source'")
	if source.LastStatus != statusFailed || source.LastSuccessAt == nil || !source.LastSuccessAt.Equal(success) {
		t.Fatalf("success credential overwritten: %+v", source)
	}
}

// TestDimensionQueuedSuccessCredentialUsesCommitClock 验证首次手工成功记录真实数据库完成时刻。
// 参数：t 为测试上下文。
// 返回值：无；手工成功无凭据、使用主机时间或后续失败/暂停清空凭据时失败。
func TestDimensionQueuedSuccessCredentialUsesCommitClock(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	queueTestSyncSource(t, r, job.SubsystemCode, "PENDING", nil, nil)
	lease, claimed, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if err := r.finishQueueJob(ctx, lease, nil, false); err != nil {
		t.Fatal(err)
	}
	source := queueTestSyncSourceRow(t, r, job.SubsystemCode)
	if source.LastSuccessAt == nil {
		t.Fatal("first manual success has no credential")
	}
	credential := *source.LastSuccessAt
	var completed SyncJob
	if err := r.aggDB.First(&completed, "id=?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if completed.FinishedAt == nil || !credential.Equal(*completed.FinishedAt) {
		t.Fatalf("credential=%v database completion=%v", credential, completed.FinishedAt)
	}
	for _, pause := range []bool{false, true} {
		active := "ACTIVE"
		next := SyncJob{TenantID: r.tenantID, ID: "later-" + strconv.FormatBool(pause), SubsystemCode: job.SubsystemCode, Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
		if err := r.aggDB.Create(&next).Error; err != nil {
			t.Fatal(err)
		}
		nextLease, claimed, err := r.claimQueueJob(ctx, next, time.Minute)
		if err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		if err := r.finishQueueJob(ctx, nextLease, errors.New("source incomplete"), pause); err != nil {
			t.Fatal(err)
		}
		after := queueTestSyncSourceRow(t, r, job.SubsystemCode)
		if after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(credential) || after.LastStatus != statusFailed {
			t.Fatalf("failure/pause overwrote credential: %+v", after)
		}
	}
}
