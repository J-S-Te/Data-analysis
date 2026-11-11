package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	sharedscope "github.com/unified-identity-auth-platform/data-analysis/internal/shared/scope"
)

// TestModeOwnerOrgUnitIDsSeparatesTeamFromOrganizationGrants 验证 ORG/TEAM 的 ID 来源边界：
// ORG 合并用户已获得的组织与团队范围（多角色并集），TEAM 只接受 TeamIDs，
// 未知模式和没有 ID 的模式一律返回空集合，查询侧据此失败关闭。
func TestModeOwnerOrgUnitIDsSeparatesTeamFromOrganizationGrants(t *testing.T) {
	principal := auth.Principal{
		OrganizationIDs: []string{"org-b", "org-a", "", "org-a"},
		TeamIDs:         []string{"team-b", "team-b", ""},
	}
	// TEAM 主体只带团队授权；同时带组织授权属于命名空间断言失败的场景（见下）。
	teamOnly := auth.Principal{TeamIDs: principal.TeamIDs}
	cases := []struct {
		mode      string
		principal auth.Principal
		want      []string
	}{
		{"ORG", principal, []string{"org-a", "org-b", "team-b"}},
		{"TEAM", teamOnly, []string{"team-b"}},
		{"TENANT", principal, nil},
		{"SELF", principal, nil},
		{"NONE", principal, nil},
		{"", principal, nil},
		{"UNKNOWN", principal, nil},
		{"tenant", principal, nil},
		{" TEAM ", principal, nil},
	}
	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			got, err := modeOwnerOrgUnitIDs(tc.mode, tc.principal)
			if err != nil {
				t.Fatalf("mode %q unexpected error %v", tc.mode, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("mode %q ids=%v, want %v", tc.mode, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("mode %q ids=%v, want %v", tc.mode, got, tc.want)
				}
			}
		})
	}
}

// TestModeOwnerOrgUnitIDsRejectsTeamWithOrganizationGrants 验证命名空间断言：
// TEAM 模式同时携带组织单元授权时，看板不得猜“哪个集合才是团队授权”，
// 而是返回错误且不产生任何可用集合。
func TestModeOwnerOrgUnitIDsRejectsTeamWithOrganizationGrants(t *testing.T) {
	principal := auth.Principal{OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}}
	ids, err := modeOwnerOrgUnitIDs("TEAM", principal)
	if !errors.Is(err, sharedscope.ErrTeamNamespaceOverlap) {
		t.Fatalf("err=%v, want namespace overlap", err)
	}
	if ids != nil {
		t.Fatalf("ids=%v, want nil", ids)
	}
}

