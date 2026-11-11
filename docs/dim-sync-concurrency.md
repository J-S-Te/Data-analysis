# 维表全量同步并发防护（BIZ-DATA-08）

## 背景

`internal/aggregation` 的合同/客户/商机全量同步逐行执行
`INSERT ... ON DUPLICATE KEY UPDATE`。原实现无条件覆盖所有业务列：
只要某个同步轮次读到的行被写入，本地镜像就被改成该行内容——即使它是**更旧的**来源版本
（例如两个进程/两次调度重叠，较旧的读取结果后到；或源库主从延迟导致读到旧值）。

同时，**修复前没有跨进程互斥**：`Runner.RunOnce`（定时调度入口）不持有任何分布式锁；只有
`RunQueued` 领取的 `sync_job` 通过 `sync_job.lease_*` 做了独占租约。

## 本次修复（已实现）

`dim_contract` / `dim_customer` / `dim_opportunity` 的 upsert 现在以来源行
`updated_at` 作为版本防护：

```sql
INSERT INTO dim_contract (...) VALUES (...)
ON DUPLICATE KEY UPDATE
  title=IF(VALUES(updated_at)>=updated_at,VALUES(title),title),
  amount_minor=IF(VALUES(updated_at)>=updated_at,VALUES(amount_minor),amount_minor),
  ...,
  updated_at=IF(VALUES(updated_at)>=updated_at,VALUES(updated_at),updated_at)
```

要点：

- **旧数据不覆盖新数据**：来源行 `updated_at` 早于已存行时，所有业务列保持原值。
- **updated_at 最后赋值**：MySQL 对 `ON DUPLICATE KEY UPDATE` 赋值从左到右求值，
  守卫表达式必须读到旧行时间；把 `updated_at` 放在最后才能保证守卫不被自身赋值绕过。
  `internal/aggregation/dim_upsert_test.go` 对此有形状断言。
- **相等时间戳仍允许覆盖**：同一来源行的幂等重放（相同 `updated_at`）行为不变。
- **语句由 `guardedDimUpsert` 统一生成**，避免三个语句的守卫规则漂移；占位符顺序与
  原有插入列顺序完全一致，调用方参数未变。

## 跨进程互斥（已实现）

`Runner.RunOnce`（定时调度入口）现在按 **租户 + 来源** 持有跨进程互斥锁：

- 生产 MySQL 使用会话级 `GET_LOCK(name, 0)`，锁名由租户与来源的
  SHA-256 截断生成（`data-analysis:sync:` + 40 hex，共 59 字符，低于 MySQL 64 字符上限），
  只含哈希，不带租户/DSN 原文。
- 锁在固定的 `*sql.Conn` 上获取并持有到本轮来源同步结束；正常路径显式
  `RELEASE_LOCK` 后归还连接，进程崩溃时连接关闭也会自动释放。
- 重叠实例拿不到锁时**跳过该来源**：不执行全量读取、不写维表、不覆盖
  `sync_source` 水位，只记录一条 WARN；本轮其余来源继续。锁后端错误则把该来源
  本轮记为失败（错误返回调度器），同样不写入维表或同步状态。
- SQLite 等单进程测试库退化为进程内互斥（`sync.Mutex`），不提供跨进程保证；
  生产装配始终是 MySQL。
- 手工队列链路 `RunQueued` 在领取任务前也获取同一把来源锁：定时同步持锁时任务保持
  `QUEUED` 等待下一轮，不会与定时全量同步并发写维表。`sync_job.lease_*` 仍负责
  “同一手工任务不被重复执行”，`GET_LOCK` 负责“同一租户/来源同时只有一个全量同步”，
  两者互补。

注意：`Pass.RunOnce` 中由内部 API 驱动的合同/项目快照同步不经过本锁；项目投影
已有自己的代际/发布锁，合同看板快照仍依赖单实例调度，跨进程重叠属于残余风险。

## 轮次起止与 precompute 重叠闸门（已实现，默认 warn）

来源锁只保证“同一租户/来源同时只有一个执行者”，挡不住两个 worker 版本在滚动升级窗口内
**各自跑不同来源**再把派生预计算写成混代次结果。为此新增 `sync_run`（迁移 `000031`）：

- `sync_run(tenant_id, subsystem_code, run_started_at, owner, finished_at)`，主键
  `(tenant_id, subsystem_code, run_started_at)`；`finished_at` 为空表示该轮未结束
  （进行中，或进程崩溃后残留）。`owner` 是 `hostname:pid`，不含 DSN/凭据。
- 每个定时轮次（`runScheduledSources`）开始时写一行、结束时补 `finished_at`；
  收尾使用 `context.WithoutCancel`，取消/超时也会尝试补齐。
- precompute 之前做重叠检测：同租户/`aggregation` 子系统下存在 `finished_at IS NULL`
  且 `run_started_at` 早于本轮、并且不早于 2 小时陈旧阈值的轮次 → **跳过 precompute**。
  precompute 是派生数据，跳过不损失事实，只避免混代次。
