# data_analysis · 数据看板与统计分析子系统

> 核对日期：2026-10-22。已贯通前端/API、权限/租户、聚合/预警/字典服务、存储迁移和装配；仅源码核对，未执行新的动态验收。操作见[数据看板与统计分析操作手册](../sharedDocs/10_操作手册/数据看板与统计分析操作手册.md)，历史纠正见[2026-10-13 台账](../.ai/modules/operation-docs-business-20261013.md)。 本轮静态记录见[2026-10-22 文档同步报告](../.ai/qa/docs-code-sync-20261022/agents/project-bi-settlement-report.json)。
>
> 当前生产边界：生产 Compose、Nginx 路由和子系统清单已具备，但真实生产接入仍依赖 `METABASE_EMBEDDING_SECRET`、数据库凭据、最小权限机器 Client、数据源配置和上线验收。不得将编排资产视为已投产。
> 权限基线：`authz/permission-manifest.yaml`（schema `authz.data-analysis/v1`、metadata version 2；schema 与目录版本不能混淆）
>
> **部署拓扑（团队约定）**：前端代码在统一仓库 `frontend/src/modules/data_analysis/`（随 `frontend` Docker 镜像构建，与其余子系统同一前端容器）；本目录仅存放**后端**（Go），`dashboard-api` 等各自独立 Docker（见 `compose.yaml`）。

## 版本与仓库归属

- 后端仓库：`data_analysis/` 独立 Git 仓库，与 `platform`、`contract_management` 等子系统保持相同的版本边界。
- Go module：`github.com/unified-identity-auth-platform/data-analysis`。
- 当前应用版本：根目录 `VERSION` 中的 `0.1.0`；发布标签建议使用 `v0.1.0` 格式。
- 当前迁移文件范围：`000001` 至 `000036_dimension_sync_keys`。数据库实际版本以 `da_schema_migration` 为准，不能仅根据文件名推断。
- 统一前端仍归属 `frontend` 仓库，不随本后端仓库单独发版。

## 目录结构

```text
data_analysis/
├── cmd/
│   ├── dashboard-api/          # 嵌入桥 + 业务 API（OIDC 会话 / embed / 预警中心 / 字典 / 管理）
│   ├── aggregation-worker/     # 跨库同步与预计算（队列任务与定时循环）
│   ├── alert-worker/           # 预警规则计算（聚合库上，幂等）
│   ├── authz-catalog/          # 授权目录发布（复用 CRM platformcatalog 模式）
│   ├── local-migrate/          # 本地迁移执行
│   ├── production-migrate/     # 生产迁移执行（受控）
│   └── sync-job-recover-legacy/ # 明确边界的旧同步任务预览/恢复维护命令
├── internal/
│   ├── bootstrap/              # 配置装配（对齐 CRM internal/bootstrap）
│   ├── middleware/             # 会话鉴权、请求 ID、时间与同源写校验
│   ├── shared/                 # auth / response / apperror
│   ├── platformcatalog/        # 目录发布（对齐 CRM internal/platformcatalog）
│   ├── auditoutbox/            # 事务审计持久化、租约与重试投递
│   ├── embedbridge/            # 嵌入桥：令牌签发 / 代理 / Metabase 客户端
│   ├── aggregation/            # 四类只读源、API 快照、分币种事实与租约队列
│   ├── alertworker/            # CONTRACT_EXPIRY 规则计算与幂等快照
│   └── modules/
│       ├── dashboard/          # 合同/项目/报告/财务原生摘要、下钻和月度趋势
│       ├── alerts/             # 预警中心 API 与可替换 Store
│       ├── dictionary/         # 指标字典 API
│       └── admin/              # 预警规则配置 / 数据源状态
├── authz/permission-manifest.yaml
├── migrations/                 # 聚合库版本化 SQL（当前 000001 至 000020）
├── Dockerfile                  # 单 Dockerfile 多二进制
├── compose.yaml                # dashboard-api / dashboard-mysql / metabase / workers
└── .env.example
```

## 开发启动

```bash
# 从本后端目录执行；先按 .env.example 在受控环境配置数据库、OIDC 和嵌入桥。
# 首次接入通过平台“应用接入”控制面完成。下列为操作参考，本次未执行。
go run ./cmd/local-migrate
go run ./cmd/dashboard-api
```

本地 Compose 会先执行 `dashboard-migrate`，迁移成功后才启动 API 与 Worker。生产发布由
`production-migrate` 执行同一套编译进二进制的 SQL；生产编排必须在启动迁移容器前完成备份门禁。

## 当前数据与统计边界

