-- 000034_project_report_stage.sql
-- 项目级报告阶段（report_stage）进入数据分析（BI 协同设计 2026-11-05）
--
-- 背景：项目管理系统在项目快照流与 /internal/v1/dashboard 中新增可选
-- report_stage（取值：空串或 NONE/RECTIFYING/COMPILING/REVIEWED/ISSUED/ARCHIVED）。
-- 数据分析侧只校验与存储，不重新派生；两条读路径都必须能回答“整改中项目”：
--   1) project_projection_stage / dim_project：完整快照流落库的派生维度；
--   2) api_project_dashboard：项目机器接口看板快照（TENANT 读路径）。
--
-- 兼容性：
--   * 不 bump 快照 schema_version（保持 1）；旧 PM 不发该键，解析侧按“缺失”接受。
--   * 列全部可安全补在既有数据上：历史 dim_project / stage 行 report_stage 落空串
--     （未采集），api_project_dashboard 历史行 report_stage_counts_json 保持 NULL，
--     不回填、不以 0 值冒充“已采集”。
--   * 只加列/加索引，不删除、不改写既有行；DDL 中断但版本未登记时按对象存在性重入。
--
-- 顺序：DA 先迁移、后二进制；新二进制写入 report_stage 之前旧快照仍可发布。

SET @project_projection_stage_report_stage_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='project_projection_stage'
     AND column_name='report_stage')=0,
  'ALTER TABLE project_projection_stage ADD COLUMN report_stage VARCHAR(32) NOT NULL DEFAULT '''' AFTER status',
  'SELECT 1'
);
PREPARE project_projection_stage_report_stage_stmt FROM @project_projection_stage_report_stage_ddl;
EXECUTE project_projection_stage_report_stage_stmt;
DEALLOCATE PREPARE project_projection_stage_report_stage_stmt;

SET @dim_project_report_stage_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='dim_project'
     AND column_name='report_stage')=0,
  'ALTER TABLE dim_project ADD COLUMN report_stage VARCHAR(32) NOT NULL DEFAULT '''' AFTER status',
  'SELECT 1'
);
PREPARE dim_project_report_stage_stmt FROM @dim_project_report_stage_ddl;
EXECUTE dim_project_report_stage_stmt;
DEALLOCATE PREPARE dim_project_report_stage_stmt;

SET @dim_project_report_stage_index_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='dim_project'
     AND index_name='idx_dim_project_report_stage')=0,
  'CREATE INDEX idx_dim_project_report_stage ON dim_project (tenant_id, report_stage)',
  'SELECT 1'
);
PREPARE dim_project_report_stage_index_stmt FROM @dim_project_report_stage_index_ddl;
EXECUTE dim_project_report_stage_index_stmt;
DEALLOCATE PREPARE dim_project_report_stage_index_stmt;

SET @api_project_dashboard_report_stage_counts_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='api_project_dashboard'
     AND column_name='report_stage_counts_json')=0,
  'ALTER TABLE api_project_dashboard ADD COLUMN report_stage_counts_json JSON NULL AFTER status_counts_json',
  'SELECT 1'
);
PREPARE api_project_dashboard_report_stage_counts_stmt FROM @api_project_dashboard_report_stage_counts_ddl;
EXECUTE api_project_dashboard_report_stage_counts_stmt;
DEALLOCATE PREPARE api_project_dashboard_report_stage_counts_stmt;

-- 人工回滚（受控执行，需要 DBA 批准；正常发布流程不执行）：
-- 先回退数据与二进制读路径，再删索引/列；顺序与新增相反。
-- ALTER TABLE api_project_dashboard DROP COLUMN report_stage_counts_json;
-- DROP INDEX idx_dim_project_report_stage ON dim_project;
-- ALTER TABLE dim_project DROP COLUMN report_stage;
-- ALTER TABLE project_projection_stage DROP COLUMN report_stage;
