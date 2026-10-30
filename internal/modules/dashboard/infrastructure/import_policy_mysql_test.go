package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestImportedContractExcludedAcrossOrdinaryScopes 验证真实SQL在普通范围排除历史和未知来源。
// 参数：t 为测试上下文，只接受显式隔离的本任务schema。
// 返回值：无，旧普通合同保留默认NORMAL且所有普通范围使用相同过滤。
func TestImportedContractExcludedAcrossOrdinaryScopes(t *testing.T) {
	file := os.Getenv("UIP_IMPORT_REPORT_TEST_DSN_FILE")
	if file == "" {
		t.Skip("explicit isolated DSN required")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("read private DSN")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || !strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("dedicated loopback test schema required")
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated DB")
	}
	pool, _ := db.DB()
	defer pool.Close()
	tenant := "ordinary-import-qa"
	defer db.Exec("DELETE FROM dim_contract WHERE tenant_id=?", tenant)
	db.Exec("DELETE FROM dim_contract WHERE tenant_id=?", tenant)
	for _, origin := range []string{"NORMAL", "IMPORTED", "UNKNOWN", "normal"} {
		if err = db.Exec(`INSERT INTO dim_contract(tenant_id,contract_id,title,contract_type,service_type,status,start_date,owner_user_id,owner_org_id,amount_minor,currency,updated_at,origin_kind) VALUES (?,?,'synthetic','test','test','active','2020-01-31','u','o',100,'CNY',?,?)`, tenant, origin, time.Now().UTC(), origin).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := NewGORMRepository(db)
	for _, mode := range []string{"TENANT", "ORG", "TEAM", "SELF"} {
		ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "u", ScopeMode: mode, OrganizationIDs: []string{"o"}})
		rows, count, err := repo.ListContracts(ctx, tenant, 1, 20)
		if err != nil || count != 1 || len(rows) != 1 {
			t.Fatalf("scope=%s count=%d rows=%d err=%v", mode, count, len(rows), err)
		}
	}
}
