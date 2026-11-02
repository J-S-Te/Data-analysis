# 告警生命周期收敛（BIZ-DATA-07）

## 背景

`alert-worker` 一直按规则评估结果**幂等写入** `alert_item`：同一 `(tenant_id, alert_type, target_ref)`
只新增或刷新标题/到期日/严重度，**不改变状态**。这在现网造成了两个可观测缺陷：

1. 合同离开到期窗口（续签、提前结束）后，历史告警永久停留在 `OPEN`/`ACK`，告警中心长期堆积；
2. 告警一旦被 `ACK` 或 `CLOSED`，即使同一条件（例如同一合同再次进入到期窗口）重新命中，也不会
   再次出现。

产品侧尚未确认"离开窗口是否应自动关闭""ACK 后是否允许重开""重开是否清空确认人"等策略，
因此本次修复**不改变现网行为**：收敛能力通过配置开关提供，**默认关闭**
（`ALERT_AUTO_RESOLVE=false` 仍是默认值）。

## 开关与策略

| 环境变量 | 默认 | 取值 | 作用 |
| --- | --- | --- | --- |
| `ALERT_AUTO_RESOLVE` | `false` | `true`/`false` | **总开关**：启用规则评估驱动的自动收敛（自动 CLOSED + 自动复开） |
| `ALERT_CONFIRM_ROUNDS` | `2` | `1`..`10` | 连续**未命中确认轮数**（去抖）：达到该轮数才自动 CLOSED |
| `ALERT_REOPEN_POLICY` | `auto` | `auto`/`manual` | `auto`=条件重新命中时规则自动复开自己关闭的告警；`manual`=只允许人工复开 |
| `ALERT_CLOSE_GUARD_RATIO` | `0.5` | `(0,1]` | **收缩安全阀**：本轮命中数低于"未关闭基数 × 比例"时只 WARN 不关闭 |

- 三个细项**只在 `ALERT_AUTO_RESOLVE=true` 时生效**；`false` 时 `alert-worker` 与既有版本完全
  一致：只做幂等 upsert，不写 `status`/`closed_at`/`closed_reason`/`miss_streak`。
- 取值校验：`ALERT_CONFIRM_ROUNDS`、`ALERT_REOPEN_POLICY`、`ALERT_CLOSE_GUARD_RATIO` 写错时
  `alert-worker` **启动失败**（非零退出），不会静默退化；`ALERT_AUTO_RESOLVE` 沿用宽松解析
  （历史取值不阻断启动）。
- 代码入口：`internal/alertworker.LifecycleOptionsFromEnv`；`cmd/alert-worker` 读取失败即退出。

## 每轮评估的生效链路

1. 规则算出本轮告警集合 → 幂等 upsert（**必须在收敛之前**，否则本轮命中的告警会被误判未命中）；
2. 若 `ALERT_AUTO_RESOLVE=false` → 到此为止（现网行为）；
3. 读取该规则当前**未关闭基数**（`CountActiveAlerts`）；
4. **收缩安全阀**判断（见下文），触发则只写 WARN 日志并跳过本轮收敛；
5. 三阶段收敛（`ReconcileAlerts`）：命中→累计命中/清零未命中；未命中→累计未命中，达确认轮数才
   CLOSED；`ALERT_REOPEN_POLICY=auto` 时对闭原因属于自动原因的记录复开。

## 去抖：确认轮数（`ALERT_CONFIRM_ROUNDS`）

未命中不再是"一次未命中就关闭"，而是**连续计数**：

- 命中：`miss_streak=0`，`hit_streak+1`，写 `last_hit_at`；
- 未命中：`miss_streak+1`；只有 `miss_streak >= ALERT_CONFIRM_ROUNDS` 才写
  `status='CLOSED'` + `closed_at` + `closed_reason='AUTO_CONDITION_CLEARED'`。

