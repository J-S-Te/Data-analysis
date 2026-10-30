package oidc

import "testing"

func TestPrincipalScopeRequiresExplicitTenantBinding(t *testing.T) {
	if mode, _, _ := principalScope(nil, []string{"sales_director"}); mode != ScopeNone {
		t.Fatalf("empty scopes mode=%s, want NONE", mode)
	}
	if mode, orgs, _ := principalScope([]DataScope{{RoleCode: "sales_director", ScopeType: "ORG", ScopeID: "org-1"}, {RoleCode: "sales_director", ScopeType: "ORG", ScopeID: "org-2"}}, []string{"sales_director"}); mode != ScopeOrg || len(orgs) != 2 || orgs[0] != "org-1" || orgs[1] != "org-2" {
		t.Fatalf("org scope=%s/%v", mode, orgs)
	}
	if mode, _, teams := principalScope([]DataScope{{RoleCode: "tech_director", ScopeType: "ORG", ScopeID: "team-1"}}, []string{"tech_director"}); mode != ScopeTeam || len(teams) != 1 || teams[0] != "team-1" {
		t.Fatalf("team scope=%s/%v", mode, teams)
	}
	if mode, _, _ := principalScope([]DataScope{{RoleCode: "boss", ScopeType: "TENANT"}}, []string{"boss"}); mode != ScopeTenant {
		t.Fatalf("tenant mode=%s", mode)
	}
	if mode, _, _ := principalScope([]DataScope{{RoleCode: "boss", ScopeType: "APPLICATION"}}, []string{"boss"}); mode != ScopeTenant {
		t.Fatalf("application role must be tenant-wide, mode=%s", mode)
	}
}

// TestPrincipalScopeExplicitSelf 使用真实平台授权范围映射，禁止请求指定用户。
// 参数：t 为测试上下文。
// 返回值：无，SELF保留为个人边界，未知或缺失范围保持NONE。
func TestPrincipalScopeExplicitSelf(t *testing.T) {
	mode, orgs, teams := principalScope([]DataScope{{ScopeType: "SELF"}}, nil)
	if mode != "SELF" || len(orgs) != 0 || len(teams) != 0 {
		t.Fatalf("actual SELF mapping: %s %v %v", mode, orgs, teams)
	}
	if mode, _, _ := principalScope([]DataScope{{ScopeType: "mystery"}}, nil); mode != ScopeNone {
		t.Fatal("unknown became tenant")
	}
}
