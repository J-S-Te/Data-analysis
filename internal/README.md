# internal 包说明（骨架）

| 包 | 状态 | 复用来源 |
|---|---|---|
| middleware | 待填充 | 直接复制/适配 CRM `internal/middleware`：auth.go（SessionAuth / RequireSameOriginWrite / MachineAuth）、request_audit.go、request_id.go |
| shared/auth | 待填充 | CRM `internal/shared/auth`（principal） |
| shared/requestaudit | 待填充 | CRM `internal/shared/requestaudit`（outbox + poll 投递，独立审计机器 Client） |
| shared/integrationhttp | 待填充 | CRM `internal/shared/integrationhttp`（TLS 约束 HTTP 客户端） |
| platformcatalog | 待填充 | CRM `internal/platformcatalog`（catalog.go / manifests.go） |
| orgdirectory | 待填充 | CRM `internal/modules/ownerdirectory`（http_client.go 调平台 `GET /internal/owner-directory`，scope=owner_directory.read） |
| embedbridge | 新增 | 无先例：签名嵌入令牌签发/单次消费代理/Metabase API 客户端（设计方案 §4.4） |
| aggregation | 新增 | 无先例：源库适配器 + 水印增量 + 预计算（设计方案 §5.3） |
| alerting | 新增 | 规则引擎 + alert_item upsert（设计方案 §6） |
| bootstrap | 待填充 | CRM `internal/bootstrap`（config.go / app.go 装配模式） |
