package scope

import (
	"errors"
	"reflect"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// TestOwnerOrgUnitIDsKeepsModeSemantics 固化统一映射的语义（BIZ-DATA-09）：
// ORG=组织∪团队（去重排序）、TEAM=仅团队、其它模式恒空。
func TestOwnerOrgUnitIDsKeepsModeSemantics(t *testing.T) {
	principal := auth.Principal{
		OrganizationIDs: []string{"org-b", "org-a", "", "org-a"},
		TeamIDs:         []string{"team-b", "team-b", ""},
	}
	// TEAM 视角的主体只带团队授权：同时带组织授权属于命名空间断言失败的场景，
	// 由 TestOwnerOrgUnitIDsRejectsTeamWithOrganizationGrants 覆盖。
	teamOnly := auth.Principal{TeamIDs: principal.TeamIDs}
	for _, tc := range []struct {
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
		// 模式大小写敏感：非规范拼写按未知模式失败关闭，不做“纠正性”放宽。
		{"tenant", principal, nil},
		{"org", principal, nil},
		{" TEAM ", principal, nil},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			got, err := OwnerOrgUnitIDs(tc.mode, tc.principal)
			if err != nil {
				t.Fatalf("mode %q unexpected error %v", tc.mode, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mode %q ids=%v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// TestOwnerOrgUnitIDsEmptyGrantsStayEmpty 验证空集合恒空：任何模式都不会因为
// 没有 ID 而返回“无过滤”信号。
func TestOwnerOrgUnitIDsEmptyGrantsStayEmpty(t *testing.T) {
	for _, mode := range []string{"ORG", "TEAM", "TENANT", "SELF", "NONE", ""} {
		ids, err := OwnerOrgUnitIDs(mode, auth.Principal{UserID: "u"})
		if err != nil {
			t.Fatalf("mode %q unexpected error %v", mode, err)
		}
		if len(ids) != 0 {
			t.Fatalf("mode %q ids=%v, want empty", mode, ids)
		}
	}
}

// TestOwnerOrgUnitIDsRejectsTeamWithOrganizationGrants 验证命名空间断言：
// TEAM 模式携带组织单元授权时返回错误且不返回任何 ID，任何一个方向都不放宽。
func TestOwnerOrgUnitIDsRejectsTeamWithOrganizationGrants(t *testing.T) {
	principal := auth.Principal{OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"team-b"}}
	ids, err := OwnerOrgUnitIDs("TEAM", principal)
	if !errors.Is(err, ErrTeamNamespaceOverlap) {
		t.Fatalf("err=%v, want ErrTeamNamespaceOverlap", err)
	}
	if ids != nil {
		t.Fatalf("ids=%v, want nil：断言失败不得返回任何集合", ids)
	}
	// 断言只针对 TEAM：ORG 并集与 SELF/TENANT 语义不受影响。
	if ids, err = OwnerOrgUnitIDs("ORG", principal); err != nil || len(ids) != 2 {
		t.Fatalf("ORG ids=%v err=%v", ids, err)
	}
}

// TestWarnNamespaceViolationDeduplicatesByShape 验证告警按 (mode, 集合大小) 去重，
// 且同形状只告警一次——避免每个请求刷屏，也不需要把 ID 写进日志。
func TestWarnNamespaceViolationDeduplicatesByShape(t *testing.T) {
	if !WarnNamespaceViolation("TEST-MODE-A", 1, 2) {
		t.Fatal("first warning for a shape must be emitted")
	}
	if WarnNamespaceViolation("TEST-MODE-A", 1, 2) {
		t.Fatal("repeated warning for the same shape must be suppressed")
	}
	if !WarnNamespaceViolation("TEST-MODE-A", 2, 2) {
		t.Fatal("different shape must be emitted")
	}
	if !WarnNamespaceViolation("TEST-MODE-B", 1, 2) {
		t.Fatal("different mode must be emitted")
	}
}
