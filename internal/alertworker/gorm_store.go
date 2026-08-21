package alertworker

import (
	"context"
	"time"

	"gorm.io/gorm"
)

type gormRule struct {
	RuleCode      string `gorm:"column:rule_code"`
	Severity      string `gorm:"column:severity"`
	ThresholdJSON []byte `gorm:"column:threshold_json"`
}

// GormStore 使用版本化迁移创建的 alert_rule、alert_item 和 dim_contract 表。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) ListEnabledRules(ctx context.Context) ([]Rule, error) {
	var rows []gormRule
	if err := s.db.WithContext(ctx).
		Table("alert_rule").
		Select("rule_code, severity, threshold_json").
		Where("enabled = ?", true).
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
