package application

import "testing"

func TestGetReturnsPublishedReadOnlyCatalog(t *testing.T) {
	catalog := NewCatalogService().Get()
	if catalog.Version != "2026-07-28" || catalog.Status != "ALL_CONFIRMED" || len(catalog.Metrics) != 22 {
		t.Fatalf("unexpected catalog: version=%q status=%q metrics=%d", catalog.Version, catalog.Status, len(catalog.Metrics))
	}
}
