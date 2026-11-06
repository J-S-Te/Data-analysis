package migration

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// TestEmbeddedMigrationAddsProjectReportStage 验证报告阶段增量迁移的编号、加列语义
// 与安全边界：只加列/索引、历史行保持未采集、任何列更新与破坏性语句都不允许。
func TestEmbeddedMigrationAddsProjectReportStage(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	const name = "project_report_stage"
	var (
		reportStage   string
		foundVersion  uint64
		previousItems []Item
	)
	for _, item := range items {
		if item.Name == name {
			reportStage, foundVersion = item.SQL, item.Version
			break
		}
		previousItems = append(previousItems, item)
	}
	if reportStage == "" {
		t.Fatalf("embedded migrations do not contain a %s migration", name)
	}
	// 版本必须紧接在既有历史（000033）之后；Load 已校验 000001..N 连续。
	if foundVersion != 34 {
		t.Fatalf("migration version = %d, want 34", foundVersion)
	}
	if len(previousItems) != 33 || previousItems[len(previousItems)-1].Name != "standard_finance_facts" {
		t.Fatalf("report stage 迁移没有接在既有历史之后：%d 个前置迁移", len(previousItems))
	}

	for _, fragment := range []string{
		// 快照投影与维表同名列，默认空串表示未采集/不可判定。
		"ALTER TABLE project_projection_stage ADD COLUMN report_stage VARCHAR(32) NOT NULL DEFAULT ''",
		"ALTER TABLE dim_project ADD COLUMN report_stage VARCHAR(32) NOT NULL DEFAULT ''",
		"CREATE INDEX idx_dim_project_report_stage ON dim_project (tenant_id, report_stage)",
		// 租户看板持久快照的可选计数：NULL=未采集，不能被回填成 0。
		"ALTER TABLE api_project_dashboard ADD COLUMN report_stage_counts_json JSON NULL",
		// 可重入守卫：列/索引存在时不再 ALTER，DDL 中断后重跑安全。
		"information_schema.columns",
		"information_schema.statistics",
		"table_name='project_projection_stage'",
		"table_name='dim_project'",
		"table_name='api_project_dashboard'",
		"column_name='report_stage'",
		"column_name='report_stage_counts_json'",
		"index_name='idx_dim_project_report_stage'",
		"PREPARE project_projection_stage_report_stage_stmt FROM @project_projection_stage_report_stage_ddl",
		"PREPARE dim_project_report_stage_stmt FROM @dim_project_report_stage_ddl",
		"PREPARE dim_project_report_stage_index_stmt FROM @dim_project_report_stage_index_ddl",
		"PREPARE api_project_dashboard_report_stage_counts_stmt FROM @api_project_dashboard_report_stage_counts_ddl",
		"DEALLOCATE PREPARE project_projection_stage_report_stage_stmt",
		"DEALLOCATE PREPARE dim_project_report_stage_stmt",
		"DEALLOCATE PREPARE dim_project_report_stage_index_stmt",
		"DEALLOCATE PREPARE api_project_dashboard_report_stage_counts_stmt",
	} {
		if !strings.Contains(reportStage, fragment) {
			t.Errorf("report stage migration missing %q", fragment)
		}
	}

	// 破坏性语句与历史回填一条都不允许（回滚 DDL 只作为注释块提供）。
	// 同时确认可执行语句不触碰快照 schema_version（该版本保持 1）。
	for _, line := range strings.Split(reportStage, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(strings.ToLower(trimmed), "schema_version") {
			t.Errorf("report stage migration executes schema_version change: %s", trimmed)
		}
		for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE", "DELETE FROM", "UPDATE DIM_PROJECT", "UPDATE PROJECT_PROJECTION_STAGE", "UPDATE API_PROJECT_DASHBOARD"} {
			if strings.Contains(strings.ToUpper(trimmed), forbidden) {
				t.Errorf("report stage migration executes destructive/backfill statement %q: %s", forbidden, trimmed)
			}
		}
	}

	// 对称人工回滚 DDL：注释块给出逆序删列，供受控回滚使用。
	for _, fragment := range []string{
		"-- ALTER TABLE api_project_dashboard DROP COLUMN report_stage_counts_json",
		"-- DROP INDEX idx_dim_project_report_stage ON dim_project",
		"-- ALTER TABLE dim_project DROP COLUMN report_stage",
		"-- ALTER TABLE project_projection_stage DROP COLUMN report_stage",
	} {
		if !strings.Contains(reportStage, fragment) {
			t.Errorf("report stage migration missing rollback hint %q", fragment)
		}
	}
}
