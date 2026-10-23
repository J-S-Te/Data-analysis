package embedbridge

import (
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func TestEmbedScopeNeverDefaultsToTenant(t *testing.T) {
	if _, err := embedScope(auth.Principal{TenantID: "tenant-1", ScopeMode: "NONE"}); err == nil {
		t.Fatal("NONE scope must not produce a tenant-wide embed")
	}
	scope, err := embedScope(auth.Principal{TenantID: "tenant-1", ScopeMode: "ORG", OrganizationIDs: []string{"org-a", "org-b"}})
	if err != nil || scope["scope_mode"] != "ORG_SUBTREE" {
		t.Fatalf("scope=%v err=%v", scope, err)
	}
	ids, ok := scope["org_unit_ids"].([]string)
	if !ok || len(ids) != 2 || ids[0] != "org-a" || ids[1] != "org-b" {
		t.Fatalf("org ids=%#v", scope["org_unit_ids"])
	}
	team, err := embedScope(auth.Principal{TenantID: "tenant-1", ScopeMode: "TEAM", TeamIDs: []string{"team-a"}})
	if err != nil || team["scope_mode"] != "TEAM" {
		t.Fatalf("team scope=%v err=%v", team, err)
	}
}
