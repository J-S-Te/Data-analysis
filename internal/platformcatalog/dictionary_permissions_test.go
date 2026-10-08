package platformcatalog

import "testing"

func TestMetricDictionaryManagementPermissionIsExplicit(t *testing.T) {
	manifest := DataAnalysisManifest()
	if manifest.Version != "data-analysis-v2" {
		t.Fatalf("catalog version: %s", manifest.Version)
	}
	declared := false
	for _, permission := range manifest.Permissions {
		if permission.Code == "dictionary.manage" {
			declared = true
			if permission.Action != "manage" || permission.RiskLevel != "HIGH" {
				t.Fatal("dictionary management is not an explicit high-risk permission")
			}
		}
	}
	if !declared {
		t.Fatal("dictionary.manage not declared")
	}
	for _, role := range manifest.Roles {
		got := false
		for _, permission := range role.Permissions {
			if permission == "dictionary.manage" {
				got = true
			}
		}
		want := role.Code == "admin" || role.Code == "dashboard_admin"
		if got != want {
			t.Fatalf("role %s dictionary management=%v want %v", role.Code, got, want)
		}
	}
}
