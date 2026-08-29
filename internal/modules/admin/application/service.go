// Package application 实现数据源和预警规则的管理用例。
package application

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
)

var (
	// ErrSourceNotFound 表示指定的数据源不存在。
	ErrSourceNotFound = errors.New("sync source not found")
	// ErrSourceLookupFailed 表示读取数据源时发生基础设施错误。
	ErrSourceLookupFailed = errors.New("failed to load sync source")
	// ErrSourceDisabled 表示数据源存在但当前不允许入队。
	ErrSourceDisabled = errors.New("sync source is disabled")
	// ErrSyncAlreadyQueued 表示同一租户的数据源已有等待或执行中的同步任务。
	ErrSyncAlreadyQueued = errors.New("sync source already has an active job")
	// ErrRulePayloadInvalid 表示预警规则缺少必要业务字段。
	ErrRulePayloadInvalid = errors.New("invalid alert rule payload")
	ErrRuleNotFound       = errors.New("alert rule not found")
	ErrRuleHasHistory     = errors.New("alert rule has alert history")
)

const maxRulesPerTenant = 50

// Repository 是管理模块自己的持久化端口，不向其他子系统暴露。
type Repository interface {
	ListSources(context.Context, string) ([]domain.SyncSource, error)
	FindSource(context.Context, string, string) (domain.SyncSource, bool, error)
	CreateSyncJob(context.Context, domain.SyncJob) (bool, error)
	EnsureDefaultAlertRules(context.Context, string, time.Time) error
	ListAlertRules(context.Context, string) ([]domain.AlertRule, error)
	ReplaceAlertRules(context.Context, string, []domain.AlertRule) error
	DeleteAlertRule(context.Context, string, string) error
}

// DeleteAlertRule 仅删除没有历史预警的规则，避免破坏审计和历史追溯。
func (service *Service) DeleteAlertRule(ctx context.Context, tenantID, ruleID string) error {
	if strings.TrimSpace(ruleID) == "" {
		return ErrRuleNotFound
	}
	return service.repository.DeleteAlertRule(ctx, tenantID, ruleID)
}

// Service 协调数据源入队和预警规则替换用例。
type Service struct {
	repository Repository
	now        func() time.Time
	newID      func() string
}

// NewService 构造生产环境使用的管理应用服务。
func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: func() time.Time { return time.Now().UTC() }, newID: generateID}
}

// ListSources 返回当前配置的数据源及其最近同步状态。
func (service *Service) ListSources(ctx context.Context, tenantID string) ([]domain.SyncSource, error) {
	return service.repository.ListSources(ctx, tenantID)
}

// TriggerSource 为启用的数据源创建一个 QUEUED 同步任务。
func (service *Service) TriggerSource(ctx context.Context, tenantID, sourceID string) (domain.SyncJob, error) {
	source, found, err := service.repository.FindSource(ctx, tenantID, sourceID)
	if err != nil {
		return domain.SyncJob{}, fmt.Errorf("%w: %v", ErrSourceLookupFailed, err)
	}
	if !found {
		return domain.SyncJob{}, ErrSourceNotFound
	}
	if !source.Enabled {
		return domain.SyncJob{}, ErrSourceDisabled
	}
	job := domain.SyncJob{TenantID: tenantID, ID: service.newID(), SourceID: source.ID, SubsystemCode: source.SubsystemCode, Status: "QUEUED", RequestedAt: service.now()}
	created, err := service.repository.CreateSyncJob(ctx, job)
	if err != nil {
		return domain.SyncJob{}, err
	}
	if !created {
		return domain.SyncJob{}, ErrSyncAlreadyQueued
	}
	return job, nil
}

// ListAlertRules 返回按规则编码排序的预警规则。
func (service *Service) ListAlertRules(ctx context.Context, tenantID string) ([]domain.AlertRule, error) {
	if err := service.repository.EnsureDefaultAlertRules(ctx, tenantID, service.now()); err != nil {
		return nil, err
	}
	return service.repository.ListAlertRules(ctx, tenantID)
}

// ReplaceAlertRules 保持原有整表替换语义，并在应用层统一校验规则字段。

// ReplaceAlertRules 在指定租户内原子替换预警规则，并拒绝无法由 Worker 安全执行的配置。
func (service *Service) ReplaceAlertRules(ctx context.Context, tenantID, actorID string, input []domain.AlertRule) ([]domain.AlertRule, error) {
	if len(input) == 0 || len(input) > maxRulesPerTenant {
		return nil, ErrRulePayloadInvalid
	}
	now := service.now()
	rules := make([]domain.AlertRule, 0, len(input))
	seenCodes := make(map[string]struct{}, len(input))
	for _, rule := range input {
		rule.RuleCode = strings.ToUpper(strings.TrimSpace(rule.RuleCode))
		rule.Name = strings.TrimSpace(rule.Name)
		rule.SourceFCT = strings.TrimSpace(rule.SourceFCT)
		rule.Severity = strings.ToUpper(strings.TrimSpace(rule.Severity))
		if !validAlertRule(rule) {
			return nil, ErrRulePayloadInvalid
		}
		if _, duplicated := seenCodes[rule.RuleCode]; duplicated {
			return nil, ErrRulePayloadInvalid
		}
		seenCodes[rule.RuleCode] = struct{}{}
		if rule.ID == "" {
			rule.ID = service.newID()
		}
		rule.TenantID = tenantID
		rule.CreatedAt, rule.UpdatedAt = now, now
		if actorID != "" {
			actor := actorID
			rule.UpdatedBy = &actor
		}
		rules = append(rules, rule)
	}
	if err := service.repository.ReplaceAlertRules(ctx, tenantID, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func validAlertRule(rule domain.AlertRule) bool {
	if rule.RuleCode != "CONTRACT_EXPIRY" || rule.SourceFCT != "dim_contract" || len(rule.Name) > 128 {
		return false
	}
	if rule.Severity != "LOW" && rule.Severity != "MEDIUM" && rule.Severity != "HIGH" {
		return false
	}
	if rule.ThresholdJSON == nil || len(*rule.ThresholdJSON) > 1024 {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(*rule.ThresholdJSON))
	decoder.DisallowUnknownFields()
	var threshold struct {
		Days int `json:"days"`
	}
	if err := decoder.Decode(&threshold); err != nil || threshold.Days < 1 || threshold.Days > 3650 {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func generateID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer), "=")[:26]
	}
	value := fmt.Sprint(time.Now().UnixNano())
	return strings.Repeat("0", 26-len(value)) + value
}