合同、项目提供原生摘要；报告/财务也已接入 `business_dashboard_snapshot` 原生快照，经营总览、报告和财务页面同时可加载配置的 Metabase 嵌入图表。API 快照不存在时返回 `available=false`，不能当作真实零业务。摘要快照、维表下钻、月度事实来自不同同步通道，应分别核对更新时间。

2026-10-23新增项目完整投影：`PROJECT_PROJECTION_URL` 单独指向项目机器接口 `/internal/v1/dashboard/projects/snapshot`，复用项目专用机器凭据。只配置该URL也会登记项目源，周期与手工队列调用同一producer；`PROJECT_REPORT_URL`调用`/internal/v1/dashboard/reports`并复用`PROJECT_MACHINE_*`及`dashboard.project.read`；`PROJECT_READ_DSN`仅保留可选报告统计兼容来源，`PROJECT_INTERNAL_URL`仍为独立租户摘要通道。

迁移000021新增暂存及独立发布状态。接收预算默认35s、50k项目、64MiB、单行64KiB；必须完整验证header/project/end、顺序、租户、原始行SHA和数量才在事务中替换本租户维表。所有写入保留商业门禁、队列租约及投影generation，提交前重核。失败不清旧发布、独立成功时间或另一路报告快照；可信完整空集合才清本租户项目。

组织/团队项目统计使用同一只读RR事务读取已发布维表和独立水位，以可信owner_org_id过滤，直接汇总源risk/in_flight/service_items。完整发布后的范围内0项可用，无发布记录为UNKNOWN。最新失败/运行中/停用保留last_success_at并标陈旧，创建趋势使用源created_at月份。TENANT原有持久摘要及合同freshness保持兼容。

合同/项目摘要响应包含 `freshness`，区分 `source_updated_at`（范围内源业务时间）、`synced_at`（成功同步或持久快照写入时间）、`computed_at`（本次查询时间）。合同维表核验 `sync_source` 最近成功凭据，后续失败/运行中保留成功时间并标陈旧；项目ORG/TEAM使用独立投影状态与 `last_success_at`，后续失败仍保留成功时间并标陈旧。停用、失败、运行中及超过48小时展示容差均说明原因。全租户持久快照保持原 `snapshot_at`、`SNAPSHOT` 与写入时间语义；恰好48小时仍在容差内，缺失、非法或晚于查询时刻的快照时间标陈旧，没有快照仍不可用。源时间缺失留空，不用查询时间代替，也不把未标陈旧当作实时保证。

报告与财务的主通道为机器 HTTP，兼容只读源 `CONTRACT_READ_DSN`、`CRM_READ_DSN`、`PROJECT_READ_DSN`、`SETTLEMENT_READ_DSN` 默认留空，只有明确选择兼容读取才配置 SELECT-only 账号。合同、CRM、审批维表按可信完整源快照成功发布后做删除对账；来源失败、取消或失去许可、租约时保留旧发布。项目完整投影维持同代次原子发布。手工同步 QUEUED 仅表示入队；API 摘要源 `contract_dashboard`、`project_dashboard` 与项目报告、结算财务使用与自动轮次相同的 producer，最终状态仍须核验。队列使用数据库 UTC 租约、心跳与 owner/generation 写栅栏；旧无租约 RUNNING 任务须按[恢复维护说明](docs/sync-job-lease-recovery.md)受控处理，不能直接改库重置。

全租户签约事实按 `start_date` 月、币种归集，默认只计 `active/in_progress/pending_pay/completed`；商机漏斗由独立 `fct_opportunity_conversion` 按有效商机创建月归集，分子为已签约且关联同租户有效合同的去重商机，不再拿合同数代替商机数。合同与 CRM 同步均成功后才重算转化；组织/团队转化在同一可信范围内分别查询有效商机分母与有效 NORMAL 合同关联商机分子，CRM 和合同均须存在成功同步凭据，零分母明确不可计算。折扣缺少权威标准报价，明确不可用；金额按币种展示，不做汇率换算。组织/团队趋势直接按范围内维表计算，不能与全租户月度签约事实视为同一口径。指标字典的确认文本不是计算实现。字典支持租户版本与自定义定义，但仅符合内置 `2.1` 规则的项目状态分布可以启用计算器，自定义公式不执行 SQL。

报告快照展示有效/作废报告版本、状态分布及编制至签发 P50（仅完整非负时间样本）；首过率/投诉数未提供。财务快照按币种展示已确认应收、已核销回款、未收余额、超过 90 天余额及北京时间当月有效已登记回款；仅计 CONFIRMED 且 RELEASED 应收及有效回款，排除 HOLD，冲销按净额；回款不是收入确认，成本/利润尚未提供。报告/财务缺来源或缺快照时保持不可用，不能补零冒充经营结果。