// TestContractTeamScopeUsesOnlyTeamIDsAndFailsClosed 验证合同范围分支不会把组织 ID 当成团队授权，
// 也不会在团队 ID 未命中、为空或模式未知时返回数据；跨租户行始终不可见。
func TestContractTeamScopeUsesOnlyTeamIDsAndFailsClosed(t *testing.T) {
	db := scopedContractFixture(t)
	const tenant = "tenant-team-scope"
	insertScopedContract(t, db, "NORMAL", tenant, "c-org-a", "active", "", "u-org", "org-a")
	insertScopedContract(t, db, "NORMAL", tenant, "c-team-b", "active", "", "u-team", "team-b")
	insertScopedContract(t, db, "NORMAL", tenant, "c-org-c", "active", "", "u-other", "org-c")
	// 其它租户即使同名团队也不得出现在任何范围结果里。
	insertScopedContract(t, db, "NORMAL", "tenant-other", "c-leak", "active", "", "u-team", "team-b")
	repository := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))

	cases := []struct {
		name       string
		principal  auth.Principal
		wantCount  int64
		wantAmount int64
	}{
		{
			// 多角色并集：组织与团队范围都是已授予的数据边界。
			name:       "org unions granted team scope",
			principal:  auth.Principal{TenantID: tenant, UserID: "u-org", ScopeMode: "ORG", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}},
			wantCount:  2,
			wantAmount: 200,
		},
		{
			// TEAM 只认 TeamIDs：纯团队授权命中 team-b 一行。
			name:       "team uses team ids only",
			principal:  auth.Principal{TenantID: tenant, UserID: "u-team", ScopeMode: "TEAM", TeamIDs: []string{"team-b"}},
			wantCount:  1,
			wantAmount: 100,
		},
		{
			// 命名空间断言：TEAM 同时带组织授权时无法判定命名空间，整段范围不可用，
			// 既不放宽（借用组织授权），也不猜哪个集合才是团队授权。
			name:      "team with organization grants is unavailable",
			principal: auth.Principal{TenantID: tenant, UserID: "u-team", ScopeMode: "TEAM", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}},
		},
		{
			// 旧构造若只填 OrganizationIDs，TEAM 范围必须恒空而不是借用组织授权。
			name:      "team without team ids is empty",
			principal: auth.Principal{TenantID: tenant, UserID: "u-org", ScopeMode: "TEAM", OrganizationIDs: []string{"org-a"}},
		},
		{
			name:      "team with unknown id is empty",
			principal: auth.Principal{TenantID: tenant, UserID: "u-team", ScopeMode: "TEAM", TeamIDs: []string{"team-x"}},
		},
		{
			name:      "none is empty",
			principal: auth.Principal{TenantID: tenant, UserID: "u-team", ScopeMode: "NONE", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}},
		},
		{
			name:      "unknown mode is empty",
			principal: auth.Principal{TenantID: tenant, UserID: "u-team", ScopeMode: "TEAM ", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithPrincipal(context.Background(), tc.principal)
			snapshot, available, err := repository.LatestContract(ctx, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.TotalContracts != tc.wantCount || snapshot.TotalAmountMinor != tc.wantAmount {
				t.Fatalf("contract=%+v available=%v, want contracts=%d amount=%d", snapshot, available, tc.wantCount, tc.wantAmount)
			}
			wantAvailable := tc.wantCount > 0 || tc.name == "team with unknown id is empty"
			if available != wantAvailable {
				t.Fatalf("available=%v, want %v", available, wantAvailable)
			}
		})
	}
}

// TestProjectTeamScopeUsesOnlyTeamIDsAndFailsClosed 验证项目摘要同样按模式取 ID：
// ORG 并集可见组织与团队项目，TEAM 只匹配 TeamIDs，空/未知/未命中时返回不可用。
func TestProjectTeamScopeUsesOnlyTeamIDsAndFailsClosed(t *testing.T) {
	db := freshnessTestDB(t)
	const tenant = "tenant-team-project"
	repo := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
	publishTestProjection(t, db, tenant, 2)
	now := time.Now().UTC().Add(-2 * time.Hour)
	for _, row := range []struct {
		id, org string
	}{{"p-org-a", "org-a"}, {"p-team-b", "team-b"}, {"p-other", "org-c"}} {
		if err := db.Exec(`INSERT INTO dim_project (tenant_id,project_id,status,owner_org_id,snapshot_id,risk,in_flight,service_items,source_updated_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			tenant, row.id, "已完成", row.org, "snap", false, false, 1, now, now, now).Error; err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name      string
		principal auth.Principal
		wantCount int
		wantOK    bool
	}{
		{"org unions granted team scope", auth.Principal{TenantID: tenant, ScopeMode: "ORG", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}}, 2, true},
		{"team uses team ids only", auth.Principal{TenantID: tenant, ScopeMode: "TEAM", TeamIDs: []string{"team-b"}}, 1, true},
		// 命名空间断言失败：TEAM + 组织授权时项目摘要同样返回不可用。
		{"team with organization grants is unavailable", auth.Principal{TenantID: tenant, ScopeMode: "TEAM", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}}, 0, false},
		{"team without team ids is empty", auth.Principal{TenantID: tenant, ScopeMode: "TEAM", OrganizationIDs: []string{"org-a"}}, 0, false},
		// 已发布投影下未命中团队是“可信的空”，与“无范围/未发布”不同。
		{"team with unknown id is empty", auth.Principal{TenantID: tenant, ScopeMode: "TEAM", TeamIDs: []string{"team-x"}}, 0, true},
		{"none is empty", auth.Principal{TenantID: tenant, ScopeMode: "NONE", OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithPrincipal(context.Background(), tc.principal)
			got, ok, err := repo.LatestProject(ctx, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantOK || got.ProjectCount != tc.wantCount {
				t.Fatalf("project=%+v available=%v, want count=%d available=%v", got, ok, tc.wantCount, tc.wantOK)
			}
		})
	}
}
