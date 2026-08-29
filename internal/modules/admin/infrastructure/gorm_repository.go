// Package infrastructure 提供管理模块本地的 GORM 持久化适配器。
package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

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

func (record syncSourceRecord) toDomain() domain.SyncSource {
	return domain.SyncSource{TenantID: record.TenantID, ID: record.ID, SubsystemCode: record.SubsystemCode, DBHost: record.DBHost, DBSchema: record.DBSchema,
		Enabled: record.Enabled, LastStatus: record.LastStatus, LastRunAt: record.LastRunAt, LastError: record.LastError}
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
	TenantID      string    `gorm:"column:tenant_id"`
	ID            string    `gorm:"column:id;primaryKey"`
	RuleCode      string    `gorm:"column:rule_code"`
	Name          string    `gorm:"column:name"`
	SourceFCT     string    `gorm:"column:source_fct"`
	Severity      string    `gorm:"column:severity"`
	Enabled       bool      `gorm:"column:enabled"`
	ThresholdJSON *string   `gorm:"column:threshold_json"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (alertRuleRecord) TableName() string { return "alert_rule" }

func (record alertRuleRecord) toDomain() domain.AlertRule {
	return domain.AlertRule{TenantID: record.TenantID, ID: record.ID, RuleCode: record.RuleCode, Name: record.Name, SourceFCT: record.SourceFCT,
		Severity: record.Severity, Enabled: record.Enabled, ThresholdJSON: record.ThresholdJSON, UpdatedBy: record.UpdatedBy,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Version: 1}
}

func alertRuleRecordFromDomain(rule domain.AlertRule) alertRuleRecord {
	return alertRuleRecord{TenantID: rule.TenantID, ID: rule.ID, RuleCode: rule.RuleCode, Name: rule.Name, SourceFCT: rule.SourceFCT,
		Severity: rule.Severity, Enabled: rule.Enabled, ThresholdJSON: rule.ThresholdJSON, UpdatedBy: rule.UpdatedBy,
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
	if err := repository.db.WithContext(ctx).Create(&record).Error; err != nil {
		if isDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EnsureDefaultAlertRules 为首次访问规则中心的租户建立默认规则。
func (repository *GORMRepository) EnsureDefaultAlertRules(ctx context.Context, tenantID string, now time.Time) error {
	defaultRule := alertRuleRecord{
		TenantID: tenantID, ID: defaultAlertRuleID(tenantID), RuleCode: "CONTRACT_EXPIRY", Name: "合同到期提醒",
		SourceFCT: "dim_contract", Severity: "HIGH", Enabled: true, ThresholdJSON: stringPtr(`{"days":30}`), CreatedAt: now, UpdatedAt: now,
	}
	return repository.db.WithContext(ctx).Exec(`INSERT INTO alert_rule
(tenant_id, id, rule_code, name, source_fct, severity, enabled, threshold_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE tenant_id = tenant_id`,
		defaultRule.TenantID, defaultRule.ID, defaultRule.RuleCode, defaultRule.Name, defaultRule.SourceFCT,
		defaultRule.Severity, defaultRule.Enabled, defaultRule.ThresholdJSON, defaultRule.CreatedAt, defaultRule.UpdatedAt,
	).Error
}

// ListAlertRules 返回指定租户内按规则编码排序的预警规则。
func (repository *GORMRepository) ListAlertRules(ctx context.Context, tenantID string) ([]domain.AlertRule, error) {
	var records []alertRuleRecord
	if err := repository.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("rule_code, id").Find(&records).Error; err != nil {
		return nil, err
	}
	rules := make([]domain.AlertRule, 0, len(records))
	for _, record := range records {
		rules = append(rules, record.toDomain())
	}
	return rules, nil
}

// ReplaceAlertRules 在同一事务中替换指定租户的全部规则，不影响其他租户。
func (repository *GORMRepository) ReplaceAlertRules(ctx context.Context, tenantID string, rules []domain.AlertRule) error {
	return repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ?", tenantID).Delete(&alertRuleRecord{}).Error; err != nil {
			return err
		}
		records := make([]alertRuleRecord, 0, len(rules))
		for _, rule := range rules {
			records = append(records, alertRuleRecordFromDomain(rule))
		}
		if err := tx.Create(&records).Error; err != nil {
			return err
		}
		// 规则表采用整表替换以保持现有 API 语义；版本表保留每次提交的不可变快照。
		for _, rule := range rules {
			var latest int64
			tx.Table("alert_rule_version").Where("tenant_id = ? AND rule_id = ?", tenantID, rule.ID).
				Select("COALESCE(MAX(version), 0)", tenantID, rule.ID).Scan(&latest)
			version := latest + 1
			snapshot, err := json.Marshal(rule)
			if err != nil {
				return err
			}
			id := defaultAlertRuleVersionID(tenantID, rule.ID, version)
			if err := tx.Exec(`INSERT INTO alert_rule_version
(id, tenant_id, rule_id, version, rule_snapshot, changed_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE rule_snapshot = VALUES(rule_snapshot), changed_by = VALUES(changed_by)`,
				id, tenantID, rule.ID, version, string(snapshot), rule.UpdatedBy, rule.UpdatedAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteAlertRule 删除无历史告警的租户规则；已有历史告警时返回保护错误。
func (repository *GORMRepository) DeleteAlertRule(ctx context.Context, tenantID, ruleID string) error {
	var count int64
	if err := repository.db.WithContext(ctx).Table("alert_rule").Where("tenant_id = ? AND id = ?", tenantID, ruleID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return application.ErrRuleNotFound
	}
	if err := repository.db.WithContext(ctx).Table("alert_item").Where("tenant_id = ? AND rule_code = (SELECT rule_code FROM alert_rule WHERE tenant_id = ? AND id = ?)", tenantID, tenantID, ruleID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return application.ErrRuleHasHistory
	}
	return repository.db.WithContext(ctx).Exec("DELETE FROM alert_rule WHERE tenant_id = ? AND id = ?", tenantID, ruleID).Error
}

func isDuplicateKeyError(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(strings.ToLower(err.Error()), "duplicate entry")
}

func defaultAlertRuleID(tenantID string) string {
	sum := sha256.Sum256([]byte("data-analysis:alert-rule:" + tenantID + ":CONTRACT_EXPIRY"))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:26]
}

func stringPtr(value string) *string { return &value }

func defaultAlertRuleVersionID(tenantID, ruleID string, version int64) string {
	sum := sha256.Sum256([]byte("data-analysis:alert-rule-version:" + tenantID + ":" + ruleID + ":" + fmt.Sprint(version)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:26]
}