原生合同/项目摘要、明细和趋势按 principal 的 `TENANT`/`ORG`/`TEAM`/`NONE` 过滤；报告/财务快照只对显式 `TENANT` 范围开放。预警中心按 `alert_item.owner_org_id` 过滤，历史无归属预警仅 `TENANT` 可见。Metabase 嵌入参数由服务端签名下发，但看板 SQL/参数配置不在本仓库，仍需现场核对。到期预警目前仅 `CONTRACT_EXPIRY`，受理/关闭只更新状态与时间，没有处理人/处理说明字段，也没有该 Worker 自动通知投递。

Metabase 内网地址或嵌入密钥缺失会令当前 API 装配失败；仅缺独立公开来源时嵌入不可用，不代表已运行的原生摘要关闭。独立来源约束与验收见[BI 配置说明](../sharedDocs/BI独立嵌入来源配置.md)。

报告机器 URL 为 `PROJECT_REPORT_URL`，结算财务 URL 为 `SETTLEMENT_FINANCE_URL`（`/internal/v1/dashboard/finance`），后者使用独立 `SETTLEMENT_MACHINE_CLIENT_ID`、`SETTLEMENT_MACHINE_CLIENT_SECRET` 和唯一 `SETTLEMENT_MACHINE_TOKEN_SCOPE=dashboard.settlement.read`。平台用途为 `settlement_dashboard_read`，发送方为同租户同环境 `data_analysis`；接收端分别绑定公开客户端标签与 OAuth 注册记录 ID。两 URL 默认留空表示未接入，平台受控配置后才登记来源；无法访问或响应不可信时记录 FAILED 并保留旧快照，不补零。`DASHBOARD_FINANCE_SOURCE=standard` 不读取结算 URL、机器凭据或 DSN，只读已发布标准事实；`DASHBOARD_INTEGRATION_SYNC_ENABLED=false` 不解析或装配任何外部链路。

`DASHBOARD_SNAPSHOT_RECENT_DAYS=7`、`DASHBOARD_SNAPSHOT_TREND_MONTHS=24`、`DASHBOARD_SNAPSHOT_BATCH_SIZE=500`、`DASHBOARD_SNAPSHOT_MAX_BATCHES=20` 控制有界 API 摘要留存：保留近 7 天全部、近 24 个业务月的月末快照与全局最新，逐轮清理旧冗余。范围分别为 1..3660、24..120、1..500、1..20；显式 0 或非法值在外部同步启用时拒绝启动。清理按租户及固定表白名单执行，不删除标准财务批次、审计、历史合同发布凭据或月度事实。

项目下钻 `GET /dashboard/projects` 使用可信会话租户及 TENANT/ORG/TEAM 范围，分页默认 1/20、上限 100，同一只读 RR 快照核验成功投影与读取列表；无发布或无可信范围返回 `available=false`，成功空集合为 true 且 items 为空。趋势以 `contract_available`、`project_available` 分别表示当月来源是否可用，多币种金额仅在 `currency_totals` 中分币种展示，不相加。报告、财务可附 `source_code`、`source_updated_at`、`source_snapshot_at` 与 `freshness`；源业务更新时间、源生成快照时间、本地成功同步时间和本次查询时间不可互相替代。`GET /auth/me` 返回可信 `scope_mode`、`organization_ids` 及 `team_ids`，页面不得从角色名推断范围。本轮相关接口契约见 [OpenAPI](docs/openapi.yaml)，该文件仅覆盖列出的接口，并非全 API 目录。

### 数据库迁移约束

1. 迁移文件使用连续的 `000001_descriptive_name.sql` 命名，发布后不得修改或删除。
2. 迁移器使用 `da_schema_migration` 保存版本、名称、SHA-256 checksum 和应用时间；历史 checksum 不一致时立即终止。
3. 迁移期间持有 MySQL `GET_LOCK` 会话锁，避免多个发布任务并发执行 DDL。
4. MySQL DDL 可能隐式提交，因此每份迁移必须可安全重试，优先使用 `IF NOT EXISTS` 和幂等数据变更。
5. 运行时禁止 `AutoMigrate`。MySQL 初始化目录只创建 `dashboard_metabase`，业务 Schema 必须通过迁移二进制生成。

> 实现状态以代码、测试和 `da_schema_migration` 记录为准。生产发布前还必须完成数据源、Metabase 嵌入密钥、机器身份、授权目录和运行验收。
