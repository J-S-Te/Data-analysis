# 手工同步任务租约与旧任务恢复

管理员触发的任务由 `aggregation-worker` 领取。迁移15新增持久化
`lease_owner`、`lease_generation`、`lease_expires_at` 和旧任务恢复审计表。
升级必须先执行版本化迁移，不能使用旧表结构启动新 Worker。

新任务使用聚合数据库 UTC 时钟；默认租约2分钟、每30秒续租，单任务最多30分钟。
正常长任务持续续租，不能根据 `started_at` 的年龄被抢走。Worker 异常退出后，
其他实例仅通过过期租约与 generation 的 CAS 领取。旧 owner 不能续租、完成任务或
发布聚合结果：聚合写事务与任务行锁、owner/generation/期限检查一起执行。
已提交的维表 upsert 或快照会在恢复时幂等重放，不把首次完成之前的中间批次解释成
全任务成功。授权到期或正常取消将未完成任务回队，保留活动槽；续签后重新领取。

Worker 可配置 `--sync-job-lease`、`--sync-job-heartbeat`、`--sync-job-timeout`，均为
Go duration；lease 必须至少是 heartbeat 的3倍，值必须为正。数据库不可访问、
心跳失败或失去租约均停止当前任务，不能在不确定拥有权时继续写。

旧 RUNNING 任务没有 owner 或心跳，无法判断是否仍在执行。迁移不重置它们，
运行时也不会把它们猜成过期任务。恢复须由部署维护人员先受控停止完整 Compose
项目中的 `data-analysis-aggregation-worker`，核实多实例全部停止，并使用主机维护
命令按租户和明确任务 ID 操作。不要启动新旧 Worker 混合消费。

`sync-job-recover-legacy` 默认仅预览。它随 production-migrate 镜像交付在
`/app/sync-job-recover-legacy`；实际提交在部署主机以 root 执行已审核镜像中的该
Linux 二进制（可从新建、尚未启动的受控临时容器复制，复制后删除本轮临时容器）。
不向业务容器添加 Docker socket，也不在不含 Docker CLI 的迁移容器内直接提交。
聚合 DSN 由 `DASHBOARD_MYSQL_DSN` 安全注入，不传到参数或日志。

预览需要 `--tenant-id TENANT --job-ids ID1,ID2`。仅列出状态仍为 RUNNING、
generation=0、owner/expiry 均为空的旧任务；不是自动扫描所有任务后批量重置。

提交另需：

- `--apply --operator OPERATOR`：明确操作者，写入恢复审计。
- `--deployment-root /opt/unified-identity-platform`：复用 `runtime/.deploy.lock`，
  非规范路径、符号链接锁或其他部署操作占用锁时拒绝。
- `--compose-project uip`：使用现场已核实的 Compose 项目名。
- `--expected-workers CONTAINER_ID@sha256:IMAGE_ID,...`：所有 Worker 的完整不可变
  容器与镜像身份，从受控停止结果核实；不接受只有镜像标签、部分容器或空边界。

命令绑定本地主机 `/var/run/docker.sock`，枚举该项目该服务的全部容器，拒绝任何
运行、暂停、重启中容器及数量、身份或镜像变化。持有部署锁期间，在恢复事务开始
和提交前重复核对停止证据；任何变化整体回滚。已有 owner 的现代任务、跨租户、
状态/generation 不匹配、重复 ID、同源活动槽冲突都拒绝，不擅自合并或删除任务。
成功后将选定旧任务回队，并原子写入 `sync_job_recovery_audit`，记录操作者、租户、
任务、时间与停止证据 SHA-256。

本命令仅支持当前受控 Compose 部署边界。手工进程、跨主机 Worker 或不属于该
项目的任务消费者无法由本机完整证明停止，不能据此使用提交命令。先通过项目
既有部署维护流程解决消费者归属与停止证明，不通过数据库直接改状态绕过保护。
维护结束后正常启动升级后的 Worker；许可仍按签名期限与原权限执行。
