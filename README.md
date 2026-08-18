# data_analysis · 数据看板与统计分析子系统

> 新子系统骨架（V0.1，2026-08-17）。技术栈与工程模式**严格对齐 customer_and_opportunity**（Go + Gin + GORM + Temporal + 多 cmd 二进制 + 单 Dockerfile + Vue 3 统一前端模块）。
>
> 权威设计：`Data-analysis/数据看板与统计分析子系统 · 基础平台接入与鉴权设计方案.md`（V0.1）
> 权限基线：`authz/permission-manifest.yaml`（V1，格式对齐合同 manifest）
>
> **部署拓扑（团队约定）**：前端代码在统一仓库 `frontend/src/modules/data_analysis/`（随 `frontend` Docker 镜像构建，与其余子系统同一前端容器）；本目录仅存放**后端**（Go），`dashboard-api` 等各自独立 Docker（见 `compose.yaml`）。

## 版本与仓库归属

- 后端仓库：`data_analysis/` 独立 Git 仓库，与 `platform`、`contract_management` 等子系统保持相同的版本边界。
- Go module：`github.com/unified-identity-auth-platform/data-analysis`。
- 当前应用版本：根目录 `VERSION` 中的 `0.1.0`；发布标签建议使用 `v0.1.0` 格式。
- 当前数据库版本：`000005_api_project_dashboard`。数据库实际版本以 `da_schema_migration` 为准，不能仅根据文件名推断。
- 统一前端仍归属 `frontend` 仓库，不随本后端仓库单独发版。

## 目录结构

```text
data_analysis/
├── cmd/
│   ├── dashboard-api/          # 嵌入桥 + 业务 API（OIDC 会话 / embed / 预警中心 / 字典 / 管理）
│   ├── aggregation-worker/     # 夜间跨库同步 + T+1 预计算（可定时循环；Temporal 调度待接入）
│   ├── alert-worker/           # 预警规则计算（聚合库上，幂等）
│   ├── authz-catalog/          # 授权目录发布（复用 CRM platformcatalog 模式）
│   ├── local-migrate/          # 本地迁移执行
│   └── production-migrate/     # 生产迁移执行（受控）
├── internal/
│   ├── bootstrap/              # 配置装配（对齐 CRM internal/bootstrap）
│   ├── middleware/             # 会话鉴权、请求 ID、时间与同源写校验
│   ├── shared/                 # auth / response / apperror
│   ├── platformcatalog/        # 目录发布（对齐 CRM internal/platformcatalog）
│   ├── embedbridge/            # 嵌入桥：令牌签发 / 代理 / Metabase 客户端
│   ├── aggregation/            # 跨库同步适配器 + 预计算（写聚合库）
│   ├── alertworker/            # CONTRACT_EXPIRY 规则计算与幂等快照
│   └── modules/
│       ├── alerts/             # 预警中心 API 与可替换 Store
│       ├── dictionary/         # 指标字典 API
│       └── admin/              # 预警规则配置 / 数据源状态
├── authz/permission-manifest.yaml
├── migrations/                 # 聚合库版本化 SQL（当前 000001 至 000005）
├── Dockerfile                  # 单 Dockerfile 多二进制
├── compose.yaml                # dashboard-api / dashboard-mysql / metabase / workers
└── .env.example
```

## 开发启动

```bash
go mod tidy            # 首次拉取依赖
cd .. && bash platform/scripts/subsystem.sh onboard --application-code data_analysis ...   # 平台接入（见设计方案 §3.1）
cd data_analysis
go run ./cmd/local-migrate
go run ./cmd/dashboard-api
```

本地 Compose 会先执行 `dashboard-migrate`，迁移成功后才启动 API 与 Worker。生产发布由
`production-migrate` 执行同一套编译进二进制的 SQL；生产编排必须在启动迁移容器前完成备份门禁。

### 数据库迁移约束

1. 迁移文件使用连续的 `000001_descriptive_name.sql` 命名，发布后不得修改或删除。
2. 迁移器使用 `da_schema_migration` 保存版本、名称、SHA-256 checksum 和应用时间；历史 checksum 不一致时立即终止。
3. 迁移期间持有 MySQL `GET_LOCK` 会话锁，避免多个发布任务并发执行 DDL。
4. MySQL DDL 可能隐式提交，因此每份迁移必须可安全重试，优先使用 `IF NOT EXISTS` 和幂等数据变更。
5. 运行时禁止 `AutoMigrate`。MySQL 初始化目录只创建 `dashboard_metabase`，业务 Schema 必须通过迁移二进制生成。

> 子系统仍处于 V0.1 增量建设阶段；实现状态以代码、测试和 `da_schema_migration` 记录为准。
