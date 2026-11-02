-- 000024_session_idle.sql
-- 会话改为“空闲 30 分钟 + 绝对 8 小时”双期限：记录最近一次显式浏览器活动时间。
-- 该列只由 POST /api/v1/auth/activity 续期；普通业务请求只刷新 last_seen_at。
-- 空值兼容：升级前存量行回填 created_at，代码读取 NULL 时同样按 created_at 处理，
-- 避免升级瞬间把仍在线用户判定为空闲到期而强制重登。

ALTER TABLE da_oidc_sessions
  ADD COLUMN last_interactive_at DATETIME(3) NULL AFTER last_seen_at;

UPDATE da_oidc_sessions
  SET last_interactive_at = created_at
  WHERE last_interactive_at IS NULL;
