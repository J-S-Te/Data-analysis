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
	"github.com/unified-identity-auth-platform/data-analysis/internal/ownerdirectory"
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
	ErrRulePayloadInvalid  = errors.New("invalid alert rule payload")
	ErrRuleNotFound        = errors.New("alert rule not found")
	ErrRuleHasHistory      = errors.New("alert rule has alert history")
	ErrRuleVersionConflict = errors.New("alert rule version conflict")
	// ErrRuleSavedRefreshFailed 表示提交已经完成，仅后续列表读取失败。
	ErrRuleSavedRefreshFailed = errors.New("规则已保存，但最新列表读取失败")
)

const maxRulesPerTenant = 50

// Repository 是管理模块自己的持久化端口，不向其他子系统暴露。
type Repository interface {
	ListSources(context.Context, string) ([]domain.SyncSource, error)
	FindSource(context.Context, string, string) (domain.SyncSource, bool, error)
	CreateSyncJob(context.Context, domain.SyncJob) (bool, error)
	ListAlertRules(context.Context, string) ([]domain.AlertRule, error)
	ReplaceAlertRules(context.Context, string, []domain.AlertRule) error
	DeleteAlertRule(context.Context, string, string) error
	PreviewContractExpiry(context.Context, string, int, domain.AlertRuleScope, time.Time, int) (domain.AlertPreviewResult, error)
	AlertRuleScopeOptions(context.Context, string) (domain.AlertRuleScopeOptions, error)
	LatestAlertRuleExecutions(context.Context, string) ([]domain.AlertRuleExecution, error)
}

// TriggerAllSources 为当前租户一次性排入全部启用数据源，并跳过已有活动任务。
func (service *Service) TriggerAllSources(ctx context.Context, tenantID string) (queued, skipped int, err error) {
	sources, err := service.repository.ListSources(ctx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	for _, source := range sources {
		if !source.Enabled {
			skipped++
			continue
		}
		job := domain.SyncJob{TenantID: tenantID, ID: service.newID(), SourceID: source.ID, SubsystemCode: source.SubsystemCode, Status: "QUEUED", RequestedAt: service.now()}
		created, createErr := service.repository.CreateSyncJob(ctx, job)
		if createErr != nil {
			return queued, skipped, createErr
		}
		if created {
			queued++
		} else {
			skipped++
		}
	}
	return queued, skipped, nil
}

// DeleteAlertRule 仅删除没有历史预警的规则，避免破坏审计和历史追溯。
func (service *Service) DeleteAlertRule(ctx context.Context, tenantID, ruleID string) error {
	if strings.TrimSpace(ruleID) == "" {
		return ErrRuleNotFound
	}
	return service.repository.DeleteAlertRule(ctx, tenantID, ruleID)
}

// LatestAlertRuleExecutions 返回每个规则最近一次执行记录。
// 参数：ctx 为上下文；tenantID 为可信租户。
// 返回值：按规则 ID 去重后的执行结果；读取失败返回错误。
func (service *Service) LatestAlertRuleExecutions(ctx context.Context, tenantID string) ([]domain.AlertRuleExecution, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, ErrRulePayloadInvalid
	}
	return service.repository.LatestAlertRuleExecutions(ctx, tenantID)
}

// AlertRuleScopeOptions 读取当前租户可用于规则范围的聚合维度值。
// 参数：ctx 为上下文；tenantID 为可信租户。
// 返回值：组织、客户类型、币种去重列表；读取失败返回错误。
func (service *Service) AlertRuleScopeOptions(ctx context.Context, tenantID string) (domain.AlertRuleScopeOptions, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.AlertRuleScopeOptions{}, ErrRulePayloadInvalid
	}
	options, err := service.repository.AlertRuleScopeOptions(ctx, tenantID)
	if err != nil {
		return domain.AlertRuleScopeOptions{}, err
	}
	if service.directory != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		for index := range options.Users {
			if lookupCtx.Err() != nil {
				break
			}
			if strings.TrimSpace(options.Users[index].DisplayName) != "" {
				continue
			}
			name, lookupErr := service.directory.Lookup(lookupCtx, options.Users[index].UserID)
			if lookupErr == nil && strings.TrimSpace(name) != "" {
				options.Users[index].DisplayName = strings.TrimSpace(name)
			}
		}
	}
	return options, nil
}

