package migration

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// TestEmbeddedMigrationAddsAuditOutboxRequestIDAndPayload 验证审计证据列迁移的顺序与安全边界。
// 参数：t为测试上下文，迁移需按列存在性重入并保留历史事件。
// 返回值：无；错版本、缺列、重写事件或缺少受控回滚提示使测试失败。
func TestEmbeddedMigrationAddsAuditOutboxRequestIDAndPayload(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	const name = "bi_audit_outbox_request_payload"
	var (
		auditOutbox   string
		foundVersion  uint64
		previousItems []Item
	)
	for _, item := range items {
		if item.Name == name {
			auditOutbox, foundVersion = item.SQL, item.Version
			break
		}
		previousItems = append(previousItems, item)
	}
	if auditOutbox == "" {
		t.Fatalf("embedded migrations do not contain a %s migration", name)
	}
	// 版本必须紧接在已有历史之后，保持 000001..N 连续（Load 已校验连续性，这里固定本迁移的序号）。
	if foundVersion != 29 {
		t.Fatalf("migration version = %d, want 29", foundVersion)
	}
	if len(previousItems) != 28 || previousItems[len(previousItems)-1].Name != "alert_lifecycle_phase" {
		t.Fatalf("audit outbox 迁移没有接在既有历史之后：%d 个前置迁移", len(previousItems))
	}

	for _, fragment := range []string{
		"ALTER TABLE bi_audit_outbox ADD COLUMN request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER resource_id",
		"ALTER TABLE bi_audit_outbox ADD COLUMN payload JSON NULL AFTER request_id",
		// 可重入守卫：只在 information_schema.columns 显示缺列时才 ALTER。
		"information_schema.columns",
		"table_name='bi_audit_outbox'",
		"column_name='request_id'",
		"column_name='payload'",
		"PREPARE audit_outbox_request_id_stmt FROM @audit_outbox_request_id_ddl",
		"PREPARE audit_outbox_payload_stmt FROM @audit_outbox_payload_ddl",
		"DEALLOCATE PREPARE audit_outbox_request_id_stmt",
		"DEALLOCATE PREPARE audit_outbox_payload_stmt",
	} {
		if !strings.Contains(auditOutbox, fragment) {
			t.Errorf("audit outbox migration missing %q", fragment)
		}
	}

	// 破坏性语句一条都不允许（回滚 DDL 只作为注释块提供）。
	for _, line := range strings.Split(auditOutbox, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE", "DELETE FROM", "UPDATE BI_AUDIT_OUTBOX"} {
			if strings.Contains(strings.ToUpper(trimmed), forbidden) {
				t.Errorf("audit outbox migration executes destructive statement %q: %s", forbidden, trimmed)
			}
		}
	}

	// 对称人工回滚 DDL：注释块里必须给出逆序删列，供受控回滚使用。
	for _, fragment := range []string{"-- ALTER TABLE bi_audit_outbox DROP COLUMN payload", "-- ALTER TABLE bi_audit_outbox DROP COLUMN request_id"} {
		if !strings.Contains(auditOutbox, fragment) {
			t.Errorf("audit outbox migration missing rollback hint %q", fragment)
		}
	}
}
