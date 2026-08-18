-- MySQL 容器首次初始化时只负责创建第三方 Metabase 元数据库。
-- data_analysis 业务表由 local-migrate/production-migrate 统一版本化管理。
CREATE DATABASE IF NOT EXISTS dashboard_metabase
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

GRANT ALL PRIVILEGES ON dashboard_metabase.* TO 'dashboard'@'%';