因此单轮抖动（上游慢一拍、某次查询偶发空结果）不会立即关闭告警；默认 `2` 表示"连续两轮未命中"。
确认轮数越大越保守，代价是收敛变慢。

## 安全阀：比例收缩只告警不关闭（`ALERT_CLOSE_GUARD_RATIO`）

收敛相对于"当前仍处于 `OPEN`/`ACK` 的告警基数"判断，触发以下任一条件即**整轮跳过收敛**：

| 情形 | 触发原因（日志 `reason`） | 行为 |
| --- | --- | --- |
| 本轮命中数为 `0` | `no_active_hits` | 不收敛、不累计未命中，WARN 日志 |
| 命中数 `< 未关闭基数 × ALERT_CLOSE_GUARD_RATIO` | `active_shrunk_below_ratio` | 不收敛，WARN 日志（含 `evaluated_hits`/`open_total`/`close_guard_ratio`） |

这是**安全阀而不是开关**：上游表缺失、租户切换、筛选条件写错都会表现为"命中集合骤降"，
此时批量 CLOSED 会掩盖真实数据问题，因此宁可保留 OPEN 并告警，也不自动关闭。基数本身为 `0`
时不写日志（没有可关闭的对象）。

## 复开策略（`ALERT_REOPEN_POLICY`）

| 取值 | 条件重新命中时 | 适用场景 |
| --- | --- | --- |
| `auto`（默认） | 规则自动把**自己关闭的**告警复开为 `OPEN`，`reopen_count+1` | 允许"条件回来就重新提醒" |
| `manual` | 规则**不**复开；已关闭告警只能由人工重新打开/确认 | 关闭即终态，避免反复打扰 |

无论哪种取值，**人工关闭（`closed_reason='MANUAL'`）与历史空原因永不被规则复开**。

## 关闭原因语义：`AUTO_CONDITION_CLEARED` 与 `AUTO_WINDOW_EXIT` 的区别

| `closed_reason` | 写入方 | 语义 | 是否可被规则自动复开 |
| --- | --- | --- | --- |
| `AUTO_CONDITION_CLEARED` | 本轮起 `ReconcileAlerts`（迁移 `000028` 之下的确认轮数逻辑） | **条件连续 `ALERT_CONFIRM_ROUNDS` 轮未命中**才关闭，是"去抖后确认条件消失" | 是 |
| `AUTO_WINDOW_EXIT` | 旧版"离开窗口"收敛（迁移 `000027` 语义）写入的历史数据 | 旧口径的"离开窗口即关闭"，可能是单轮抖动；现网存量数据仍在 | 是（保持向后兼容，避免历史自动关闭永久卡住） |
| `MANUAL` | `internal/modules/alerts` 的人工关闭（`UpdateStatus`） | 人的决定 | **否** |
| `''`（历史空值） | 迁移前的存量行 | 视为人工/历史关闭 | **否** |

要点：新代码不再写 `AUTO_WINDOW_EXIT`，但它仍算"自动原因"，只用于让历史自动关闭能被重新提醒。

## 状态转移（启用后）

| 当前状态 | 本轮是否命中 | 结果 |
| --- | --- | --- |
| `OPEN` | 命中 | 不变（`miss_streak=0`，`hit_streak+1`，刷新 `last_hit_at`） |
| `OPEN` | 未命中，`miss_streak < 确认轮数` | 不变（只累计 `miss_streak`，去抖） |
| `OPEN` | 未命中，`miss_streak >= 确认轮数` | `CLOSED`，`closed_reason='AUTO_CONDITION_CLEARED'` |
| `ACK` | 命中 | 不变（保留确认） |
| `ACK` | 未命中并达到确认轮数 | `CLOSED`，`closed_reason='AUTO_CONDITION_CLEARED'`，**`acknowledged_at`/`acknowledged_by` 原样保留** |
| `CLOSED` + 自动原因 | 命中且 `ALERT_REOPEN_POLICY=auto` | 复开为 `OPEN`，清空 `closed_at`/`closed_reason`，`reopen_count+1`，**`acknowledged_*` 保留** |
| `CLOSED` + 自动原因 | 命中且 `ALERT_REOPEN_POLICY=manual` | 不变 |
| `CLOSED` + `MANUAL`/`''` | 任意 | 不变：人工关闭永不被规则评估覆盖 |

