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

// FindContractExpiryCandidates 读取普通合同在"到期条件集合"内的租户候选。
//
// 与旧版"只取未来 N 天窗口"不同，条件集合同时包含刚过期但未续签的合同
// （end_date 落在 [today-days, today+days] 内），因此已到期合同会持续命中、
// 保持 OPEN，而不是因为滑出窗口被一次未命中关闭。是否位于提醒窗口内由
// InWindow 单独返回，供调用方区分"即将到期"与"已到期未续签"。
// 参数：ctx 为查询上下文，evaluatedAt 为窗口基准日（业务时区"今天"的 UTC 墙钟
// 零点，调用方用 businessDayStart 换算后传入，DATE() 截断即得业务日），days 为
// 到期提醒窗口天数。
// 返回值：条件集合内的候选列表；租户缺失或读取失败返回错误。
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
		Table("dim_contract").Where("origin_kind = 'NORMAL'").
		Select(`tenant_id, owner_org_id, contract_id AS target_ref,
CONCAT('合同「', title, '」将在 ', DATE_FORMAT(end_date, '%Y-%m-%d'), ' 到期') AS title,
end_date AS due_date,
(end_date >= DATE(?) AND end_date <= DATE_ADD(DATE(?), INTERVAL ? DAY)) AS in_window`,
			evaluatedAt, evaluatedAt, days).
		Where("end_date IS NOT NULL").
		Where("tenant_id = ?", s.tenantID).
		Where("end_date >= DATE_SUB(DATE(?), INTERVAL ? DAY)", evaluatedAt, days).
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

