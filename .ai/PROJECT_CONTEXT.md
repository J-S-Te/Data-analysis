# Data Analysis Project Context

## 项目简介

数据看板与统计分析后端，负责租户隔离的聚合快照、趋势、合同下钻、预警和嵌入桥接口。统一前端模块位于相邻的 `frontend` 仓库。

## 技术栈与边界

- Go、Gin、GORM、MySQL；多个 `cmd` 二进制分别运行 API、聚合 worker 和预警 worker。
- 统计数据写入聚合库，业务源数据通过受控内部接口或只读同步获取。
- 所有业务查询必须使用认证 principal 的 `tenant_id`；前端不自行推断数据范围。
- 合同与项目原生摘要页展示真实聚合数据；没有快照时显示空态，不填充演示数据。

## 关键入口

- `internal/modules/dashboard/`：合同/项目摘要、趋势和合同明细。
- `internal/modules/alerts/`：预警列表、状态和聚合。
- `internal/aggregation/`：跨库同步与聚合事实表写入。
- `internal/embedbridge/`：Metabase 嵌入 token 与代理。
- `migrations/`：版本化聚合库 Schema，运行时禁止 AutoMigrate。

## 验证方式

后端在本目录执行 `GOCACHE=/tmp/data-analysis-deep-go-cache go test ./...`、`go vet ./...`；统一前端在 `frontend` 执行 `npm test -- --runInBand` 和 `npm run build`。