其它边界：

- **规则隔离**：只处理同一 `tenant_id + rule_code`；其它规则、其它租户的告警不受影响。
- **改写范围**：只关闭 `OPEN`/`ACK`；已 `CLOSED` 的记录不会被重复改写。
- **空命中集合**：由安全阀 `no_active_hits` 拦住，**不会**批量关闭（旧文档"空集合收敛全部"的描述
  已作废）。

## ACK 证据不被自动关闭清除

自动关闭/自动复开只改 `status`/`closed_at`/`closed_reason`/`reopen_count`/`miss_streak`/
`hit_streak`/`last_hit_at`，**不触碰 `acknowledged_at`、`acknowledged_by`**：

- 先 ACK 后条件消失 → 记录 `CLOSED`，确认人和确认时间仍在，可用于"谁在什么时候确认过"；
- 条件回来被自动复开 → 历史确认证据保留，不要求重新 ACK 才留痕（是否要求重新 ACK 属产品策略，
  见下文）；
- 人工关闭（`MANUAL`）覆盖 `closed_reason`，但同样不清空确认证据。

## 仍需产品确认的策略

1. `ACK` 后条件消失再回来，复开后是否需要**再次**确认（当前实现：复开为 `OPEN`，保留原 ACK 证据）；
2. 复开是否应通知/重新指派负责人（当前只改状态，不触发通知）；
3. 确认轮数与安全阀比例的租户级差异（当前是部署级配置）；
4. 告警保留期与归档（`WITHDRAWN` 语义不适用于告警，告警只做状态收敛）。

## 迁移与回滚

- `migrations/000027_alert_lifecycle.sql`：新增 `alert_item.closed_reason` 与
  `idx_alert_rule_status`；不改写既有状态，已有行 `closed_reason=''`（视为人工/历史关闭）。
- `migrations/000028_alert_lifecycle_phase.sql`：新增 `miss_streak`/`hit_streak`/`last_hit_at`/
  `reopen_count`/`acknowledged_at`/`acknowledged_by` 与 `idx_alert_rule_state`；只加列，不回填。
- 回退首选：把 `ALERT_AUTO_RESOLVE` 置回 `false`（立即恢复旧行为；新增列无破坏性读取依赖）；
  再把 `ALERT_CONFIRM_ROUNDS`/`ALERT_REOPEN_POLICY`/`ALERT_CLOSE_GUARD_RATIO` 恢复默认。
  已产生的 `AUTO_CONDITION_CLEARED` 记录可保留（与 `AUTO_WINDOW_EXIT` 同属自动原因）。
- 列级回滚 DDL 只作为迁移文件末尾的人工回滚注释提供，正常发布流程不执行。

## 验证

- `internal/alertworker/lifecycle_test.go`：自动收敛、重开、人工关闭保持、租户/规则隔离、
  默认关闭与显式启用的调用时序；确认轮数去抖（`TestReconcileAlertsRequiresConfirmRoundsBeforeAutoConditionCleared`）、
  兼容 `AUTO_WINDOW_EXIT`、`manual` 复开策略、空命中集合不动作。
- `internal/alertworker/worker_test.go`：四个环境变量的默认值、合法取值与非法取值（启动失败），
  以及安全阀拒绝关闭（`TestWorkerCloseGuardSkipsWhenNoActiveHits` /
  `TestWorkerCloseGuardSkipsWhenActiveShrinksBelowRatio`）。
- `internal/modules/alerts/infrastructure/closed_reason_test.go`、
  `ack_evidence_test.go`：人工关闭写入 `MANUAL`、ACK 写确认证据且不被自动收敛清除。
