-- 合同草稿允许在审批完成前暂不生成合同编号，与合同源库 000007 保持一致。
ALTER TABLE dim_contract
  MODIFY COLUMN contract_number VARCHAR(64) NULL;
