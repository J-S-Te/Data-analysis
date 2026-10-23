package oidc

import (
	"sort"
	"strings"
)

const (
	ScopeTenant = "TENANT"
	ScopeOrg    = "ORG"
	ScopeTeam   = "TEAM"
	ScopeNone   = "NONE"
)

// principalScope 将平台数据范围转换为看板边界。
// 未知或空范围集绝不默认成 TENANT，只有显式
// TENANT 绑定才可授予租户全范围读取。
func principalScope(scopes []DataScope, roles []string) (string, []string, []string) {
	hasSalesDirector, hasTechDirector := false, false
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
		if scopeType == ScopeTenant {
			return ScopeTenant, nil, nil
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