- `AGG_RUN_OVERLAP_GUARD`：
  - `warn`（默认）：只记 WARN，当轮不失败；
  - `enforce`：同样跳过 precompute，但把重叠计为当轮错误；错误在本轮全部来源执行完之后
    才汇总，**不阻断其它来源**；
  - 非法值让 aggregation-worker 启动失败（fail-closed）。
- `sync_run` 表缺失或写入失败时只记 WARN 并放行 precompute（守卫是可观测性增强，
  不能因为审计表不可用冻结派生事实）；超过 2 小时的未结束行视为崩溃残留，不再阻断。
- 手工队列路径（`RunQueued`）不写 `sync_run`；它仍受来源锁与任务租约保护。

### 滚动升级发布顺序（约束，不能只靠代码）

1. 本次升级先做迁移（`000030`/`000031`/`000032`，均可重入）；
2. aggregation-worker 先缩到 1 副本（或停旧实例）再起新实例，最后恢复副本数；
   **同一个 release 内不允许新旧两个版本并行跑轮次**——`sync_run` 只能“检测并跳过”，
   不能阻止老镜像（不认识本表）继续写。
3. 连接池：`DASHBOARD_MYSQL_DSN` 的连接数需 ≥ 2（1 条持 `GET_LOCK` 的来源锁连接 +
   1 条写入），否则锁连接会与写入争用。
4. 稳定后把 `AGG_RUN_OVERLAP_GUARD` 从 `warn` 切 `enforce`，让重叠以错误形式进入调度告警。

## 合同审批投影通道（`contract_approval`）

`dim_contract_approval`（迁移 `000030`）是 `con_approval_instance` 的只读投影，
供看板在 `DASHBOARD_APPROVAL_SOURCE=instances` 时用 `EXISTS(status='running')` 计数：

- 与合同维表共用 `CONTRACT_READ_DSN`，但使用**独立** `sync_source` 行（`subsystem_code='contract_approval'`），
  审批表授权缺失只让该通道 `FAILED`，合同维表仍可 `SUCCESS`。
- **守卫列是来源 `con_approval_instance.updated_at`**：审批状态变化不改合同行，把
  `in_approval` 之类标志塞进 `dim_contract` 会被 `con_contract.updated_at` 守卫直接拒绝更新。
- 切换 `instances` 的硬校验：`contract_approval` 通道必须已至少成功一轮
  （`sync_source.last_success_at`，迁移 `000032`）；否则 dashboard-api 启动失败/运行期拒绝，
  **不会静默从“近似 >0”退化成 0**。
- 全租户（TENANT）分支仍读 `api_contract_dashboard`（合同机器接口产出），不双写。
- 部署项：合同库只读账号需 `SELECT con_approval_instance`；`DASHBOARD_APPROVAL_SOURCE`
  需加入 dashboard-api 的部署环境（compose/Secret），未下发时默认 `status`。

## 完整源快照与硬删除对账（2026-11-11）

合同、CRM 客户/商机、审批投影现在分别使用 `REPEATABLE READ` + `READ ONLY`
源事务，保持每批 500 行的主键游标扫描。CRM 两张源表共享同一源事务；合同与审批
继续独立执行，审批表不可读不撤销已完成的合同同步。

- 迁移 `000036_dimension_sync_keys.sql` 新增 `dimension_sync_key`，只存租户、
  128 位随机 run token、维表名称、源主键/审批所属合同及创建时间，不复制业务内容。
- 既有 `guardedDimUpsert` 的 `updated_at` 守卫保持不变。每批全部 upsert 成功后暂存
  该批主键，只有完整扫描、权限检查和源事务提交都成功后才执行删除。
- 聚合库在同一个 `aggregationWrite` 事务内执行租户限定的 `NOT EXISTS` 删除与
  当前 run 暂存清理；CRM 两张维表一起提交。取消、许可失败、源读取/提交失败、
  队列旧持有者或提交前租约失效都会拒绝删除或回滚当前删除事务。
- 成功空源可以删除本租户对应维表全部镜像；其他租户不变。审批对账使用
  `(contract_id, approval_id)` 复合键，不能用审批ID单独近似。
- 来源锁仍由定时/手工队列入口持有；`runDimensionSnapshot` 必须在既有来源锁内调用。
  来源失败的暂存不作为新轮次集合复用。下一次持有同一来源锁时，仅回收该来源
  超过 24 小时的旧暂存，每轮最多 500 项；不会清理其他正在同步来源的暂存。
- `sync_source.last_success_at` 仅在完整同步及删除对账成功后推进，后续失败不清空。
  手工队列终态与任务完成时间共用同一数据库时钟，首次手工成功也能建立成功凭据；
  失败、暂停或失去租约不会推进或清空成功凭据。
  `contract_import_history`、历史合同发布凭据、项目投影代次均不在本对账清理范围。

