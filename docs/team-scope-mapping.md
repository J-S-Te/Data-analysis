# 看板 TEAM 范围到数据列的映射（BIZ-DATA-09）

## 结论

当前数据模型下，`TEAM` 与 `ORG` 都映射到 **`owner_org_id`**，不是
`dim_project.team`。原因不是猜测，而是标识命名空间可由代码追踪：

1. `internal/oidc/scope.go` 只把平台数据范围归为 `TENANT/ORG/TEAM/SELF/NONE`；
   其中 `TEAM` 条目来自两类来源：
   - 平台显式 `TEAM` scope（当前平台未签发）；
   - `tech_director` 且无 `sales_director` 时的 **`ORG` scope**（`ScopeID` 进入 `TeamIDs`）。
2. 平台侧 `ORG` scope 的 `ScopeID` 是组织单元 ID；合同管理系统
   （`contract_management/internal/infrastructure/mysql/repository.go`）与项目管理系统
   （`project_management/internal/infrastructure/mysql/repository.go`）都把
   `ORG` scope 的 `ScopeID` 直接与 `owner_org_id` 比较。
3. 因此 data-analysis 的 `TeamIDs` 在现行平台里仍是**组织单元 ID**，
   与 `owner_org_id` 同命名空间；`dim_project.team` 只是“团队名快照”
   （`migrations/000001_init_aggregation_schema.sql` 注释），不是授权键。

## 实现口径

统一实现在 `internal/shared/scope.OwnerOrgUnitIDs(mode, principal)`（`internal/shared/scope/scope.go`）。
看板、告警、导入历史三处读取入口都调用它，不再各自维护一份并集逻辑
（此前的 `internal/modules/dashboard/infrastructure/scope.go::modeOwnerOrgUnitIDs` 已改为委托）：

- `ORG`：取 `OrganizationIDs ∪ TeamIDs`（同一用户可能同时持有组织与团队角色，
  各范围都是已授予的数据边界，不能丢）；
- `TEAM`：只取 `TeamIDs`，不允许用 `OrganizationIDs` 冒充团队授权；
- `TENANT/SELF/NONE/未知`：不产生组织单元集合；查询侧按既有语义处理
  （SELF 走 `owner_user_id`，其余失败关闭）。

**命名空间断言（fail-closed）**：`TEAM` 模式若同时携带 `OrganizationIDs`，说明无法再判定
`TeamIDs` 是否仍是组织单元 ID（平台未来可能签发独立 `TEAM` 实体），此时返回
`ErrTeamNamespaceOverlap` 与空集合，调用方按"范围不可用/恒空"处理，并写一条只含模式与
集合大小的告警（`WarnNamespaceViolation`，不含 ID/令牌/查询串）。绝不猜测方向、绝不退化为全租户。

调用点：`internal/modules/dashboard/infrastructure/scope.go`、
`internal/modules/alerts/interfaces/http/handler.go`、`internal/importreport/http.go`
（`historyScopeQuery`）。`applyOrgScope` 只接受"归属组织单元"列（当前均为 `owner_org_id`）。
`dim_project.team` 是展示名称，任何模式都不得用它做权限过滤。

## 行为与测试

`internal/shared/scope/scope_test.go`：`ORG`=组织∪团队（去重排序）、`TEAM`=仅团队、
其它模式恒空、`TEAM` 携带组织授权时 fail-closed。

`internal/modules/dashboard/infrastructure/scope_team_test.go`：

- `ORG` 视角可见已授予的组织与团队行，范围外组织（`org-c`）不可见；
- `TEAM` 视角只匹配 `TeamIDs`，即使主体同时带 `OrganizationIDs` 也不会越权
  （该组合现在直接失败关闭，返回 `ErrTeamNamespaceOverlap`）；
- `TEAM` 没有 `TeamIDs`、团队 ID 未命中、`NONE`、未知模式均为恒空
  （项目摘要返回"不可用"，合同返回 `available=false`）；
- 其它租户的同名团队行始终不可见。

## 残余风险 / 产品缺口

- **Metabase 嵌入（`internal/embedbridge`）仍是并集/回退口径**：`embedScope` 在
  `ORG` 时下发 `ORG_SUBTREE` + `org_unit_ids`（并带 `team_ids`），在 `TEAM` 时
  `TeamIDs` 为空会**回退到 `OrganizationIDs`**。因此嵌入图表与看板/告警/导入历史
  （已统一为 `internal/shared/scope.OwnerOrgUnitIDs`）口径不完全一致，属**已知残留**。
  统一它是独立改动：需要 Metabase 侧确认 `TEAM` 视图只用团队维度，并移除该回退。
- **没有组织层级展开**：平台 `ORG`/团队范围按精确组织单元 ID 比较。若
  `tech_director` 的授权单元是父部门，而项目 `owner_org_id` 是子团队单元，
  结果会为空。要支持"管辖团队含下级"，需要同步平台组织树并做后代展开，
  当前 data-analysis 没有该目录数据。
- **平台未来若签发独立 `TEAM` 实体 ID**，`TeamIDs` 将不再与组织单元同命名空间。
  届时必须在此显式失败关闭或改为团队→组织映射表，不能继续与 `owner_org_id` 比较。
  该映射现已集中在一处（`internal/shared/scope`），改动不需要再逐个页面追。
- **`TEAM` 恒空的可用性代价**：主体同时持有组织与团队授权时，`TEAM` 页面恒空而不是
  看到并集，属于安全优先的取舍；若产品要求"团队视角看并集"，应显式改为 `ORG` 模式
  授权，而不是放宽映射。