// PreviewAlertRule 计算合同到期规则当前会命中的合同数量和样例，不写入任何状态。
// 参数：tenantID 为可信租户；days 为到期前后窗口；scopeJSON 为可选范围。
// 返回值：命中数量、最多 20 条样例和是否被截断；非法参数或读取失败返回错误。
func (service *Service) PreviewAlertRule(ctx context.Context, tenantID string, days int, scopeJSON *string) (domain.AlertPreviewResult, error) {
	if strings.TrimSpace(tenantID) == "" || days < 1 || days > 3650 {
		return domain.AlertPreviewResult{}, ErrRulePayloadInvalid
	}
	rule := domain.AlertRule{ScopeJSON: scopeJSON}
	if err := normalizeAlertRuleScope(&rule); err != nil {
		return domain.AlertPreviewResult{}, ErrRulePayloadInvalid
	}
	var scope domain.AlertRuleScope
	if rule.ScopeJSON != nil {
		if err := json.Unmarshal([]byte(*rule.ScopeJSON), &scope); err != nil {
			return domain.AlertPreviewResult{}, ErrRulePayloadInvalid
		}
	}
	location := service.location
	if location == nil {
		location = time.UTC
	}
	return service.repository.PreviewContractExpiry(ctx, tenantID, days, scope, service.now().In(location), 20)
}

// Service 协调数据源入队和预警规则替换用例。
type Service struct {
	repository Repository
	location   *time.Location
	now        func() time.Time
	newID      func() string
	directory  *ownerdirectory.Client
}

// NewService 构造生产环境使用的管理应用服务；默认 UTC，由启动方注入业务时区。
func NewService(repository Repository) *Service {
	return &Service{repository: repository, location: time.UTC, now: func() time.Time { return time.Now().UTC() }, newID: generateID}
}

// SetOwnerDirectory 注入平台负责人目录，用于把用户ID解析成姓名。
// 参数：directory 为独立机器身份的目录读取器，nil保留原ID。
// 返回值：无。
func (service *Service) SetOwnerDirectory(directory *ownerdirectory.Client) {
	service.directory = directory
}

// SetBusinessLocation 注入预警预览和规则执行使用的业务时区。
// 参数：location 为IANA时区；nil保持UTC。
// 返回值：无。
func (service *Service) SetBusinessLocation(location *time.Location) {
	if location != nil {
		service.location = location
	}
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
	if strings.TrimSpace(tenantID) == "" {
		return nil, ErrRulePayloadInvalid
	}
	return service.repository.ListAlertRules(ctx, tenantID)
}

