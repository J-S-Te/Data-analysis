# 标准经营事实导入 v1：财务

本能力独立于合同、项目、CRM、结算系统，只处理截至某个业务日的完整已确认应收与有效回款事实。它不提供其他事实类型、任意 SQL/数据源接入、收入确认、部门成本、退款或增量账务同步。

## 启用及兼容

`DASHBOARD_FINANCE_SOURCE=integration|standard`，默认 `integration` 保持原联动查询；非法值启动失败。`standard` 只读取标准来源已发布批次，绝不回退或混加联动快照。切回 integration 不删除标准批次；两个来源相互独立。

原生部署还可设 `DASHBOARD_EMBED_ENABLED=false`，不要求 Metabase 凭据和进程。标准财务来源与嵌入开关独立；本次标准指标只承诺原生财务看板，不自动改造外部 Metabase 图表。

上线前使用既有受控迁移入口执行 `000033_standard_finance_facts.sql`。源码不会运行 AutoMigrate。没有迁移或来源存储故障时返回错误，不能解释为“尚未发布”。本任务未执行任何业务数据库迁移。

## 文件契约

UTF-8 JSON，`schema_version=1`，文件最多 1 MiB，应收和回款合计最多 1000 条。不允许额外字段、客户端 tenant_id 或计算后指标。两类数组都必填，空数组代表使用者明确确认没有该类记录；`complete=true` 必填。每批是本来源完整事实替换，不能把本月新增部分当完整文件发布。

| 字段 | 业务含义及校验 |
| --- | --- |
| batch_id | 使用者填写的唯一批次编号，1–64字符；字母数字开头，仅字母数字及 `_.:-`。同编号只对应同一规范化完整内容 |
| as_of_date | 截至业务日，严格 YYYY-MM-DD，不晚于北京时间今天 |
| receivables[].external_id | 已确认应收的外部编号，在本类事实内唯一，同 batch_id 字符规则 |
| receivables[].currency | 三位大写币种代码，不跨币种累加 |
| receivables[].original_minor | 原始已确认应收，非负整数；单位为币种金额的 1/100；单条最多 1,000,000,000,000（100亿元） |
| receivables[].allocated_minor | 截至日已核销回款额，整数，0至原始应收 |
| receivables[].due_date | 应收到期业务日，严格 YYYY-MM-DD，可晚于截至日 |
| receipts[].external_id | 截至日有效登记的回款外部编号，在回款事实内唯一 |
| receipts[].currency | 三位大写币种代码 |
| receipts[].amount_minor | 有效回款金额，正整数，同 1/100 币种单位及单条上限 |
| receipts[].receipt_date | 有效登记业务日，严格 YYYY-MM-DD，不晚于截至日 |

所有行字段都必须显式填写，缺失/null不能代表零。金额不用浮点元；例如 100.25 元写为 10025。单条金额与1000条共同限额保证聚合数值低于浏览器安全整数上限。文件内无责任人/组织字段，本版不允许把全租户汇总读取为 ORG/TEAM/SELF 范围。

空模板可从 API 下载：

```json
{
  "schema_version": 1,
  "batch_id": "",
  "as_of_date": "2026-11-04",
  "complete": true,
  "receivables": [],
  "receipts": []
}
```

空 batch_id 有意不合法，使用者必须填写批次；模板没有演示事实。下载的截至日使用服务端北京时间今天。

## API 与权限

均位于数析前缀 `/data_analysis/api/v1`，继续经过既有服务端会话、商业许可、写请求同源/CSRF和审计门禁。管理四接口额外要求 `aggregation.manage`、有效当前租户/用户以及明确 `ScopeMode=TENANT`；不能以角色名称或缺范围字段推断全租户。租户和操作者只取自会话。

| API | 输入与结果 |
| --- | --- |
| GET /admin/standard-finance/template | 返回 `{filename, document, guide, max_rows, max_bytes}` |
| GET /admin/standard-finance/status | 返回 `{selected_source, publication:{source_code,published,version,batch_id,digest,published_at,replayed}}`；联动模式不查询标准表 |
| POST /admin/standard-finance/preview | body为完整文件；不写数据库；返回`{schema_version,batch_id,digest,row_count,expected_version,snapshot}` |
| POST /admin/standard-finance/publish | body为`{document,digest,expected_version}`；重新校验并原子发布，返回权威 publication |
| GET /dashboard/finance | 保持原 `dashboard.finance.view` 权限及应用层明确 TENANT 范围检查；standard时读取当前发布快照 |

预览 digest 是标准 Go JSON 编码的 SHA-256，绑定完整内容与数组顺序。发布不信任客户端计算指标。指纹不符返回400 `STANDARD_FACTS_PREVIEW_MISMATCH`；事实非法400；旧来源版本409 `STANDARD_FACTS_VERSION_CONFLICT`；同 batch_id 不同内容409 `STANDARD_FACTS_BATCH_CONFLICT`；来源未启用503 `STANDARD_FACTS_SOURCE_DISABLED`；范围拒绝403 `STANDARD_FACTS_SCOPE_DENIED`。

首次版本0。成功发布后+1。若同租户、同标准来源、同 batch_id 和同 digest 已成功发布，重试返回该批次原回执、`replayed=true`，不重复事实/审计，也不把历史批次重新切成当前版本。其他批次必须匹配当下版本。超时/断网后先查 status 核对批次，禁止通过更换 batch_id 伪造重试。

## 持久化及指标

固定来源命名空间 `standard-finance`。独立 `standard_finance_source` 保存当前版本/批次指针，`standard_finance_batch` 保存不可变发布证据及服务端计算快照，`standard_finance_fact` 保存按 tenant/source/batch/type/external_id 隔离的事实。发布事务对来源行 `SELECT ... FOR UPDATE`，核验版本与批次，写入完整事实、快照、指针及 `bi_audit_outbox`；审计、许可或任一写入失败回滚全部。本版保留历史批次，无自动删除任务；后续清理策略需另行明确。

本来源不写 `business_dashboard_snapshot`、`api_*_dashboard`、`dim_*` 或 `fct_*`，worker继续只维护原联动来源，不覆盖本来源。

服务端按币种计算原始应收、已核销额、未清余额、到期日早于截至日减90天的未清余额、截至月份有效登记回款。回款率=已核销额/原始应收；原额为0时比例不可用。不同币种分别展示；不把回款称为收入。snapshot_at是发布时刻，basis同时标明来源、批次和事实截至日，不把发布时间冒充事实日期。

未发布返回 available=false 和明确原因；完整空文件发布后 available=true/currencies=[]，表示真实零值。存储故障或损坏的发布指针/快照返回错误，不降级为未发布或旧联动数据。页面采用既有财务指标与 basis 展示，缺失收入/部门成本仍明确不可用。

## 当前验证边界

可用单元、隔离内存 SQLite 事务、真实组件脚本与服务端渲染测试及本地构建覆盖代码边界。未连接业务数据库；没有隔离MySQL DSN，未验证真实 MySQL 锁竞争/并发、真实SQL迁移、OIDC/许可服务、浏览器上传及部署业务闭环。此文档不把这些检查描述为已验收。
