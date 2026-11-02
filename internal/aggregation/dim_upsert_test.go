package aggregation

import (
	"context"
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

// TestGuardedDimUpsertBuildsStaleSafeAssignments 验证维表 upsert 的语句形状：
// 业务列全部带来源时间守卫，updated_at 自身最后更新，占位符与插入列同序。
// 参数：t 为测试上下文。
// 返回值：无；任何业务列缺少守卫或 updated_at 提前赋值都视为旧数据可覆盖新数据。
func TestGuardedDimUpsertBuildsStaleSafeAssignments(t *testing.T) {
	cases := []struct {
		table   string
		sql     string
		columns int
		guarded []string
	}{
		{"dim_contract", dimContractUpsert, 19, []string{"origin_kind", "title", "amount_minor", "owner_org_id", "project_id"}},
		{"dim_customer", dimCustomerUpsert, 12, []string{"customer_name", "region", "deleted_at"}},
		{"dim_opportunity", dimOpportunityUpsert, 20, []string{"name", "current_stage", "deleted_at"}},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			if !strings.Contains(tc.sql, "INSERT INTO "+tc.table) {
				t.Fatalf("缺少目标表: %s", tc.sql)
			}
			insertPart, updatePart, found := strings.Cut(tc.sql, "ON DUPLICATE KEY UPDATE ")
			if !found {
				t.Fatalf("缺少冲突更新子句: %s", tc.sql)
			}
			if placeholders := strings.Count(insertPart, "?"); placeholders != tc.columns {
				t.Fatalf("占位符 %d，期望 %d", placeholders, tc.columns)
			}
			assignments := strings.Split(updatePart, ", ")
			for _, column := range tc.guarded {
				guard := column + "=IF(VALUES(updated_at)>=updated_at,VALUES(" + column + ")," + column + ")"
				if !strings.Contains(updatePart, guard) {
					t.Fatalf("列 %s 缺少来源时间守卫: %s", column, updatePart)
				}
			}
			last := assignments[len(assignments)-1]
			if last != "updated_at=IF(VALUES(updated_at)>=updated_at,VALUES(updated_at),updated_at)" {
				t.Fatalf("updated_at 必须最后并同样守卫，实际最后为 %q", last)
			}
			for _, assignment := range assignments[:len(assignments)-1] {
				if strings.HasPrefix(assignment, "updated_at=") {
					t.Fatal("updated_at 被提前赋值会绕过其它列的守卫")
				}
			}
		})
	}
}

// TestGuardedDimUpsertRejectsUnguardedShape 验证构造器只生成带守卫的赋值。
// 参数：t 为测试上下文。
// 返回值：无。
func TestGuardedDimUpsertRejectsUnguardedShape(t *testing.T) {
	sql := guardedDimUpsert("t_dim", []string{"tenant_id", "id", "name", "updated_at"}, []string{"name"})
	want := "INSERT INTO t_dim\n(tenant_id, id, name, updated_at)\nVALUES (?, ?, ?, ?)\nON DUPLICATE KEY UPDATE " +
		"name=IF(VALUES(updated_at)>=updated_at,VALUES(name),name), updated_at=IF(VALUES(updated_at)>=updated_at,VALUES(updated_at),updated_at)"
	if sql != want {
		t.Fatalf("生成语句\n%s\n期望\n%s", sql, want)
	}
}

// TestDimUpsertStaleSourceRowDoesNotOverride 验证真实 MySQL 上较旧来源行不会覆盖较新镜像行。
// 参数：t 为测试上下文，只能使用显式隔离的 data_analysis_test_* 私有 DSN 文件。
// 返回值：无；缺少隔离 DSN 时跳过，不连接任何业务库。
func TestDimUpsertStaleSourceRowDoesNotOverride(t *testing.T) {
	file := os.Getenv("UIP_BI_DIM_SYNC_TEST_DSN_FILE")
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
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("dedicated loopback data_analysis_test_*_test schema required")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dsn := cfg.FormatDSN()
	if _, err = migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated aggregation DB")
	}
	if pool, e := db.DB(); e == nil {
		defer pool.Close()
	}
	const tenant = "synthetic-dimsync-tenant1"
	contract := "synthetic-dimsync-contract1"
	cleanup := func() {
		if e := db.Exec("DELETE FROM dim_contract WHERE tenant_id = ?", tenant).Error; e != nil {
			t.Error(e)
		}
	}
	cleanup()
	defer cleanup()
	r := &Runner{aggDB: db, tenantID: tenant}
	newer := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	apply := func(at time.Time, amount int64, title string) error {
		return r.execAggregation(ctx, dimContractUpsert, "NORMAL", tenant, contract, "N-1", title, "test", "test",
			"active", nil, nil, nil, nil, amount, "CNY", "u", "o", "", "", at)
	}
	if err := apply(newer, 200, "newer"); err != nil {
		t.Fatal(err)
	}
	// 较旧来源行（迟到读取）不得覆盖较新镜像。
	if err := apply(older, 100, "stale"); err != nil {
		t.Fatal(err)
	}
	var row struct {
		AmountMinor int64
		Title       string
		UpdatedAt   time.Time
	}
	if err := db.Table("dim_contract").Select("amount_minor, title, updated_at").
		Where("tenant_id = ? AND contract_id = ?", tenant, contract).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AmountMinor != 200 || row.Title != "newer" || !row.UpdatedAt.Equal(newer) {
		t.Fatalf("旧来源覆盖了新镜像 %+v", row)
	}
	// 新来源行必须正常覆盖。
	if err := apply(newer.Add(time.Hour), 300, "latest"); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("dim_contract").Select("amount_minor, title, updated_at").
		Where("tenant_id = ? AND contract_id = ?", tenant, contract).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AmountMinor != 300 || row.Title != "latest" || !row.UpdatedAt.Equal(newer.Add(time.Hour)) {
		t.Fatalf("新来源未覆盖 %+v", row)
	}
	// 相同时间戳的重放保持幂等。
	if err := apply(newer.Add(time.Hour), 300, "latest"); err != nil {
		t.Fatal(err)
	}
}
