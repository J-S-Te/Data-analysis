// Package scope 统一 data-analysis 各读取入口的数据范围映射（BIZ-DATA-09）。
//
// 映射依据：平台只签发 ORG 类型的数据范围（无独立 TEAM 实体），data-analysis 的
// TeamIDs 由 internal/oidc/scope.go 从 ORG 范围派生，因此团队 ID 与组织 ID 同为
// 平台组织单元 ID，可映射到数据列 owner_org_id。dim_project.team 只是团队名称
// 快照（见 migrations/000001_init_aggregation_schema.sql），不是授权键，任何模式
// 都不得用它过滤。
//
// 该映射必须只有一处实现：同一会话在不同页面看到不同数据范围本身就是越权风险，
// 因此看板、告警、导入历史都调用本包，而不是各自复制一份并集逻辑。
package scope

import (
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// ErrTeamNamespaceOverlap 表示主体同时携带 TEAM 范围模式与组织单元授权。
//
// 平台当前从 ORG 范围派生 TeamIDs，两者同属组织单元命名空间；一旦主体同时持有
// 两类授权，就无法再判断 TeamIDs 是否仍是组织单元 ID（平台未来可能签发独立
// TEAM 实体）。此时按失败关闭拒绝映射，而不是猜测某个方向。
var ErrTeamNamespaceOverlap = errors.New("scope: TEAM mode combined with organization grants")

// maxWarnedShapes 限制命名空间告警去重表的规模，避免异常主体制造无界状态。
const maxWarnedShapes = 64

var (
	warnMu       sync.Mutex
	warnedShapes = make(map[string]struct{}, maxWarnedShapes/2)
)

// OwnerOrgUnitIDs 把可信主体的范围模式映射为可用于“归属组织单元”列的 ID 集合。
// 参数：mode 为已归一化的范围模式（大小写敏感，非规范拼写按未知模式处理），
// principal 为已验签并通过授权复核的主体。
// 返回值：去重且已排序的 ID 集合，以及命名空间断言结果。
//
// 语义（与看板既有实现等价，三处调用点共用）：
//   - ORG：OrganizationIDs ∪ TeamIDs（同一用户可能同时持有组织与团队角色，
//     两类范围都是已授予的数据边界，不能丢）；
//   - TEAM：只取 TeamIDs，不允许用 OrganizationIDs 冒充团队授权；
//   - 其它模式（TENANT/SELF/NONE/未知/空串）：不产生组织单元集合。
//
// 空集合表示“没有可用范围”：调用方必须按不可用或恒空处理，绝不能退化为全租户。
// 命名空间断言失败时返回 nil 集合与 ErrTeamNamespaceOverlap，绝不返回任何 ID。
func OwnerOrgUnitIDs(mode string, principal auth.Principal) ([]string, error) {
	// 命名空间断言（fail-closed）：TEAM 模式不得携带组织单元授权。
	if mode == "TEAM" && len(principal.OrganizationIDs) > 0 {
		return nil, ErrTeamNamespaceOverlap
	}
	var values []string
	switch mode {
	case "ORG":
		values = append(values, principal.OrganizationIDs...)
		values = append(values, principal.TeamIDs...)
	case "TEAM":
		values = append(values, principal.TeamIDs...)
	default:
		return nil, nil
	}
	set := make(map[string]struct{}, len(values))
	for _, id := range values {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// WarnNamespaceViolation 记录一条命名空间断言告警。
// 参数：mode 为范围模式，organizationCount/teamCount 为对应集合大小。
// 返回值：本次调用是否真的写出告警（同一形状只会写一次）。
//
// 内容只含模式与集合大小，绝不写 ID、令牌或查询串原文：告警用于发现授权姿态
// 异常，不需要也不允许把授权明细复制到日志。相同 (mode, 集合大小) 形状只告警
// 一次，避免每次请求刷屏。
func WarnNamespaceViolation(mode string, organizationCount, teamCount int) bool {
	shape := mode + "/" + strconv.Itoa(organizationCount) + "/" + strconv.Itoa(teamCount)
	warnMu.Lock()
	if _, seen := warnedShapes[shape]; seen {
		warnMu.Unlock()
		return false
	}
	if len(warnedShapes) >= maxWarnedShapes {
		warnMu.Unlock()
		return false
	}
	warnedShapes[shape] = struct{}{}
	warnMu.Unlock()
	slog.Warn("scope namespace assertion failed; treating scope as unavailable",
		"mode", mode, "organization_ids", organizationCount, "team_ids", teamCount)
	return true
}
