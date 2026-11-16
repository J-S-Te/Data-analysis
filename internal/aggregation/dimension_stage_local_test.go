package aggregation

import (
	"context"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// localDimensionDB 只连接已迁移的回环local_fix_analysis专库。
// 参数：t 为测试生命周期，仅读取LOCAL_DIMENSION_DSN_FILE私有路径。
// 返回值：独立数据库；未配置跳过，其他目标连接前拒绝。
func localDimensionDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := os.Getenv("LOCAL_DIMENSION_DSN_FILE")
	if path == "" {
		t.Skip("explicit local dimension DSN file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("DSN read failed")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "local_fix_analysis" || !cfg.ParseTime {
		t.Fatal("explicit local_fix_analysis required")
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { pool.Close() })
	return db
}

// TestLocalDimensionStageAtomicPublish 验证完整暂存前live不变，成功后合并与删除原子且整数时间不失真。
// 参数：t 为显式隔离MySQL上下文。
// 返回值：无；半批可见、金额精度损失、跨租户删除或未清理暂存时失败。
func TestLocalDimensionStageAtomicPublish(t *testing.T) {
	db := localDimensionDB(t)
	tenant := fmt.Sprintf("stage-%x", time.Now().UnixNano())
	other := "other" + tenant[5:]
	run := strings.Repeat("a", 32)
	now := time.Now().UTC().Truncate(time.Millisecond)
	defer db.Exec("DELETE FROM dimension_sync_key WHERE tenant_id=?", tenant)
	defer db.Exec("DELETE FROM dim_contract WHERE tenant_id IN (?,?)", tenant, other)
	args := func(owner, id, title string, amount int64, at time.Time) []any {
		return []any{"NORMAL", owner, id, nil, title, "test", "test", "active", &now, nil, nil, nil, amount, "CNY", "user", "org", "identity", "project", at}
	}
	for _, row := range []struct{ tenant, id string }{{tenant, "keep"}, {tenant, "gone"}, {other, "gone"}} {
		if err := db.Exec(dimContractUpsert, args(row.tenant, row.id, "old", 1, now.Add(-time.Hour))...).Error; err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{aggDB: db, tenantID: tenant}
	ctx := context.Background()
	if err := r.stageDimensionWrite(ctx, run, "dim_contract", dimContractUpsert, args(tenant, "keep", "new", 9007199254740991, now)...); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := db.Table("dim_contract").Select("title").Where("tenant_id=? AND contract_id='keep'", tenant).Scan(&title).Error; err != nil || title != "old" {
		t.Fatalf("partial live=%s err=%v", title, err)
	}
	if err := r.stageDimensionKeys(ctx, run, "dim_contract", []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDimensions(ctx, "contract_management", run); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Title       string
		AmountMinor int64
		UpdatedAt   time.Time
	}
	if err := db.Table("dim_contract").Where("tenant_id=? AND contract_id='keep'", tenant).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Title != "new" || row.AmountMinor != 9007199254740991 || !row.UpdatedAt.Equal(now) {
		t.Fatalf("published %+v", row)
	}
	var count int64
	db.Table("dim_contract").Where("tenant_id=?", tenant).Count(&count)
	if count != 1 {
		t.Fatalf("removed rows=%d", count)
	}
	db.Table("dim_contract").Where("tenant_id=?", other).Count(&count)
	if count != 1 {
		t.Fatal("other tenant deleted")
	}
	db.Model(&dimensionSyncKey{}).Where("tenant_id=?", tenant).Count(&count)
	if count != 0 {
		t.Fatal("staging not cleaned")
	}
}

// TestLocalDimensionStageLateFailureRollsBackCRM 验证商机后半阶段失败时客户合并和删除全部回滚。
// 参数：t 为显式隔离MySQL上下文。
// 返回值：无；前半客户变更进入live或旧客户消失时失败。
func TestLocalDimensionStageLateFailureRollsBackCRM(t *testing.T) {
	db := localDimensionDB(t)
	tenant := fmt.Sprintf("crm-%x", time.Now().UnixNano())
	run := strings.Repeat("b", 32)
	now := time.Now().UTC().Truncate(time.Millisecond)
	defer db.Exec("DELETE FROM dimension_sync_key WHERE tenant_id=?", tenant)
	defer db.Exec("DELETE FROM dim_customer WHERE tenant_id=?", tenant)
	args := []any{tenant, uint64(1), "C1", "old", "test", "industry", "region", "active", "user", "org", nil, now.Add(-time.Hour)}
	if err := db.Exec(dimCustomerUpsert, args...).Error; err != nil {
		t.Fatal(err)
	}
	r := &Runner{aggDB: db, tenantID: tenant}
	ctx := context.Background()
	args[3] = "new"
	args[11] = now
	if err := r.stageDimensionWrite(ctx, run, "dim_customer", dimCustomerUpsert, args...); err != nil {
		t.Fatal(err)
	}
	if err := r.stageDimensionKeys(ctx, run, "dim_customer", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	broken := dimensionSyncKey{TenantID: tenant, RunID: run, Dimension: "dim_opportunity", SourceKey: "stage:broken", ParentKey: "stage", CreatedAt: now, Payload: []byte(`{}`)}
	if err := db.Create(&broken).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDimensions(ctx, "customer_and_opportunity", run); err == nil {
		t.Fatal("corrupt second stage accepted")
	}
	var name string
	if err := db.Table("dim_customer").Select("customer_name").Where("tenant_id=? AND customer_id=1", tenant).Scan(&name).Error; err != nil || name != "old" {
		t.Fatalf("partial published=%s %v", name, err)
	}
}
