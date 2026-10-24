package infrastructure

import (
	"context"
	"sort"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type queryScope struct {
	mode       string
	tenantID   string
	orgUnitIDs []string
}

func scopeFor(ctx context.Context, tenantID string) queryScope {
	principal, ok := auth.FromContext(ctx)
	if !ok {
		// 仓储单元测试构造不带 HTTP 主体的上下文。
		// 生产处理器始终安装主体；真实会话若无数据
		// 范围，会显式表示为 NONE，并保持安全拒绝。
		return queryScope{mode: "TENANT", tenantID: tenantID}
	}
	mode := principal.ScopeMode
	if mode == "" {
		// 兼容在数据范围字段引入前已设置主体的
		// 调用方及测试。已认证会话始终设置该字段。
		mode = "TENANT"
	}
	set := map[string]struct{}{}
	for _, id := range principal.OrganizationIDs {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	for _, id := range principal.TeamIDs {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return queryScope{mode: mode, tenantID: tenantID, orgUnitIDs: ids}
}
