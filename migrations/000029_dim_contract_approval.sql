-- 000029_dim_contract_approval.sql
-- 合同“审批中”权威投影（BIZ-DATA-06 口径修复）
--
-- 背景：看板的 ORG/TEAM/SELF 分支过去用 dim_contract.status='pending' 近似“审批中”。
-- 源状态只覆盖“新建审批中”，kind=status_change 的变更审批在合同仍为 active 时进行，
-- 因此会漏计；反向地 pending 也不保证存在在途审批实例。合同权威口径是
-- con_approval_instance.status='running'（contract_management/migrations/000001_contract_workflow.sql）。
--
-- 本迁移只新增镜像表，不改 dim_contract 结构与既有守卫：
--     dim_contract 的版本防护列是 con_contract.updated_at；审批状态变化不会改合同行，
--     若把 in_approval 之类标志塞进 dim_contract，会被合同行守卫直接拒绝更新。
-- 因此审批投影必须自带来源 updated_at（con_approval_instance.updated_at）作为守卫列，
-- 由 internal/aggregation 的 syncContractApproval 写入（guardedDimUpsert 复用）。
--
-- 全租户（TENANT）口径继续读合同机器接口落地的 api_contract_dashboard，本表不双写。
-- 回滚：DASHBOARD_APPROVAL_SOURCE 切回 status；本表可留（无读取依赖时不影响现网）。

CREATE TABLE IF NOT EXISTS dim_contract_approval (
  tenant_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  contract_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'con_approval_instance.contract_id',
  approval_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'con_approval_instance.id',
  kind         VARCHAR(32) NOT NULL COMMENT 'contract_approval / status_change',
  status       VARCHAR(32) NOT NULL COMMENT 'running/approved/rejected/withdrawn/expired/failed（源词表）',
  started_at   DATETIME(3) NOT NULL COMMENT 'con_approval_instance.created_at',
  completed_at DATETIME(3) NULL,
  updated_at   DATETIME(3) NOT NULL COMMENT '来源守卫列：con_approval_instance.updated_at',
  PRIMARY KEY (tenant_id, contract_id, approval_id),
  KEY idx_dim_contract_approval_running (tenant_id, status, contract_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='con_approval_instance 只读投影；看板审批中用 EXISTS(status=running) 判定';

-- 人工回滚（受控执行，需要 DBA 批准）：
-- DROP TABLE dim_contract_approval;
