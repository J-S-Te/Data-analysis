package application

import "testing"

// TestGetReturnsPublishedReadOnlyCatalog 验证发布目录版本与指标数量契约。
// 参数：t为当前测试上下文。
// 返回值：无；发布日期、状态或指标数量不符合发布版本时失败。
func TestGetReturnsPublishedReadOnlyCatalog(t *testing.T) {
	catalog := NewCatalogService().Get()
	if catalog.Version != "2026-11-12" || catalog.Status != "BUILTIN_CATALOG" || len(catalog.Metrics) != 22 {
		t.Fatalf("unexpected catalog: version=%q status=%q metrics=%d", catalog.Version, catalog.Status, len(catalog.Metrics))
	}
}