// ReplaceAlertRules 用途：在指定租户内原子保存规则，删除使用独立的历史保护接口。
// 参数：ctx 为请求上下文，tenantID 和 actorID 为可信身份，input 为规则内容。
// 返回值：保存后的规则；提交后读取失败返回明确已保存的错误语义。
func (service *Service) ReplaceAlertRules(ctx context.Context, tenantID, actorID string, input []domain.AlertRule) ([]domain.AlertRule, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorID) == "" || len(input) == 0 || len(input) > maxRulesPerTenant {
		return nil, ErrRulePayloadInvalid
	}
	now := service.now()
	rules := make([]domain.AlertRule, 0, len(input))
	seenNames := make(map[string]struct{}, len(input))
	enabledCodes := make(map[string]struct{}, len(input))
	for _, rule := range input {
		rule.RuleCode = strings.ToUpper(strings.TrimSpace(rule.RuleCode))
		rule.Name = strings.TrimSpace(rule.Name)
		rule.SourceFCT = strings.TrimSpace(rule.SourceFCT)
		rule.Severity = strings.ToUpper(strings.TrimSpace(rule.Severity))
		if !validAlertRule(rule) {
			return nil, ErrRulePayloadInvalid
		}
		if err := normalizeAlertRuleScope(&rule); err != nil {
			return nil, ErrRulePayloadInvalid
		}
		if err := normalizeAlertRuleNotification(&rule); err != nil {
			return nil, ErrRulePayloadInvalid
		}
		if rule.Version < 0 || (rule.ID == "" && rule.Version != 0) || (rule.ID != "" && (len(rule.ID) != 26 || rule.Version < 1)) {
			return nil, ErrRulePayloadInvalid
		}
		duplicateKey := rule.RuleCode + "\x00" + rule.Name
		if _, duplicated := seenNames[duplicateKey]; duplicated {
			return nil, ErrRulePayloadInvalid
		}
		seenNames[duplicateKey] = struct{}{}
		if rule.Enabled {
			if _, alreadyEnabled := enabledCodes[rule.RuleCode]; alreadyEnabled {
				return nil, ErrRulePayloadInvalid
			}
			enabledCodes[rule.RuleCode] = struct{}{}
		}
		if rule.ID == "" {
			rule.ID = service.newID()
		}
		rule.TenantID = tenantID
		rule.UpdatedAt = now
		if rule.CreatedAt.IsZero() {
			rule.CreatedAt = now
		}
		if actorID != "" {
			actor := actorID
			rule.UpdatedBy = &actor
		}
		rules = append(rules, rule)
	}
	if err := service.repository.ReplaceAlertRules(ctx, tenantID, rules); err != nil {
		return nil, err
	}
	saved, err := service.repository.ListAlertRules(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRuleSavedRefreshFailed, err)
	}
	return saved, nil
}

// normalizeAlertRuleScope 校验并规范化规则范围 JSON 与生效日期。
// 参数：rule 为待保存规则；空 scope/日期表示不限制。
// 返回值：非法 JSON、负数金额、范围倒置或非法日期返回错误。
func normalizeAlertRuleScope(rule *domain.AlertRule) error {
	if rule == nil {
		return ErrRulePayloadInvalid
	}
	if rule.ScopeJSON != nil {
		raw := strings.TrimSpace(*rule.ScopeJSON)
		if raw == "" || raw == "null" {
			rule.ScopeJSON = nil
		} else {
			decoder := json.NewDecoder(strings.NewReader(raw))
			decoder.DisallowUnknownFields()
			var scope domain.AlertRuleScope
			if err := decoder.Decode(&scope); err != nil {
				return ErrRulePayloadInvalid
			}
			scope.OwnerOrgIDs = normalizeStringList(scope.OwnerOrgIDs, 100, 64)
			scope.CustomerTypes = normalizeStringList(scope.CustomerTypes, 100, 64)
			scope.Currencies = normalizeCurrencyList(scope.Currencies)
			if scope.AmountMinMinor != nil && *scope.AmountMinMinor < 0 {
				return ErrRulePayloadInvalid
			}
			if scope.AmountMaxMinor != nil && *scope.AmountMaxMinor < 0 {
				return ErrRulePayloadInvalid
			}
			if scope.AmountMinMinor != nil && scope.AmountMaxMinor != nil && *scope.AmountMinMinor > *scope.AmountMaxMinor {
				return ErrRulePayloadInvalid
			}
			if len(scope.OwnerOrgIDs) == 0 && len(scope.CustomerTypes) == 0 && len(scope.Currencies) == 0 && scope.AmountMinMinor == nil && scope.AmountMaxMinor == nil {
				rule.ScopeJSON = nil
			} else {
				encoded, err := json.Marshal(scope)
				if err != nil {
					return ErrRulePayloadInvalid
				}
				normalized := string(encoded)
				rule.ScopeJSON = &normalized
			}
		}
	}
	if rule.EffectiveFrom != nil {
		value := strings.TrimSpace(*rule.EffectiveFrom)
		if value == "" {
			rule.EffectiveFrom = nil
		} else if _, err := time.Parse("2006-01-02", value); err != nil {
			return ErrRulePayloadInvalid
		} else {
			rule.EffectiveFrom = &value
		}
	}
	if rule.EffectiveTo != nil {
		value := strings.TrimSpace(*rule.EffectiveTo)
		if value == "" {
			rule.EffectiveTo = nil
		} else if _, err := time.Parse("2006-01-02", value); err != nil {
			return ErrRulePayloadInvalid
		} else {
			rule.EffectiveTo = &value
		}
	}
	if rule.EffectiveFrom != nil && rule.EffectiveTo != nil && *rule.EffectiveFrom > *rule.EffectiveTo {
		return ErrRulePayloadInvalid
	}
	return nil
}

