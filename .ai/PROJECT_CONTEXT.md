# Data Analysis Project Context

## 项目简介

数据看板与统计分析后端，负责租户隔离的聚合快照、趋势、合同下钻、预警和嵌入桥接口。统一前端模块位于相邻的 `frontend` 仓库。

## 技术栈与边界

- Go、Gin、GORM、MySQL；多个 `cmd` 二进制分别运行 API、聚合 worker 和预警 worker。
- 统计数据写入聚合库，业务源数据通过受控内部接口或只读同步获取。
- 所有业务查询必须使用认证 principal 的 `tenant_id`；前端不自行推断数据范围。
- 预警规则使用 `alert.manage` 权限；当前可执行类型仅 `CONTRACT_EXPIRY`。API/Worker 不运行时自动播种；新建默认停用，显式启用后扫描。保存保留稳定 ID 与不可变版本快照并检查版本；有历史告警只能停用，未关联历史允许删除。
- 告警候选与写入均限制 Worker 配置租户；写入/停用/删除共用规则行锁，避免失效规则迟到写入。原 Worker 返回数量仍为候选数量，非并发状态变更后的精确持久化数量。
- 合同与项目原生摘要页展示真实聚合数据；没有快照时显示空态，不填充演示数据。
- 指标字典读取使用 dictionary.view，维护使用 dictionary.manage（目录 data-analysis-v2）；编码不可变，新建默认停用，版本冲突拒绝写入，删除保留墓碑和不可变历史。内置与已登记引用保护；历史接口返回最近 200 条。
- migration 13 增加字典启用/来源/删除状态、历史快照与引用登记表。数据库读取失败显式返回错误，不静默回退。自定义公式仅口径说明，不执行任意计算；当前仅已确认且与内置定义一致的 2.1 绑定 PROJECT_STATUS_COUNTS_V1，其他未绑定指标不可启用。字典启停不动态改变既有固定看板计算。

## 关键入口

- `internal/modules/dashboard/`：合同/项目摘要、趋势和合同明细。
- `internal/modules/alerts/`：预警列表、状态和聚合。
- `internal/modules/dictionary/`：租户字典、生命周期和版本历史。
- `internal/aggregation/`：跨库同步与聚合事实表写入。
- `internal/embedbridge/`：Metabase 嵌入 token 与代理。
- `migrations/`：版本化聚合库 Schema，运行时禁止 AutoMigrate。

## 验证方式

后端在本目录执行 `GOCACHE=/tmp/data-analysis-deep-go-cache go test ./...`、`go vet ./...`；统一前端在 `frontend` 执行 `npm test -- --runInBand` 和 `npm run build`。