// CountActiveAlerts 统计同一租户、同一规则下仍未关闭（OPEN/ACK）的告警数。
// 参数：ctx 为查询上下文，ruleCode 为规则编码。
// 返回值：未关闭告警数量；租户/规则缺失或读取失败返回错误。
// 用途：调用方以该基数判断本轮命中集合是否异常收缩（安全阀），避免上游数据缺失
// 导致整条规则被误判为"条件已消失"而批量关闭。
func (s *GormStore) CountActiveAlerts(ctx context.Context, ruleCode string) (int64, error) {
	if err := s.validateTenant(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(ruleCode) == "" {
		return 0, errors.New("alert active count rule_code is required")
	}
	var count int64
	err := s.db.WithContext(ctx).
		Table("alert_item").
		Where("tenant_id = ? AND rule_code = ? AND status IN ?", s.tenantID, ruleCode, []string{"OPEN", "ACK"}).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

// closedReasonAutoWindowExit 是旧版"离开窗口"收敛写入的关闭原因。
// 该原因的历史数据仍会被规则自动重开，保持向后兼容。
const closedReasonAutoWindowExit = "AUTO_WINDOW_EXIT"

// closedReasonAutoConditionCleared 是本轮起"条件连续未命中达到确认轮数"写入的关闭原因。
// 只有这两个自动原因会被规则评估重开；人工关闭（alerts 模块写入 MANUAL）永不复活。
const closedReasonAutoConditionCleared = "AUTO_CONDITION_CLEARED"

// reopenableClosedReasons 列出规则评估允许自动复开的关闭原因。
var reopenableClosedReasons = []string{closedReasonAutoWindowExit, closedReasonAutoConditionCleared}

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
(id, tenant_id, alert_type, rule_code, severity, owner_org_id, target_ref, title, due_date, status, closed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'OPEN', NULL, ?, ?)
ON DUPLICATE KEY UPDATE
rule_code = VALUES(rule_code),
severity = VALUES(severity),
owner_org_id = VALUES(owner_org_id),
title = VALUES(title),
due_date = VALUES(due_date),
updated_at = VALUES(updated_at)`,
				alert.ID,
				alert.TenantID,
				alert.AlertType,
				alert.RuleCode,
				alert.Severity,
				alert.OwnerOrgID,
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

// ReconcileAlerts 按本轮规则命中集合分三阶段收敛告警生命周期：
//  1. 命中：miss_streak 归零、hit_streak 累加、写入 last_hit_at；
//  2. 未命中：miss_streak 累加，只有连续未命中达到 ConfirmRounds 才 CLOSED
//     并写 closed_reason=AUTO_CONDITION_CLEARED（单轮抖动不再误关）；
//  3. 复开：仅对 closed_reason 属于自动原因（AUTO_WINDOW_EXIT /
//     AUTO_CONDITION_CLEARED）的告警回到 OPEN，reopen_count 累加，人工确认
//     证据 acknowledged_* 原样保留。
//
// 参数：ctx 为执行上下文，ruleCode 为已评估规则，active 为本轮实际命中的告警，
// evaluatedAt 为本轮计算基准时间，同时作为状态变更时间；options 为确认轮数与
// 复开策略。
// 返回值：租户/规则不一致或数据库失败返回错误。
//
// 边界：
//   - 只处理同一租户、同一规则的告警，不跨租户；
//   - 只关闭仍处于 OPEN/ACK 的记录，已关闭记录不会被重复改写；
//   - 命中集合为空（len(active)==0）时不做任何收敛，避免把"上游本轮没算出东西"
//     当成"条件全部消失"而批量关闭；收缩安全阀由调用方（Worker）按未关闭基数判断；
//   - 人工关闭（closed_reason=MANUAL）永不被重开。
func (s *GormStore) ReconcileAlerts(ctx context.Context, ruleCode string, active []Alert, evaluatedAt time.Time, options ReconcileOptions) error {
	if err := s.validateTenant(); err != nil {
		return err
	}
	if strings.TrimSpace(ruleCode) == "" {
		return errors.New("alert reconcile rule_code is required")
	}
	targets := make([]string, 0, len(active))
	seen := make(map[string]bool, len(active))
	for _, alert := range active {
		if alert.TenantID != s.tenantID {
			return errors.New("alert reconcile tenant does not match worker tenant")
		}
		if alert.RuleCode != ruleCode {
			return errors.New("alert reconcile rule does not match evaluated rule")
		}
		if alert.TargetRef == "" || seen[alert.TargetRef] {
			continue
		}
		seen[alert.TargetRef] = true
		targets = append(targets, alert.TargetRef)
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		// 安全阀：空命中集合一律不收敛（含累加 miss_streak），与调用方策略保持一致。
		return nil
	}
	confirmRounds := options.ConfirmRounds
	if confirmRounds < 1 {
		confirmRounds = 1
	}
	reopen := options.ReopenPolicy != ReopenPolicyManual
	openStatuses := []string{"OPEN", "ACK"}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 阶段 1：本轮命中，重置未命中计数并累加命中轮次。
		if err := tx.Table("alert_item").
			Where("tenant_id = ? AND rule_code = ? AND status IN ? AND target_ref IN ?",
				s.tenantID, ruleCode, openStatuses, targets).
			Updates(map[string]any{
				"miss_streak": 0,
				"hit_streak":  gorm.Expr("hit_streak + 1"),
				"last_hit_at": evaluatedAt,
				"updated_at":  evaluatedAt,
			}).Error; err != nil {
			return err
		}
		// 阶段 2：本轮未命中，先累加连续未命中轮数。
		leaving := tx.Table("alert_item").
			Where("tenant_id = ? AND rule_code = ? AND status IN ? AND target_ref NOT IN ?",
				s.tenantID, ruleCode, openStatuses, targets)
		if err := leaving.Updates(map[string]any{
			"miss_streak": gorm.Expr("miss_streak + 1"),
			"updated_at":  evaluatedAt,
		}).Error; err != nil {
			return err
		}
		// 达到确认轮数才关闭；closed_reason 标记为条件消失，允许条件重新命中后复开。
		if err := tx.Table("alert_item").
			Where("tenant_id = ? AND rule_code = ? AND status IN ? AND target_ref NOT IN ? AND miss_streak >= ?",
				s.tenantID, ruleCode, openStatuses, targets, confirmRounds).
			Updates(map[string]any{
				"status":        "CLOSED",
				"closed_at":     evaluatedAt,
				"closed_reason": closedReasonAutoConditionCleared,
				"updated_at":    evaluatedAt,
			}).Error; err != nil {
			return err
		}
		if !reopen {
			return nil
		}
		// 阶段 3：条件重新命中，只复开规则自己关闭的告警；acknowledged_* 证据保留。
		return tx.Table("alert_item").
			Where("tenant_id = ? AND rule_code = ? AND target_ref IN ? AND status = ? AND closed_reason IN ?",
				s.tenantID, ruleCode, targets, "CLOSED", reopenableClosedReasons).
			Updates(map[string]any{
				"status":        "OPEN",
				"closed_at":     nil,
				"closed_reason": "",
				"reopen_count":  gorm.Expr("reopen_count + 1"),
				"miss_streak":   0,
				"hit_streak":    gorm.Expr("hit_streak + 1"),
				"last_hit_at":   evaluatedAt,
				"updated_at":    evaluatedAt,
			}).Error
	})
}
