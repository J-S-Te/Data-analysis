-- 000031_sync_run.sql
-- 聚合轮次起止记录（BIZ-DATA-08 残项 B）
--
-- 目的：滚动升级窗口内可能有两个 aggregation-worker 版本同时跑轮次。来源级
-- GET_LOCK 只保证“同一租户/来源同时只有一个执行者”，挡不住两轮混代次地写派生
-- 预计算（fct_contract_signing / fct_opportunity_conversion）。本表让每轮起止可查，
-- 并在 precompute 之前检测“同租户/同子系统存在未结束且开始更早的轮次”，据此跳过
-- 派生预计算（事实数据不损失）并 WARN；策略由 AGG_RUN_OVERLAP_GUARD=warn|enforce 控制。
--
-- 说明：
--   finished_at 为空表示该轮仍在进行（或进程崩溃后未收尾）。读取侧只用
--   run_started_at >= 陈旧阈值 的行，避免崩溃残留永久冻结 precompute。
--   本表只增不删（审计/排障用），不保存任何凭据或 DSN。
--
-- 回滚：回退镜像即可；本表可留（旧版本不读取）。

CREATE TABLE IF NOT EXISTS sync_run (
  tenant_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  subsystem_code  VARCHAR(64) NOT NULL COMMENT '轮次子系统：aggregation = 定时全量同步轮次',
  run_started_at  DATETIME(3) NOT NULL COMMENT 'UTC 墙钟，轮次开始时间',
  owner           VARCHAR(128) NOT NULL DEFAULT '' COMMENT '实例标识（hostname:pid），不含凭据',
  finished_at     DATETIME(3) NULL COMMENT 'UTC 墙钟；NULL 表示未结束/异常残留',
  PRIMARY KEY (tenant_id, subsystem_code, run_started_at),
  KEY idx_sync_run_open (tenant_id, subsystem_code, finished_at, run_started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='aggregation 轮次起止；重叠检测只读未结束且未超过陈旧阈值的行';

-- 人工回滚（受控执行，需要 DBA 批准）：
-- DROP TABLE sync_run;
