// Package infrastructure 提供管理模块本地的 GORM 持久化适配器。
package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
)

type syncSourceRecord struct {
	TenantID      string     `gorm:"column:tenant_id"`
	ID            string     `gorm:"column:id;primaryKey"`
	SubsystemCode string     `gorm:"column:subsystem_code"`
	DBHost        string     `gorm:"column:db_host"`
	DBSchema      string     `gorm:"column:db_schema"`
	Enabled       bool       `gorm:"column:enabled"`
	LastStatus    string     `gorm:"column:last_status"`
	LastRunAt     *time.Time `gorm:"column:last_run_at"`
	LastError     *string    `gorm:"column:last_error"`
}

func (syncSourceRecord) TableName() string { return "sync_source" }

// toDomain 用途：投影数据源状态，内部诊断只转换为安全公开分类。
// 参数：record 为当前租户的数据源持久化记录。
// 返回值：公开状态回执；不改变数据库诊断、水位或任务状态。
func (record syncSourceRecord) toDomain() domain.SyncSource {
	value := domain.SyncSource{TenantID: record.TenantID, ID: record.ID, SubsystemCode: record.SubsystemCode, DBHost: record.DBHost, DBSchema: record.DBSchema,
		Enabled: record.Enabled, LastStatus: record.LastStatus, LastRunAt: record.LastRunAt, LastError: record.LastError}
	if record.LastError != nil {
		code, message := domain.PublicSyncFailure(*record.LastError)
		value.LastErrorCode = code
		if code != "" {
			value.LastError = &message
		}
	}
	return value
}

type syncJobRecord struct {
	TenantID      string    `gorm:"column:tenant_id"`
	ID            string    `gorm:"column:id;primaryKey"`
	SourceID      string    `gorm:"column:source_id"`
	SubsystemCode string    `gorm:"column:subsystem_code"`
	Status        string    `gorm:"column:status"`
	RequestedAt   time.Time `gorm:"column:requested_at"`
	ActiveKey     *string   `gorm:"column:active_key"`
}

func (syncJobRecord) TableName() string { return "sync_job" }

