package alertworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"gorm.io/gorm"
)

type gormRule struct {
	RuleCode      string `gorm:"column:rule_code"`
	Severity      string `gorm:"column:severity"`
	ThresholdJSON []byte `gorm:"column:threshold_json"`
}

// GormStore 使用版本化迁移创建的 alert_rule、alert_item 和 dim_contract 表。
type GormStore struct {
	db       *gorm.DB
	tenantID string
}

// NewGormStore 构造只读取指定租户规则的预警仓储。
func NewGormStore(db *gorm.DB, tenantID string) *GormStore {
	return &GormStore{db: db, tenantID: tenantID}
}

func (s *GormStore) ListEnabledRules(ctx context.Context) ([]Rule, error) {
	if err := s.ensureDefaultRule(ctx); err != nil {
		return nil, err
	}
	var rows []gormRule
	if err := s.db.WithContext(ctx).
		Table("alert_rule").
		Select("rule_code, severity, threshold_json").
		Where("tenant_id = ? AND enabled = ?", s.tenantID, true).
		Order("rule_code").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, Rule{
			Code:          row.RuleCode,
			Severity:      row.Severity,
			ThresholdJSON: row.ThresholdJSON,
		})
	}
	return rules, nil
}

// ensureDefaultRule 为首次运行的租户写入可执行的最小默认规则，不覆盖已由管理员维护的规则。
func (s *GormStore) ensureDefaultRule(ctx context.Context) error {
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte("data-analysis:alert-rule:" + s.tenantID + ":CONTRACT_EXPIRY"))
	id := hex.EncodeToString(sum[:])[:26]
	return s.db.WithContext(ctx).Exec(`INSERT INTO alert_rule
(tenant_id, id, rule_code, name, source_fct, severity, enabled, threshold_json, created_at, updated_at)
VALUES (?, ?, 'CONTRACT_EXPIRY', '合同到期提醒', 'dim_contract', 'HIGH', 1, '{"days":30}', ?, ?)
ON DUPLICATE KEY UPDATE tenant_id = tenant_id`, s.tenantID, id, now, now).Error
}

func (s *GormStore) FindContractExpiryCandidates(
	ctx context.Context,
	evaluatedAt time.Time,
	days int,
) ([]ContractExpiryCandidate, error) {
	var candidates []ContractExpiryCandidate
	err := s.db.WithContext(ctx).
		Table("dim_contract").
		Select(`tenant_id, contract_id AS target_ref,
CONCAT('合同「', title, '」将在 ', DATE_FORMAT(end_date, '%Y-%m-%d'), ' 到期') AS title,
end_date AS due_date`).
		Where("end_date IS NOT NULL").
		Where("end_date >= DATE(?)", evaluatedAt).
		Where("end_date <= DATE_ADD(DATE(?), INTERVAL ? DAY)", evaluatedAt, days).
		Where("status NOT IN ?", []string{"COMPLETED", "ARCHIVED", "TERMINATED"}).
		Order("tenant_id, end_date, contract_id").
		Find(&candidates).Error
	return candidates, err
}

func (s *GormStore) UpsertAlerts(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, alert := range alerts {
			if err := tx.Exec(`INSERT INTO alert_item
(id, tenant_id, alert_type, rule_code, severity, target_ref, title, due_date, status, closed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'OPEN', NULL, ?, ?)
ON DUPLICATE KEY UPDATE
rule_code = VALUES(rule_code),
severity = VALUES(severity),
title = VALUES(title),
due_date = VALUES(due_date),
updated_at = VALUES(updated_at)`,
				alert.ID,
				alert.TenantID,
				alert.AlertType,
				alert.RuleCode,
				alert.Severity,
				alert.TargetRef,
				alert.Title,
				alert.DueDate,
				alert.EvaluatedAt,
				alert.EvaluatedAt,
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
