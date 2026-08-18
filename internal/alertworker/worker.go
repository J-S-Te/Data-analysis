// Package alertworker evaluates enabled alert rules against aggregation tables.
package alertworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	RuleContractExpiry = "CONTRACT_EXPIRY"
	defaultExpiryDays  = 30
	maxExpiryDays      = 3650
)

// Rule 是 alert_rule 中 Worker 执行所需的最小字段集合。
type Rule struct {
	Code          string
	Severity      string
	ThresholdJSON []byte
}

// ContractExpiryCandidate 是从 dim_contract 筛出的合同到期候选项。
type ContractExpiryCandidate struct {
	TenantID  string
	TargetRef string
	Title     string
	DueDate   time.Time
}

// Alert 是写入 alert_item 的标准化预警快照。
type Alert struct {
	ID          string
	TenantID    string
	AlertType   string
	RuleCode    string
	Severity    string
	TargetRef   string
	Title       string
	DueDate     time.Time
	EvaluatedAt time.Time
}

// Store 定义规则读取、候选查询和幂等写入边界。
type Store interface {
	ListEnabledRules(ctx context.Context) ([]Rule, error)
	FindContractExpiryCandidates(ctx context.Context, evaluatedAt time.Time, days int) ([]ContractExpiryCandidate, error)
	UpsertAlerts(ctx context.Context, alerts []Alert) error
}

// Worker 按现有聚合表计算预警；同一租户、类型和目标重复执行时幂等更新。
type Worker struct {
	store Store
	now   func() time.Time
}

func New(store Store) *Worker {
	return &Worker{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// RunOnce 计算一轮所有已启用规则，并返回成功写入的候选数量。
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	rules, err := w.store.ListEnabledRules(ctx)
	if err != nil {
		return 0, fmt.Errorf("list enabled alert rules: %w", err)
	}

	evaluatedAt := w.now().UTC()
	processed := 0
	var runErrors []error
	for _, rule := range rules {
		switch strings.ToUpper(strings.TrimSpace(rule.Code)) {
		case RuleContractExpiry:
			count, evaluateErr := w.evaluateContractExpiry(ctx, rule, evaluatedAt)
			processed += count
			if evaluateErr != nil {
				runErrors = append(runErrors, fmt.Errorf("evaluate rule %s: %w", rule.Code, evaluateErr))
			}
		default:
			runErrors = append(runErrors, fmt.Errorf("alert rule %s is not supported", rule.Code))
		}
	}
	return processed, errors.Join(runErrors...)
}

func (w *Worker) evaluateContractExpiry(ctx context.Context, rule Rule, evaluatedAt time.Time) (int, error) {
	days, err := contractExpiryDays(rule.ThresholdJSON)
	if err != nil {
		return 0, err
	}
	candidates, err := w.store.FindContractExpiryCandidates(ctx, evaluatedAt, days)
	if err != nil {
		return 0, fmt.Errorf("find contract expiry candidates: %w", err)
	}
	alerts := make([]Alert, 0, len(candidates))
	for _, candidate := range candidates {
		alerts = append(alerts, Alert{
			ID:          stableAlertID(candidate.TenantID, RuleContractExpiry, candidate.TargetRef),
			TenantID:    candidate.TenantID,
			AlertType:   RuleContractExpiry,
			RuleCode:    rule.Code,
			Severity:    normalizedSeverity(rule.Severity),
			TargetRef:   candidate.TargetRef,
			Title:       candidate.Title,
			DueDate:     candidate.DueDate,
			EvaluatedAt: evaluatedAt,
		})
	}
	if err := w.store.UpsertAlerts(ctx, alerts); err != nil {
		return 0, fmt.Errorf("upsert contract expiry alerts: %w", err)
	}
	return len(alerts), nil
}

func contractExpiryDays(raw []byte) (int, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultExpiryDays, nil
	}
	var threshold struct {
		Days int `json:"days"`
	}
	if err := json.Unmarshal(raw, &threshold); err != nil {
		return 0, fmt.Errorf("decode CONTRACT_EXPIRY threshold_json: %w", err)
	}
	if threshold.Days <= 0 || threshold.Days > maxExpiryDays {
		return 0, fmt.Errorf("CONTRACT_EXPIRY days must be between 1 and %d", maxExpiryDays)
	}
	return threshold.Days, nil
}

func normalizedSeverity(severity string) string {
	switch normalized := strings.ToUpper(strings.TrimSpace(severity)); normalized {
	case "LOW", "MEDIUM", "HIGH":
		return normalized
	default:
		return "MEDIUM"
	}
}

func stableAlertID(tenantID, alertType, targetRef string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + alertType + "\x00" + targetRef))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:26]
}
