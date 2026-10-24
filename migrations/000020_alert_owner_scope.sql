-- 预警中心按组织/团队范围过滤（BI 数据范围修复）。
-- 旧数据没有 owner_org_id 时默认为空串：只有 TENANT 范围可见，ORG/TEAM 必须显式匹配，
-- 避免历史预警因缺少归属信息被错误扩大可见范围。
ALTER TABLE alert_item
  ADD COLUMN owner_org_id VARCHAR(64) NOT NULL DEFAULT '' AFTER target_ref,
  ADD KEY idx_alert_scope (tenant_id, owner_org_id, status, severity, due_date);

-- 合同到期预警的 target_ref 是 contract_id；用既有聚合维表回填归属，避免升级后
-- 组织/团队用户看不到仍在 OPEN/ACK 的历史预警。其他类型没有可靠归属时保持空串，
-- 仅 TENANT 范围可见。
UPDATE alert_item AS a
JOIN dim_contract AS c
  ON c.tenant_id = a.tenant_id AND c.contract_id = a.target_ref
SET a.owner_org_id = c.owner_org_id
WHERE a.alert_type = 'CONTRACT_EXPIRY' AND a.owner_org_id = '';
