-- 000001_init_aggregation_schema.sql
-- 聚合库首批 Schema（设计方案 §5.2）：同步元数据 + 维表镜像 + 合同看板首批聚合事实表
-- 依据（已核实源库结构）：
--   con_contract      contract_management/migrations/000001+000006+000007+000012+000015
--   crm_customers     customer_and_opportunity/migrations/000002
--   crm_opportunities customer_and_opportunity/migrations/000003
--   pm_project        project_management/migrations/000001+000004
-- 口径唯一来源：Data-analysis/数据看板与统计分析子系统·指标字典（模板）.md §5.1
-- 注意：不镜像脱敏/合规列（如 crm_customers.unified_credit_code_cipher/hmac，NG6）

-- 同步元数据（aggregation-worker 状态）
CREATE TABLE IF NOT EXISTS sync_source (
  id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  subsystem_code    VARCHAR(64)  NOT NULL,
  db_host           VARCHAR(255) NOT NULL,
  db_schema         VARCHAR(64)  NOT NULL,
  read_account_ref  VARCHAR(128) NOT NULL COMMENT '引用部署 Secret 键名，不存明文凭据',
  enabled           TINYINT(1)   NOT NULL DEFAULT 1,
  last_watermark    DATETIME(3)  NULL,
  last_run_at       DATETIME(3)  NULL,
  last_status       VARCHAR(16)  NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING/RUNNING/SUCCESS/FAILED',
  last_error        TEXT         NULL,
  created_at        DATETIME(3)  NOT NULL,
  updated_at        DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_sync_source (subsystem_code, db_schema)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 指标字典版本（聚合表与口径绑定）
CREATE TABLE IF NOT EXISTS metric_dict_version (
  version     VARCHAR(32) NOT NULL,
  released_at DATETIME(3) NOT NULL,
  checksum    CHAR(64)    NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 维表镜像：客户（源 crm_customers；区域维度用于字典 1.3）
CREATE TABLE IF NOT EXISTS dim_customer (
  tenant_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  customer_id     BIGINT UNSIGNED NOT NULL COMMENT 'crm_customers.id',
  customer_no     VARCHAR(32)  NOT NULL,
  customer_name   VARCHAR(200) NOT NULL,
  customer_type   VARCHAR(64)  NOT NULL,
  industry        VARCHAR(64)  NOT NULL,
  region          VARCHAR(64)  NOT NULL,
  status          VARCHAR(32)  NOT NULL,
  owner_user_id   VARCHAR(64)  NOT NULL,
  owner_org_id    VARCHAR(64)  NOT NULL DEFAULT '',
  deleted_at      DATETIME(3)  NULL,
  updated_at      DATETIME(3)  NOT NULL,
  PRIMARY KEY (tenant_id, customer_id),
  KEY idx_dim_customer_region (tenant_id, region),
  KEY idx_dim_customer_org (tenant_id, owner_org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='不镜像 unified_credit_code_*（加密/脱敏列，NG6）';

-- 维表镜像：商机（源 crm_opportunities；漏斗/转化率用于字典 1.4）
CREATE TABLE IF NOT EXISTS dim_opportunity (
  tenant_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  opportunity_id     BIGINT UNSIGNED NOT NULL COMMENT 'crm_opportunities.id',
  opportunity_no     VARCHAR(32)  NOT NULL,
  name               VARCHAR(200) NOT NULL,
  customer_id        BIGINT UNSIGNED NOT NULL,
  type               VARCHAR(64)  NOT NULL,
  source             VARCHAR(64)  NOT NULL,
  expected_amount    DECIMAL(18,2) NOT NULL,
  expected_sign_date DATE         NOT NULL,
  owner_user_id      VARCHAR(64)  NOT NULL,
  owner_org_id       VARCHAR(64)  NOT NULL DEFAULT '',
  current_stage      VARCHAR(32)  NOT NULL,
  opp_status         VARCHAR(32)  NOT NULL,
  contract_ref       VARCHAR(64)  NULL,
  lost_reason        VARCHAR(64)  NULL,
  stage_changed_at   DATETIME(3)  NOT NULL,
  deleted_at         DATETIME(3)  NULL,
  updated_at         DATETIME(3)  NOT NULL,
  PRIMARY KEY (tenant_id, opportunity_id),
  KEY idx_dim_opportunity_org (tenant_id, owner_org_id),
  KEY idx_dim_opportunity_status (tenant_id, opp_status, current_stage)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 维表镜像：合同（源 con_contract + 000006/000007/000012/000015；字典 1.1/1.2/1.5）
CREATE TABLE IF NOT EXISTS dim_contract (
  tenant_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  contract_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'con_contract.id',
  contract_number   VARCHAR(64)  NOT NULL,
  title             VARCHAR(255) NOT NULL,
  contract_type     VARCHAR(64)  NOT NULL,
  service_type      VARCHAR(64)  NOT NULL,
  status            VARCHAR(32)  NOT NULL,
  start_date        DATE         NULL COMMENT '字典口径"生效日"映射待确认（000006）',
  end_date          DATE         NULL,
  crm_customer_id   BIGINT UNSIGNED NULL COMMENT '区域/客户分类经 dim_customer 关联',
  opportunity_id    VARCHAR(64)  NULL COMMENT '与 dim_opportunity.opportunity_no 映射待确认',
  amount_minor      BIGINT       NOT NULL COMMENT '分；含税（字典通用口径：人民币含税）',
  currency          CHAR(3)      NOT NULL DEFAULT 'CNY',
  owner_user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  owner_org_id      VARCHAR(64)  NULL,
  owner_identity_id VARCHAR(128) NULL,
  project_id        VARCHAR(64)  NULL,
  updated_at        DATETIME(3)  NOT NULL,
  PRIMARY KEY (tenant_id, contract_id),
  KEY idx_dim_contract_status (tenant_id, status, start_date),
  KEY idx_dim_contract_org (tenant_id, owner_org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='标准报价/折扣字段：con_contract 无独立列，字典 1.5 数据源待确认（service_items_json 候选）';

-- 维表镜像：项目（源 pm_project + 000004；字典 2.1/2.2/2.6）
CREATE TABLE IF NOT EXISTS dim_project (
  tenant_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  project_id         VARCHAR(32)  NOT NULL COMMENT 'pm_project.id',
  name               VARCHAR(255) NOT NULL,
  customer           VARCHAR(255) NOT NULL,
  contract           VARCHAR(64)  NOT NULL,
  category           VARCHAR(255) NOT NULL DEFAULT '',
  team               VARCHAR(128) NOT NULL DEFAULT '' COMMENT '团队名快照',
  manager            VARCHAR(128) NOT NULL DEFAULT '' COMMENT '经理名快照',
  manager_identity_id VARCHAR(128) NOT NULL DEFAULT '',
  owner_org_id       VARCHAR(64)  NOT NULL DEFAULT '',
  owner_identity_id  VARCHAR(128) NOT NULL DEFAULT '',
  status             VARCHAR(32)  NOT NULL,
  health             VARCHAR(32)  NOT NULL DEFAULT '待确认',
  progress           INT          NOT NULL DEFAULT 0,
  due                VARCHAR(64)  NOT NULL DEFAULT '',
  created_at         DATETIME(3)  NOT NULL,
  updated_at         DATETIME(3)  NOT NULL,
  PRIMARY KEY (tenant_id, project_id),
  KEY idx_dim_project_org (tenant_id, owner_org_id),
  KEY idx_dim_project_status (tenant_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='交付周期（2.2）实际开始日/确认日、项目周期（2.6）归档日：依赖交付事件表与报告子系统，待就绪后扩展';

-- 维表镜像：组织/任职快照（源：平台 owner-directory，scope=owner_directory.read；OQ-D1）
CREATE TABLE IF NOT EXISTS dim_org_user_snapshot (
  tenant_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id        VARCHAR(64)  NOT NULL COMMENT '平台用户 sub/稳定 ID',
  display_name   VARCHAR(255) NOT NULL,
  org_unit_id    VARCHAR(64)  NOT NULL,
  org_unit_name  VARCHAR(255) NOT NULL,
  is_primary     TINYINT(1)   NOT NULL DEFAULT 0,
  snapshot_date  DATE         NOT NULL COMMENT '同步日期（T+1 快照）',
  PRIMARY KEY (tenant_id, user_id, org_unit_id, snapshot_date),
  KEY idx_dim_org_snapshot_org (tenant_id, org_unit_id, snapshot_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='组织树父子层级/成员下钻：平台 owner-directory 当前不支持，ORG_SUBTREE 一期降级方案（OQ-D1/OQ-D6）';

-- 聚合事实表：合同看板签约/转化/折扣（字典 1.1/1.2/1.3/1.4/1.5；夜间预计算）
CREATE TABLE IF NOT EXISTS fct_contract_signing (
  tenant_id            CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  period_month         CHAR(7)  NOT NULL COMMENT 'YYYY-MM（字典时间基准：业务发生日）',
  region               VARCHAR(64)  NOT NULL DEFAULT '未知' COMMENT '区域缺失单列未知（字典 1.3）',
  customer_category    VARCHAR(64)  NOT NULL DEFAULT '未分类',
  sales_director_id    VARCHAR(128) NOT NULL DEFAULT '',
  sales_id             VARCHAR(128) NOT NULL DEFAULT '',
  sign_amount_minor    BIGINT       NOT NULL DEFAULT 0 COMMENT '分；Σ(状态=已生效 且 生效日∈周期)金额；框架合同计首签',
  contract_count       INT          NOT NULL DEFAULT 0,
  opportunity_count    INT          NOT NULL DEFAULT 0 COMMENT '转化率分母（进入商机阶段，纯询价不计）',
  won_contract_count   INT          NOT NULL DEFAULT 0 COMMENT '转化率分子（中标且成合同）',
  discount_rate_bucket VARCHAR(16)  NOT NULL DEFAULT '未知' COMMENT '字典 1.5 区间 0%/1-5%/5-10%/>10%；数据源待确认；主键不可为 NULL',
  metric_dict_version  VARCHAR(32)  NOT NULL,
  PRIMARY KEY (tenant_id, period_month, region, customer_category, sales_director_id, sales_id, discount_rate_bucket)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='口径严格按指标字典；金额统一分存储避免浮点误差；展示层万元换算';
