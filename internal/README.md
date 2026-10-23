# internal 包与当前实现边界

2026-10-22 按实际调用链核实；本目录不是待复制的骨架。未运行新动态测试。操作见[数据看板手册](../../sharedDocs/10_操作手册/数据看板与统计分析操作手册.md)，历史逐项依据见[2026-10-13 台账](../../.ai/modules/operation-docs-business-20261013.md)。 本轮静态记录见[2026-10-22 文档同步报告](../../.ai/qa/docs-code-sync-20261022/agents/project-bi-settlement-report.json)。

| 包 | 现有职责 | 必须保留的边界 |
| --- | --- | --- |
| bootstrap | 配置、OIDC、库连接、API 路由与服务装配 | 嵌入桥地址/密钥缺失可导致启动失败 |
| middleware、shared/auth | Cookie 会话、精确权限、可信租户、同源写校验 | 不能靠 UI 角色标签推断组织范围 |
| auditoutbox、platformaudit | 字典/规则/同步/预警事务审计持久化与投递、请求审计客户端 | API 装配投递循环；请求级审计与事务 Outbox 仍是两条路径，配置与回执需部署侧确认 |
| platformcatalog | 发布应用授权目录 | 目录存在不代表用户已获权限 |
| embedbridge | 短期令牌、单次入口消费、Metabase 资源代理 | 独立来源、租户锁定、资源白名单；不开放管理后台 |
| aggregation | 合同/CRM 维表、项目报告/结算财务只读快照、合同/项目 API 快照、租约队列、签约/商机转化事实 | 不是 CDC；合同源删除不传播，队列成功不等于后续全部事实已重算 |
| alertworker | CONTRACT_EXPIRY 候选扫描、幂等预警 | 没有自动通知装配；不会自动关闭消失候选 |
| modules/dashboard | 合同/项目及报告/财务摘要、合同维表分页、分币种趋势和时间新鲜度 | 无快照为 available=false；报告/财务快照只对显式 TENANT 范围开放，多个来源时间不保证一致 |
| modules/alerts | 按归属组织过滤的预警分页、范围内摘要、ACK/CLOSED 更新 | 无归属旧预警仅 TENANT 可见；无处理人/处理结果业务字段、无严格前序状态限制 |
| modules/dictionary | 内置/租户指标、版本历史、启停和自定义定义 | 公式是文本；仅内置 2.1 的兼容定义可绑定现有计算器 |
| modules/admin | 租户到期规则治理、来源状态、同步入队 | 非任意连接编辑；停用标记不约束当前 DSN 定时遍历 |

## 完整链路索引

统一 Vue 页面与请求位于 `frontend/src/modules/data_analysis/`（工作区相对路径）。[路由装配](bootstrap/app.go) → [鉴权](middleware/auth.go) → [摘要 Service](modules/dashboard/application/service.go) → [摘要仓储](modules/dashboard/infrastructure/gorm_repository.go)。快照由[同步 Worker](../cmd/aggregation-worker/main.go)装配[聚合器](aggregation/aggregation.go)与[API 同步器](aggregation/apisync.go)，表结构由[迁移](../migrations/embed.go)管理。

[预警规则](modules/admin/application/service.go) → [预警计算](alertworker/worker.go) / [候选与幂等存储](alertworker/gorm_store.go) → [人工状态处理](modules/alerts/application/service.go)。[字典治理](modules/dictionary/application/governance.go)与[治理迁移](../migrations/000013_metric_dictionary_governance.sql)维护元数据和不可变版本，不自动生成统计任务。

全租户合同签约事实按 start_date 月和币种归集，默认有效状态为 active/in_progress/pending_pay/completed。独立商机事实按创建月队列计算同租户有效合同支持的已签约商机，不以合同数替代分子；组织/团队漏斗尚无同粒度事实。折扣无权威基准，金额不做币种换算。报告/财务原生统计由 aggregation/business.go 从只读业务源生成，未提供首过率、投诉、收入确认或成本。需求中的指标定义保留为需求，不用字典已确认状态掩盖计算边界。

合同/项目摘要以 freshness 分离源更新、成功同步和查询计算时间；同步失败/停用/未知或超过 48 小时保留旧值并标陈旧。全租户持久快照保持 snapshot_at 语义，当前不套 48 小时判定；dim_project 仍无写入器，组织/团队项目摘要不能借报告统计水位伪称最新。手工同步的租约/旧任务恢复见[维护说明](../docs/sync-job-lease-recovery.md)。
