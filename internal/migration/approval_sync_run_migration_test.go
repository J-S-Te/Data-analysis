package migration

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// TestEmbeddedMigrationsAddContractApprovalProjection 验证审批投影迁移的守卫列与安全边界。
// 参数：t为测试上下文，迁移顺序应衔接已发布中央会话历史。
// 返回值：无；缺表、缺守卫列、错版本或破坏性语句使测试失败。
func TestEmbeddedMigrationsAddContractApprovalProjection(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	migrationSQL, version := embeddedMigrationByName(t, items, "dim_contract_approval")
	if version != 30 {
		t.Fatalf("migration version = %d, want 30", version)
	}
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS dim_contract_approval",
		"tenant_id    CHAR(26)",
		"contract_id  CHAR(26)",
		"approval_id  CHAR(26)",
		"kind         VARCHAR(32) NOT NULL",
		"status       VARCHAR(32) NOT NULL",
		"started_at   DATETIME(3) NOT NULL",
		"completed_at DATETIME(3) NULL",
		"updated_at   DATETIME(3) NOT NULL",
		"PRIMARY KEY (tenant_id, contract_id, approval_id)",
		"KEY idx_dim_contract_approval_running (tenant_id, status, contract_id)",
	} {
		if !strings.Contains(migrationSQL, fragment) {
			t.Errorf("dim_contract_approval migration missing %q", fragment)
		}
	}
	// 关键设计点：审批投影独立成表，审批标志不得写进合同维表。
	// 只检查可执行语句：注释里出现的反例说明（“不得把 in_approval 塞进 dim_contract”）是设计依据。
	for _, line := range strings.Split(migrationSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		for _, forbidden := range []string{"ALTER TABLE dim_contract ", "ALTER TABLE dim_contract\n", "in_approval"} {
			if strings.Contains(trimmed, forbidden) {
				t.Errorf("approval projection must not modify dim_contract: found %q in %s", forbidden, trimmed)
			}
		}
	}
	assertNoDestructiveStatements(t, "dim_contract_approval", migrationSQL)
}

// TestEmbeddedMigrationsAddSyncRun 验证同步轮次迁移的索引与可空结束时间。
// 参数：t为测试上下文，迁移顺序应衔接已发布中央会话历史。
// 返回值：无；错版本、缺列、缺索引或破坏性语句使测试失败。
func TestEmbeddedMigrationsAddSyncRun(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	migrationSQL, version := embeddedMigrationByName(t, items, "sync_run")
	if version != 31 {
		t.Fatalf("migration version = %d, want 31", version)
	}
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS sync_run",
		"subsystem_code  VARCHAR(64) NOT NULL",
		"run_started_at  DATETIME(3) NOT NULL",
		"owner           VARCHAR(128) NOT NULL DEFAULT ''",
		"finished_at     DATETIME(3) NULL",
		"PRIMARY KEY (tenant_id, subsystem_code, run_started_at)",
		"KEY idx_sync_run_open (tenant_id, subsystem_code, finished_at, run_started_at)",
	} {
		if !strings.Contains(migrationSQL, fragment) {
			t.Errorf("sync_run migration missing %q", fragment)
		}
	}
	assertNoDestructiveStatements(t, "sync_run", migrationSQL)
}

// TestEmbeddedMigrationsAddSyncSourceLastSuccess 验证首次同步成功证据迁移的重入与安全边界。
// 参数：t为测试上下文，迁移顺序应衔接已发布中央会话历史。
// 返回值：无；错版本、缺守卫列或破坏性语句使测试失败。
func TestEmbeddedMigrationsAddSyncSourceLastSuccess(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	migrationSQL, version := embeddedMigrationByName(t, items, "sync_source_last_success")
	if version != 32 {
		t.Fatalf("migration version = %d, want 32", version)
	}
	for _, fragment := range []string{
		"information_schema.columns",
		"table_name='sync_source'",
		"column_name='last_success_at'",
		"ADD COLUMN last_success_at DATETIME(3) NULL AFTER last_status",
		"PREPARE sync_source_last_success_stmt FROM @sync_source_last_success_ddl",
		"DEALLOCATE PREPARE sync_source_last_success_stmt",
	} {
		if !strings.Contains(migrationSQL, fragment) {
			t.Errorf("sync_source last_success_at migration missing %q", fragment)
		}
	}
	assertNoDestructiveStatements(t, "sync_source_last_success", migrationSQL)
}

// embeddedMigrationByName 返回指定名称的迁移 SQL 与版本号。
// 参数：t 为测试上下文，items 为已加载迁移，name 为文件名主体。
// 返回值：迁移 SQL 与版本号；缺失时终止用例。
func embeddedMigrationByName(t *testing.T, items []Item, name string) (string, uint64) {
	t.Helper()
	for _, item := range items {
		if item.Name == name {
			return item.SQL, item.Version
		}
	}
	t.Fatalf("embedded migrations do not contain a %s migration", name)
	return "", 0
}

// assertNoDestructiveStatements 断言迁移正文没有破坏性语句（回滚 DDL 只允许出现在注释里）。
// 参数：t 为测试上下文，name 为迁移名，sql 为迁移正文。
// 返回值：无。
func assertNoDestructiveStatements(t *testing.T, name, sql string) {
	t.Helper()
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE", "DELETE FROM"} {
			if strings.Contains(strings.ToUpper(trimmed), forbidden) {
				t.Errorf("%s migration executes destructive statement %q: %s", name, forbidden, trimmed)
			}
		}
	}
}
