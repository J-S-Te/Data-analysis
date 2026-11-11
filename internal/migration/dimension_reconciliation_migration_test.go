package migration

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// TestDimensionReconciliationMigrationKeepsTenantAndRunIsolation 验证暂存迁移的隔离键及有界回收索引。
// 参数：t 为测试上下文。
// 返回值：无；迁移缺少租户/轮次/复合键、版本不连续或包含破坏性语句时失败。
func TestDimensionReconciliationMigrationKeepsTenantAndRunIsolation(t *testing.T) {
	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	body, version := embeddedMigrationByName(t, items, "dimension_sync_keys")
	if version != 36 {
		t.Fatalf("version=%d", version)
	}
	for _, fragment := range []string{"CREATE TABLE IF NOT EXISTS dimension_sync_key", "tenant_id VARCHAR(64)", "run_id CHAR(32)", "parent_key VARCHAR(64)", "UNIQUE KEY uk_dimension_sync_key (tenant_id, run_id, dimension, source_key, parent_key)", "KEY idx_dimension_sync_key_gc (tenant_id, dimension, created_at, id)"} {
		if !strings.Contains(body, fragment) {
			t.Errorf("migration missing %q", fragment)
		}
	}
	assertNoDestructiveStatements(t, "dimension_sync_keys", body)
}
