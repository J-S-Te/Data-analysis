package alertworker

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	if err := s.validateTenant(); err != nil {
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

func (s *GormStore) validateTenant() error {
	if strings.TrimSpace(s.tenantID) == "" {
		return errors.New("alert worker tenant_id is required")
	}
	return nil
}

func (s *GormStore) FindContractExpiryCandidates(
	ctx context.Context,
	evaluatedAt time.Time,
	days int,
) ([]ContractExpiryCandidate, error) {
	if err := s.validateTenant(); err != nil {
		return nil, err
	}
	var candidates []ContractExpiryCandidate
	err := s.db.WithContext(ctx).
		Table("dim_contract").
		Select(`tenant_id, contract_id AS target_ref,
CONCAT('合同「', title, '」将在 ', DATE_FORMAT(end_date, '%Y-%m-%d'), ' 到期') AS title,
end_date AS due_date`).
		Where("end_date IS NOT NULL").
		Where("tenant_id = ?", s.tenantID).
		Where("end_date >= DATE(?)", evaluatedAt).
		Where("end_date <= DATE_ADD(DATE(?), INTERVAL ? DAY)", evaluatedAt, days).
		Where("status NOT IN ?", []string{"COMPLETED", "ARCHIVED", "TERMINATED"}).
		Order("tenant_id, end_date, contract_id").
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		if candidate.TenantID != s.tenantID {
			return nil, errors.New("contract expiry candidate tenant does not match worker tenant")
		}
	}
	return candidates, nil
}

func (s *GormStore) UpsertAlerts(ctx context.Context, alerts []Alert) error {
	if err := s.validateTenant(); err != nil {
		return err
	}
	if len(alerts) == 0 {
		return nil
	}
	ruleCodes := make(map[string]bool)
	for _, alert := range alerts {
		if alert.TenantID != s.tenantID {
			return errors.New("alert tenant does not match worker tenant")
		}
		ruleCodes[alert.RuleCode] = false
	}
	codes := make([]string, 0, len(ruleCodes))
	for code := range ruleCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与规则停用/删除共用规则行锁，避免候选查询之后继续写入失效规则。
		for _, code := range codes {
			var row struct{ Enabled bool }
			err := tx.Table("alert_rule").Select("enabled").
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("tenant_id = ? AND rule_code = ?", s.tenantID, code).
				Take(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			ruleCodes[code] = row.Enabled
		}
		for _, alert := range alerts {
			if !ruleCodes[alert.RuleCode] {
				continue
			}
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