## API快照留存（2026-11-11）

`PruneSnapshotHistory` 只允许 `api_contract_dashboard` 和 `api_project_dashboard`，
所有查询与删除均限定可信租户。默认策略：

- 保留近 7 个业务日跨度内的所有明细；
- 保留当前业务月及前 23 个月中每月最后一条快照；
- 无论快照年龄多大，保留全局最新；相同 `snapshot_at` 使用 `id DESC` 稳定选取最后一条。

业务月界在注入的 `DASHBOARD_TIMEZONE` 计算后转为 UTC 查询，nil 时区按 UTC。
每次调用固定读取本租户 `MAX(id)` 高水位，月末检查点和删除候选均不超过该水位，
并发写入的新快照即使携带旧时间也不会在本轮被删除。每批最多 500 项、每轮最多
20 批，达到预算后剩余候选留待下一轮。每批事务前后检查取消与权限守卫；
`Runner.PruneSnapshotHistory` 额外复用 `aggregationWrite` 的队列租约行锁与提交前复核。
公共函数的 guard 可用于独立API同步通道，队列执行器应调用 Runner 方法。

`SnapshotRetentionPolicy` 的零值使用 `7/24/500/20`。配置范围依次为
`RecentDays=1..3660`、`TrendMonths=1..120`、`BatchSize=1..500`、`MaxBatches=1..20`；
非法值返回错误，不隐式放宽清理预算。该 helper 不执行调度接线，轮次由启动装配调用。
不清理标准财务批次、业务快照、审计表或历史合同发布凭据。

## 已知残余风险（有 updated_at 防护后仍存在）

- **源库时间不可信**：若来源系统对同一事务内多行写入相同 `updated_at`，或来源时钟回拨，
  以 `updated_at` 为版本号无法区分先后；这需要来源提供单调版本列才能进一步收紧。
- **源快照的时点**：删除对账反映本轮 RR 快照；源库在快照开始后提交的新增/删除由下一轮收敛。
- **跨表一致性**：既有 upsert 仍逐行提交，轮次中途失败可能留下部分更新，但绝不按部分主键集合删除。CRM 客户/商机删除在同一个聚合库事务提交。

## 验证

- `internal/aggregation/dim_upsert_test.go::TestGuardedDimUpsertBuildsStaleSafeAssignments`
  （无 DB，形状/幂等断言）；
- `TestDimUpsertStaleSourceRowDoesNotOverride`（真实 MySQL，需显式隔离的
  `UIP_BI_DIM_SYNC_TEST_DSN_FILE`，无 DSN 时跳过）。
- `internal/aggregation/source_lock_test.go`：锁名有界/按租户来源隔离、进程内互斥、
  锁被占用时跳过且不改水位、执行失败仍释放锁、锁后端错误不触碰来源、
  定时持锁时队列任务保持 `QUEUED`；
  `TestMySQLScheduledSyncLockExcludesSecondHolder` 在隔离 DSN 上验证 `GET_LOCK`
  双持有者互斥，无 DSN 时跳过。
- `internal/aggregation/contract_approval_test.go`：审批投影 upsert 的形状断言
  （守卫列/updated_at 最后/不得出现 `in_approval`/不镜像审批内容）、审批通道登记幂等；
  `TestSyncContractApprovalFromRealMySQL` 在隔离 DSN 上验证跨租户过滤、变更审批投影与
  “旧 updated_at 不回退已完成审批”，无 `UIP_BI_DIM_SYNC_TEST_DSN_FILE` 时跳过。
- `internal/aggregation/sync_run_test.go`：`AGG_RUN_OVERLAP_GUARD` 解析、轮次起止记录、
  重叠时 warn/enforce 的差异（都跳过 precompute，只有 enforce 返回错误）、
  已结束与陈旧轮次不算重叠、`sync_run` 不可用时不阻断 precompute。
- `internal/modules/dashboard/infrastructure/approval_source_test.go`：instances 口径只数
  `running` 实例（含变更审批）、默认 status 口径不变、通道从未成功时拒绝、首次成功后
  即使最新一轮失败仍允许。
- `internal/migration/approval_sync_run_migration_test.go`：三份迁移的编号、守卫式可重入、
  不得出现破坏性语句。

- `dimension_reconciliation_test.go`：成功集合/空源删除、复合键、租户隔离、读取/提交/
  取消/许可失败保留、CRM删除原子性、提交前许可回滚、队列旧租约拒绝、500项暂存回收；
- `snapshot_retention_test.go`：北京月界、近7天明细、完整24月趋势、同时间ID排序、
  全局最新、固定高水位、批次预算、非法表/策略及权限失败回滚；
- `TestDimensionSnapshotRealMySQLRepeatableRead`：需显式隔离的
  `UIP_BI_DIM_SYNC_TEST_DSN_FILE`，验证MySQL跨批RR、源只读约束及删除反连接；
  无DSN时显式跳过，不能用SQLite结果替代MySQL验收。