// normalizeAlertRuleNotification 校验并规范化通知对象、静默时段和到期升级配置。
func normalizeAlertRuleNotification(rule *domain.AlertRule) error {
	if rule == nil {
		return ErrRulePayloadInvalid
	}
	if rule.NotificationJSON == nil {
		return nil
	}
	raw := strings.TrimSpace(*rule.NotificationJSON)
	if raw == "" || raw == "null" {
		rule.NotificationJSON = nil
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var notification domain.AlertRuleNotification
	if err := decoder.Decode(&notification); err != nil {
		return ErrRulePayloadInvalid
	}
	notification.Recipients = normalizeStringList(notification.Recipients, 50, 64)
	notification.EscalationRecipients = normalizeStringList(notification.EscalationRecipients, 50, 64)
	notification.QuietStart = strings.TrimSpace(notification.QuietStart)
	notification.QuietEnd = strings.TrimSpace(notification.QuietEnd)
	if (notification.QuietStart == "") != (notification.QuietEnd == "") {
		return ErrRulePayloadInvalid
	}
	for _, value := range []string{notification.QuietStart, notification.QuietEnd} {
		if value == "" {
			continue
		}
		if _, err := time.Parse("15:04", value); err != nil {
			return ErrRulePayloadInvalid
		}
	}
	if notification.EscalationAfterMinutes < 0 || notification.EscalationAfterMinutes > 10080 {
		return ErrRulePayloadInvalid
	}
	if notification.EscalationAfterMinutes > 0 && len(notification.EscalationRecipients) == 0 {
		return ErrRulePayloadInvalid
	}
	if notification.Enabled && len(notification.Recipients) == 0 {
		return ErrRulePayloadInvalid
	}
	if !notification.Enabled && len(notification.Recipients) == 0 && notification.QuietStart == "" && notification.EscalationAfterMinutes == 0 {
		rule.NotificationJSON = nil
		return nil
	}
	encoded, err := json.Marshal(notification)
	if err != nil {
		return ErrRulePayloadInvalid
	}
	normalized := string(encoded)
	rule.NotificationJSON = &normalized
	return nil
}

func normalizeCurrencyList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.ToUpper(strings.TrimSpace(raw))
		if len(value) != 3 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeStringList(values []string, maxItems, maxLen int) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || len([]rune(value)) > maxLen {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) >= maxItems {
			break
		}
	}
	return result
}

func validAlertRule(rule domain.AlertRule) bool {
	if rule.Name == "" || len([]rune(rule.Name)) > 128 {
		return false
	}
	minDays := 1
	switch rule.RuleCode {
	case "CONTRACT_EXPIRY":
		if rule.SourceFCT != "dim_contract" {
			return false
		}
	case "PAYMENT_OVERDUE":
		if rule.SourceFCT != "standard_finance_fact" {
			return false
		}
		minDays = 0
	default:
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
	if err := decoder.Decode(&threshold); err != nil || threshold.Days < minDays || threshold.Days > 3650 {
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