type alertRuleRecord struct {
	Version       int64     `gorm:"column:version;->"`
	TenantID      string    `gorm:"column:tenant_id"`
	ID            string    `gorm:"column:id;primaryKey"`
	RuleCode      string    `gorm:"column:rule_code"`
	Name          string    `gorm:"column:name"`
	SourceFCT     string    `gorm:"column:source_fct"`
	Severity      string    `gorm:"column:severity"`
	Enabled       bool      `gorm:"column:enabled"`
	ThresholdJSON *string   `gorm:"column:threshold_json"`
	ScopeJSON     *string   `gorm:"column:scope_json"`
	EffectiveFrom *string   `gorm:"column:effective_from"`
	EffectiveTo   *string   `gorm:"column:effective_to"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (alertRuleRecord) TableName() string { return "alert_rule" }

func (record alertRuleRecord) toDomain() domain.AlertRule {
	return domain.AlertRule{TenantID: record.TenantID, ID: record.ID, RuleCode: record.RuleCode, Name: record.Name, SourceFCT: record.SourceFCT,
		Severity: record.Severity, Enabled: record.Enabled, ThresholdJSON: record.ThresholdJSON, ScopeJSON: record.ScopeJSON,
		EffectiveFrom: record.EffectiveFrom, EffectiveTo: record.EffectiveTo, UpdatedBy: record.UpdatedBy,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Version: record.Version}
}

func alertRuleRecordFromDomain(rule domain.AlertRule) alertRuleRecord {
	return alertRuleRecord{TenantID: rule.TenantID, ID: rule.ID, RuleCode: rule.RuleCode, Name: rule.Name, SourceFCT: rule.SourceFCT,
		Severity: rule.Severity, Enabled: rule.Enabled, ThresholdJSON: rule.ThresholdJSON, ScopeJSON: rule.ScopeJSON,
		EffectiveFrom: rule.EffectiveFrom, EffectiveTo: rule.EffectiveTo, UpdatedBy: rule.UpdatedBy,
		CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt}
}

// GORMRepository 实现数据看板管理模块自己的仓储端口。
type GORMRepository struct{ db *gorm.DB }

// NewGORMRepository 构造管理模块的 GORM 仓储。
func NewGORMRepository(db *gorm.DB) *GORMRepository { return &GORMRepository{db: db} }

// ListSources 返回按子系统编码排序的数据源。
func (repository *GORMRepository) ListSources(ctx context.Context, tenantID string) ([]domain.SyncSource, error) {
	var records []syncSourceRecord
	if err := repository.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("subsystem_code, id").Find(&records).Error; err != nil {
		return nil, err
	}
	sources := make([]domain.SyncSource, 0, len(records))
	for _, record := range records {
		sources = append(sources, record.toDomain())
	}
	return sources, nil
}

// FindSource 返回指定数据源；未命中时 found 为 false。
func (repository *GORMRepository) FindSource(ctx context.Context, tenantID, id string) (domain.SyncSource, bool, error) {
	var record syncSourceRecord
	err := repository.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&record).Error
	if err == gorm.ErrRecordNotFound {
		return domain.SyncSource{}, false, nil
	}
	if err != nil {
		return domain.SyncSource{}, false, err
	}
	return record.toDomain(), true, nil
}

// CreateSyncJob 持久化一个等待 Worker 消费的同步任务。
func (repository *GORMRepository) CreateSyncJob(ctx context.Context, job domain.SyncJob) (bool, error) {
	activeKey := "ACTIVE"
	record := syncJobRecord{TenantID: job.TenantID, ID: job.ID, SourceID: job.SourceID, SubsystemCode: job.SubsystemCode, Status: job.Status, RequestedAt: job.RequestedAt, ActiveKey: &activeKey}
	// AUD-IP-2026 / 批次 2：手工同步任务与审计事件同事务；重复请求不产生审计。
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return auditoutbox.Append(tx, auditoutbox.Entry{
			TenantID: job.TenantID, Action: "DATA_ANALYSIS:SYNC_JOB_CREATE",
			ResourceType: "sync_job", ResourceID: job.ID, Result: "SUCCESS",
		})
	})
	if err != nil {
		if isDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListAlertRules 返回指定租户内按规则编码排序的预警规则。
func (repository *GORMRepository) ListAlertRules(ctx context.Context, tenantID string) ([]domain.AlertRule, error) {
	var records []alertRuleRecord
	if err := repository.db.WithContext(ctx).Model(&alertRuleRecord{}).
		Select("alert_rule.*, COALESCE((SELECT MAX(v.version) FROM alert_rule_version v WHERE v.tenant_id = alert_rule.tenant_id AND v.rule_id = alert_rule.id), 1) AS version").
		Where("tenant_id = ?", tenantID).Order("rule_code, id").Find(&records).Error; err != nil {
		return nil, err
	}
	rules := make([]domain.AlertRule, 0, len(records))
	for _, record := range records {
		rules = append(rules, record.toDomain())
	}
	return rules, nil
}

// ReplaceAlertRules 保存指定租户的规则；删除只能通过历史保护的显式删除接口。
func (repository *GORMRepository) ReplaceAlertRules(ctx context.Context, tenantID string, rules []domain.AlertRule) error {
	return repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, rule := range rules {
			var existing alertRuleRecord
			found := false
			if rule.ID != "" {
				err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					Where("tenant_id = ? AND id = ?", tenantID, rule.ID).First(&existing).Error
				if errors.Is(err, gorm.ErrRecordNotFound) {
					if rule.Version != 0 {
						return application.ErrRuleVersionConflict
					}
					found = false
				} else if err != nil {
					return err
				} else {
					if existing.RuleCode != rule.RuleCode {
						return application.ErrRuleVersionConflict
					}
					found = true
				}
			}
			var latest int64
			if err := tx.Table("alert_rule_version").Where("tenant_id = ? AND rule_id = ?", tenantID, rule.ID).
				Select("COALESCE(MAX(version), 0)").Scan(&latest).Error; err != nil {
				return err
			}
			if found {
				if latest == 0 {
					latest = 1
				}
				if rule.Version != latest {
					return application.ErrRuleVersionConflict
				}
				rule.CreatedAt = existing.CreatedAt
			} else if rule.Version != 0 {
				return application.ErrRuleVersionConflict
			}
			rule.Version = latest + 1
			record := alertRuleRecordFromDomain(rule)
			if found {
				if err := tx.Model(&alertRuleRecord{}).Where("tenant_id = ? AND id = ?", tenantID, rule.ID).
					Updates(map[string]any{"name": record.Name, "source_fct": record.SourceFCT, "severity": record.Severity, "enabled": record.Enabled, "threshold_json": record.ThresholdJSON, "scope_json": record.ScopeJSON, "effective_from": record.EffectiveFrom, "effective_to": record.EffectiveTo, "updated_by": record.UpdatedBy, "updated_at": record.UpdatedAt}).Error; err != nil {
					return err
				}
			} else if err := tx.Create(&record).Error; err != nil {
				if isDuplicateKeyError(err) {
					return application.ErrRuleVersionConflict
				}
				return err
			}
			snapshot, err := json.Marshal(rule)
			if err != nil {
				return err
			}
			id := defaultAlertRuleVersionID(tenantID, rule.ID, rule.Version)
			if err := tx.Exec(`INSERT INTO alert_rule_version
(id, tenant_id, rule_id, version, rule_snapshot, changed_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, tenantID, rule.ID, rule.Version, string(snapshot), rule.UpdatedBy, rule.UpdatedAt).Error; err != nil {
				return err
			}
			// 规则变更与审计事件同事务，平台不可用时由投递器重试。
			if err := auditoutbox.Append(tx, auditoutbox.Entry{
				TenantID: tenantID, ActorID: derefActor(rule.UpdatedBy), Action: "DATA_ANALYSIS:ALERT_RULE_SAVE",
				ResourceType: "alert_rule", ResourceID: rule.ID, Result: "SUCCESS",
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// PreviewContractExpiry 只读计算合同到期规则命中数量与样例，不写预警项。
// 参数：tenantID 为可信租户；days 为到期前后窗口；scope 为可选范围；
//
//	evaluatedAt 为窗口基准日；limit 为样例上限（最多 20）。
//
// 返回值：命中数量、样例和截断标记；查询失败返回错误。
func (repository *GORMRepository) PreviewContractExpiry(ctx context.Context, tenantID string, days int, scope domain.AlertRuleScope, evaluatedAt time.Time, limit int) (domain.AlertPreviewResult, error) {
	if strings.TrimSpace(tenantID) == "" || days < 1 || days > 3650 {
		return domain.AlertPreviewResult{}, application.ErrRulePayloadInvalid
	}
	if limit < 1 || limit > 20 {
		limit = 20
	}
	base := func() *gorm.DB {
		query := repository.db.WithContext(ctx).
			Table("dim_contract AS c").
			Joins("LEFT JOIN dim_customer AS dc ON dc.tenant_id = c.tenant_id AND dc.customer_id = c.crm_customer_id").
			Where("c.tenant_id = ? AND c.origin_kind = 'NORMAL' AND c.end_date IS NOT NULL", tenantID).
			Where("c.end_date >= DATE_SUB(DATE(?), INTERVAL ? DAY)", evaluatedAt, days).
			Where("c.end_date <= DATE_ADD(DATE(?), INTERVAL ? DAY)", evaluatedAt, days).
			Where("c.status NOT IN ?", []string{"COMPLETED", "ARCHIVED", "TERMINATED"})
		if len(scope.OwnerOrgIDs) > 0 {
			query = query.Where("c.owner_org_id IN ?", scope.OwnerOrgIDs)
		}
		if len(scope.CustomerTypes) > 0 {
			query = query.Where("dc.customer_type IN ?", scope.CustomerTypes)
		}
		if scope.AmountMinMinor != nil {
			query = query.Where("c.amount_minor >= ?", *scope.AmountMinMinor)
		}
		if scope.AmountMaxMinor != nil {
			query = query.Where("c.amount_minor <= ?", *scope.AmountMaxMinor)
		}
		return query
	}
	var count int64
	if err := base().Count(&count).Error; err != nil {
		return domain.AlertPreviewResult{}, err
	}
	var rows []struct {
		ContractID   string
		Title        string
		OwnerOrgID   string
		CustomerType string
		AmountMinor  int64
		DueDate      string
	}
	if err := base().
		Select("c.contract_id, c.title, c.owner_org_id, COALESCE(dc.customer_type, '') AS customer_type, c.amount_minor, CAST(c.end_date AS CHAR) AS due_date").
		Order("c.end_date, c.contract_id").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return domain.AlertPreviewResult{}, err
	}
	items := make([]domain.AlertPreviewItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.AlertPreviewItem{
			ContractID: row.ContractID, Title: row.Title, OwnerOrgID: row.OwnerOrgID,
			CustomerType: row.CustomerType, AmountMinor: row.AmountMinor, DueDate: row.DueDate,
		})
	}
	return domain.AlertPreviewResult{MatchedCount: count, Sample: items, Limited: count > int64(len(items))}, nil
}

// DeleteAlertRule 删除无历史告警的租户规则；已有历史告警时返回保护错误。
func (repository *GORMRepository) DeleteAlertRule(ctx context.Context, tenantID, ruleID string) error {
	return repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rule alertRuleRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, ruleID).First(&rule).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrRuleNotFound
			}
			return err
		}
		var count int64
		if err := tx.Table("alert_item").Where("tenant_id = ? AND rule_code = ?", tenantID, rule.RuleCode).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return application.ErrRuleHasHistory
		}
		return tx.Where("tenant_id = ? AND id = ?", tenantID, ruleID).Delete(&alertRuleRecord{}).Error
	})
}

func isDuplicateKeyError(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(strings.ToLower(err.Error()), "duplicate entry")
}

func defaultAlertRuleVersionID(tenantID, ruleID string, version int64) string {
	sum := sha256.Sum256([]byte("data-analysis:alert-rule-version:" + tenantID + ":" + ruleID + ":" + fmt.Sprint(version)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:26]
}

func derefActor(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
