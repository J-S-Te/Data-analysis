# 合同导入报表状态机器端点（BIZ-XAPI-13）

## 权威路径

```
GET /internal/v1/contract-imports/report-status
```

该路径**不随** `APP_PATH_PREFIX`（默认 `/data_analysis`）变化，也不存在
`<APP_PATH_PREFIX>/internal/v1/contract-imports/report-status` 的第二挂载。

## 合同调用方

`contract_management` 的 `executeImportReport` 固定请求
`/internal/v1/contract-imports/report-status?...`，其
`CONTRACT_IMPORT_REPORT_BASE_URL` 按“服务 origin”配置（例如
`http://data-analysis:8080`），而不是带前缀的站点根。因此：

- 只保留根路径挂载才能与真实调用契约一致；
- 同一端点出现两个路径会让“哪个路径受组级访问控制/审计规则约束”变得不确定；
- 运维排查时也不用再猜合同侧是否配置了前缀。

### BASE_URL 必须是纯 origin（带路径启动失败）

合同侧 `internal/config` 对 `CONTRACT_IMPORT_REPORT_BASE_URL`（以及
`CONTRACT_IMPORT_PROJECT_BASE_URL`/`CONTRACT_IMPORT_FINANCE_BASE_URL`）做 origin 校验：
**带路径的配置直接让 `contract_management` 启动失败**（错误只含配置项名称，不回显配置值）。
例如 `http://data-analysis:8080/internal/v1/contract-imports/report-status` 会被拒绝——
调用方按 `origin + 绝对契约路径` 拼请求，带路径的 origin 会把契约路径挪到前缀之后，
实际调用的端点与配置语义不一致。允许末尾 `/`，`*_TOKEN_URL` 保留自身路径不受该约束。


## 实现

`internal/bootstrap/app.go` 的 `mountInternalReportStatus` 是唯一挂载点，
`InternalReportStatusPath` 是唯一定义；`App.New` 只调用一次。
`internal/bootstrap/report_status_route_test.go` 断言路由表只有这一条，
且带 `/data_analysis` 前缀的路径返回 404。

## 验签 JSON 严格性

`internal/importreport/verifier.go` 对 JWT header 与 payload 使用严格对象解码：

- 拒绝未知字段（白名单为平台应用令牌实际签发的字段）；
- 拒绝重复键（避免“最后一个键生效”的解析差异）；
- 拒绝 `null` 值；
- 拒绝对象之后的任何尾随 JSON；
- 拒绝无效 UTF-8；
- 之后才按目标类型解码并执行 issuer/audience/租户/应用/环境/用途/scope/时间窗口校验。

`internal/importreport/verifier_test.go` 覆盖重复键、未知字段、尾随内容、
`null`、类型错配与无效 UTF-8 负例。

## 机器信任绑定（OAuth 客户端 + 应用 + 环境）

`internal/bootstrap/reportStatusVerifier` 只在绑定完整时构造验证器，**绑定不完整就不挂载端点**
（fail-closed：宁可不出这个入口，也不挂一个无法证明“是哪个客户端调的”的匿名读取口）。

| 配置项 | 来源 | 缺失后果 |
| --- | --- | --- |
| `CONTRACT_IMPORT_STATUS_CLIENT_ID` | 平台令牌 `sub` | 为空 → **端点不挂载**（返回 404） |
| `REPORT_STATUS_MACHINE_OAUTH_CLIENT_ID` | 平台令牌 `oauth_client_id` | 为空 → **端点不挂载**（返回 404） |
| `CONTRACT_IMPORT_STATUS_ISSUER` / `_AUDIENCE` / `_PUBLIC_KEY_PATH` / `_TENANT_ID` / `_CALLER_ENVIRONMENT_CODE` | 验签与调用方环境绑定 | 为空 → `NewVerifier` 报 `machine trust binding incomplete`，**启动失败** |
| `application_id`（令牌声明） | 调用方应用 | **必填**：为空即 `INVALID_REPORT_STATUS_MACHINE`（401） |
| `environment_id`（令牌声明） | 调用方环境 | **必填**：为空即 401 |

- 应用/环境绑定没有“关闭开关”：`RequireApplicationID`/`RequireEnvironmentID` 在配置里固定为
  `true`，构造期即校验，传 `false` 会被拒绝。
- 令牌还必须满足：`application_code='contract_management'`、
  `environment_code=CONTRACT_IMPORT_STATUS_CALLER_ENVIRONMENT_CODE`、`token_use='application'`、
  `scope` 恰好为 `["analysis.contract-import.report-status"]`、`tenant_id` 与信任租户一致，
  且 `nbf/iat/exp` 时间窗口合法；任一不满足都是 401，不泄露具体原因。
- 缺绑定时请检查 `compose.yaml`（`dashboard-api` 的 `REPORT_STATUS_MACHINE_OAUTH_CLIENT_ID`）
  与 `.env.example` 是否有值；租户与 `OIDC_TENANT_ID` 不一致会让进程启动失败
  （`report-status trusted tenant mismatch`）。

## 读取审计（request_id + payload 三元组）

成功读取在**同一事务**内写一条 `bi_audit_outbox`（`auditoutbox.Append`）：

| 列 | 值 |
| --- | --- |
| `tenant_id` | **验签得到的**租户（请求里的 `tenant_id` 参数被忽略） |
| `action` / `resource_type` / `resource_id` | `DATA_ANALYSIS:REPORT_STATUS_READ` / `contract_import_item` / `item_id` |
| `request_id` | 中间件注入的请求追踪号（无则 NULL），可与平台侧审计对齐 |
| `payload` | 精确版本三元组 JSON：`{"source_revision":…,"report_release_version":…,"state":…}` |

- `request_id`/`payload` 是迁移 `migrations/000028_bi_audit_outbox_request_payload.sql` 新增的可空列。
  未执行该迁移的库上：读取侧（`Claim` 用 `SELECT *`）会把两列静默读成零值，请求追踪号与本地
  证据永远读不回来；写入侧按列名 INSERT 则会报 `Unknown column`（MySQL 1054），把整条业务事务
  一起打回。务必与代码同批发布（`auditoutbox.Append` 在值为空时省略这两列，因此滚动窗口里
  只丢追踪号，不会阻断业务写入）。
- 审计**绝不**写令牌、授权明细或查询串原文：`payload` 只有三个可判定字段，值来自已解析的
  精确版本与投影状态，不复制原始 query string。
- 写入失败的处理保持既有语义：**业务只读证明不受影响**——事务回滚（不留下“读了却没有审计”的
  记录），响应仍是 200，并写一条不含令牌/查询串的 WARN
  （`report status read audit unavailable; response unaffected`）。未命中的精确版本（404）
  不产生审计：没有成功读取就没有读取事件。
- `payload` 只作本地审计证据，**不转发**给平台审计接口（`platformaudit.Event` 没有负载字段）。
