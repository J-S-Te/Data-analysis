package oidc

import (
	"sort"
	"strings"
)

const (
	ScopeTenant      = "TENANT"
	ScopeApplication = "APPLICATION"
	ScopeOrg         = "ORG"
	ScopeTeam        = "TEAM"
	ScopeNone        = "NONE"
	ScopeSelf        = "SELF"
)

// principalScope 将已校验的平台授权范围转换为看板边界。
// 参数：scopes 为平台范围，roles 为已验证角色；SELF用户取后续可信主体ID。
// 返回值：明确范围模式和组织/团队集合；缺失或未知范围不授予TENANT。
func principalScope(scopes []DataScope, roles []string) (string, []string, []string) {
	hasSalesDirector, hasTechDirector := false, false
	hasSelf := false
	for _, role := range roles {
		switch strings.TrimSpace(role) {
		case "sales_director":
			hasSalesDirector = true
		case "tech_director":
			hasTechDirector = true
		}
	}
	orgSet, teamSet := map[string]struct{}{}, map[string]struct{}{}
	for _, scope := range scopes {
		scopeType := strings.ToUpper(strings.TrimSpace(scope.ScopeType))
		scopeID := strings.TrimSpace(scope.ScopeID)
		// 平台应用角色以 APPLICATION 表示“整个应用/租户范围”。看板把
		// APPLICATION 与显式 TENANT 都视为全租户边界；ORG/TEAM 仍按明细收窄。
		if scopeType == ScopeTenant || scopeType == ScopeApplication {
			return ScopeTenant, nil, nil
		}
		if scopeType == ScopeSelf {
			hasSelf = true
			continue
		}
		if scopeID == "" {
			continue
		}
		switch scopeType {
		case ScopeOrg, "ORG_UNIT":
			if hasTechDirector && !hasSalesDirector && strings.TrimSpace(scope.RoleCode) == "tech_director" {
				teamSet[scopeID] = struct{}{}
			} else {
				orgSet[scopeID] = struct{}{}
			}
		case ScopeTeam:
			teamSet[scopeID] = struct{}{}
		}
	}
	orgs := scopeSortedKeys(orgSet)
	teams := scopeSortedKeys(teamSet)
	switch {
	case len(orgs) > 0:
		return ScopeOrg, orgs, teams
	case len(teams) > 0:
		return ScopeTeam, nil, teams
	case hasSelf:
		return ScopeSelf, nil, nil
	default:
		return ScopeNone, nil, nil
	}
}

func scopeSortedKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
