package aggregation

import (
	"strings"
	"testing"
)

// 生产 MySQL 上 dim_contract 与 dim_opportunity 的排序规则不同，直接比较两列会报
// "Illegal mix of collations"；必须在比较两侧显式统一排序规则，且不能改库表默认值。
func TestConversionFactQueryCollation(t *testing.T) {
	query := conversionFactQuery("mysql")
	if !strings.Contains(query, "CONVERT(c.opportunity_id USING utf8mb4) COLLATE utf8mb4_unicode_ci = CONVERT(CAST(o.opportunity_id AS CHAR) USING utf8mb4) COLLATE utf8mb4_unicode_ci") {
		t.Fatalf("MySQL 下商机列比较缺少显式排序规则：\n%s", query)
	}
	if !strings.Contains(query, "CONVERT(c.contract_id USING utf8mb4) COLLATE utf8mb4_unicode_ci = CONVERT(o.contract_id USING utf8mb4) COLLATE utf8mb4_unicode_ci") {
		t.Fatalf("MySQL 下合同列比较缺少显式排序规则：\n%s", query)
	}
	if strings.Contains(query, "ALTER TABLE") {
		t.Fatal("不得通过修改库表排序规则规避")
	}
}

// SQLite 不支持 utf8mb4_unicode_ci：本地/单测方言必须原样比较。
func TestConversionFactQuerySQLiteDialect(t *testing.T) {
	query := conversionFactQuery("sqlite")
	if strings.Contains(query, "COLLATE utf8mb4_unicode_ci") {
		t.Fatalf("SQLite 方言不应包含 MySQL 排序规则：\n%s", query)
	}
	if !strings.Contains(query, "strftime('%Y-%m'") {
		t.Fatalf("SQLite 方言应使用 strftime：\n%s", query)
	}
}
