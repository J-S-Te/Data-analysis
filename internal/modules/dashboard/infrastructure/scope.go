package infrastructure

import (
	"context"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	sharedscope "github.com/unified-identity-auth-platform/data-analysis/internal/shared/scope"
)

type queryScope struct {
	mode     string
	tenantID string
	userID   string
	// orgUnitIDs 是当前模式允许用于 owner_org_id 过滤的组织单元 ID。
	// ORG 视角取平台 ORG 范围与团队范围的并集（同一用户可能同时持有
	// 多个角色），TEAM 视角只取 TeamIDs，避免把其它范围当成团队授权。
	// 两者在平台上都来自 ORG 类型的数据范围绑定（见 internal/oidc/scope.go：
	// tech_director 的 ORG 范围归入 TeamIDs），属于同一组织单元命名空间。
	// dim_project.team 是团队名称快照，不是授权键，任何模式都不得用它过滤。
	orgUnitIDs []string
}

// scopeFor 从可信会话主体读取查询范围，不接受请求声明的用户ID。
// 参数：ctx 为主体上下文，tenantID 为上层授权租户。
// 返回值：模式、组织/团队及个人ID；真实会话未知模式在查询时拒绝。
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
	ids, err := modeOwnerOrgUnitIDs(mode, principal)
	if err != nil {
		// 命名空间断言失败（TEAM 模式携带组织单元授权）：保留模式但清空集合，
		// 于是所有范围分支都按“范围不可用”处理（available=false / 恒空），
		// 不会退化成全租户，也不猜测哪个集合才是团队授权。
		sharedscope.WarnNamespaceViolation(mode, len(principal.OrganizationIDs), len(principal.TeamIDs))
		return queryScope{mode: mode, tenantID: tenantID, userID: principal.UserID}
	}
	return queryScope{mode: mode, tenantID: tenantID, orgUnitIDs: ids, userID: principal.UserID}
}

// modeOwnerOrgUnitIDs 返回主体在指定模式下的可信组织单元 ID 集合。
// 参数：mode 为已归一化的范围模式，principal 为已验签的可信主体。
// 返回值：去重且排序的 ID；非 ORG/TEAM 模式返回 nil（查询侧失败关闭）；
// TEAM 携带组织单元授权时返回 scope.ErrTeamNamespaceOverlap。
//
// 映射本身集中在 internal/shared/scope，看板不再单独维护一份并集逻辑：
// 同一会话在不同页面看到不同数据范围就是越权风险。
func modeOwnerOrgUnitIDs(mode string, principal auth.Principal) ([]string, error) {
	return sharedscope.OwnerOrgUnitIDs(mode, principal)
}

// hasScopedIDs 判定当前模式是否带有可用于 owner_org_id 的组织单元集合。
func (scope queryScope) hasScopedIDs() bool { return len(scope.orgUnitIDs) > 0 }

// contractRowsVisible 判定合同类“范围分支”查询是否已获得可执行的数据边界。
// 全租户分支由 LatestContract 的快照查询处理，不进入本判定；
// SELF 只按 owner_user_id 收窄；未知模式与空 ORG/TEAM 集合失败关闭。
func (scope queryScope) contractRowsVisible() bool {
	return scope.hasScopedIDs() || (scope.mode == "SELF" && scope.userID != "")
}

// projectRowsVisible 判定项目类“范围分支”查询是否已获得可执行的数据边界。
// 全租户分支由 LatestProject 的快照查询处理；项目摘要与趋势暂不支持个人范围，
// 未知模式与空集合必须返回不可用而不是全区数据。
func (scope queryScope) projectRowsVisible() bool {
	return (scope.mode == "ORG" || scope.mode == "TEAM") && scope.hasScopedIDs()
}
