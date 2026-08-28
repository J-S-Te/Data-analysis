// Package http 将管理模块用例适配到现有 Gin API 协议。
package http

import (
	"context"
	"errors"
	stdhttp "net/http"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type managementService interface {
	ListSources(context.Context, string) ([]domain.SyncSource, error)
	TriggerSource(context.Context, string, string) (domain.SyncJob, error)
	ListAlertRules(context.Context, string) ([]domain.AlertRule, error)
	ReplaceAlertRules(context.Context, string, string, []domain.AlertRule) ([]domain.AlertRule, error)
}

// Handler 暴露数据源和预警规则管理接口，不直接依赖 GORM。
type Handler struct{ service managementService }

// NewHandler 构造管理模块 HTTP 适配器。
func NewHandler(service managementService) *Handler { return &Handler{service: service} }

// ListSources 返回数据源状态。
func (handler *Handler) ListSources(c *gin.Context) {
	sources, err := handler.service.ListSources(c.Request.Context(), tenantID(c))
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "SOURCE_LIST_FAILED", "failed to list sources"))
		return
	}
	response.OK(c, sources)
}

// TriggerSource 将手工同步请求写入队列。
func (handler *Handler) TriggerSource(c *gin.Context, id string) {
	job, err := handler.service.TriggerSource(c.Request.Context(), tenantID(c), id)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrSourceNotFound):
			response.Error(c, apperror.New(stdhttp.StatusNotFound, "SOURCE_NOT_FOUND", "sync source not found"))
		case errors.Is(err, application.ErrSourceLookupFailed):
			response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "SOURCE_LOOKUP_FAILED", "failed to load sync source"))
		case errors.Is(err, application.ErrSourceDisabled):
			response.Error(c, apperror.New(stdhttp.StatusConflict, "SOURCE_DISABLED", "sync source is disabled"))
		case errors.Is(err, application.ErrSyncAlreadyQueued):
			response.Error(c, apperror.New(stdhttp.StatusConflict, "SYNC_ALREADY_QUEUED", "sync source already has an active job"))
		default:
			response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "SYNC_JOB_CREATE_FAILED", "failed to queue sync job"))
		}
		return
	}
	response.OK(c, gin.H{"id": job.ID, "source_id": job.SourceID, "status": job.Status, "note": "同步请求已排队，aggregation-worker 将异步执行"})
}

// ListAlertRules 返回当前预警规则。
func (handler *Handler) ListAlertRules(c *gin.Context) {
	rules, err := handler.service.ListAlertRules(c.Request.Context(), tenantID(c))
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "RULE_LIST_FAILED", "failed to list rules"))
		return
	}
	response.OK(c, rules)
}

// PutAlertRules 按原有整表替换语义保存预警规则。
func (handler *Handler) PutAlertRules(c *gin.Context) {
	var input []alertRuleRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		response.Error(c, apperror.New(stdhttp.StatusBadRequest, "RULE_PAYLOAD_INVALID", "invalid rule payload"))
		return
	}
	actorID := ""
	if principal, ok := auth.FromContext(c.Request.Context()); ok {
		actorID = principal.UserID
	}
	rules, err := handler.service.ReplaceAlertRules(c.Request.Context(), tenantID(c), actorID, alertRulesFromRequest(input))
	if err != nil {
		if errors.Is(err, application.ErrRulePayloadInvalid) {
			response.Error(c, apperror.New(stdhttp.StatusBadRequest, "RULE_PAYLOAD_INVALID", "rule_code, name and source_fct are required"))
			return
		}
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "RULE_SAVE_FAILED", "failed to save rules"))
		return
	}
	response.OK(c, rules)
}

type alertRuleRequest struct {
	RuleCode      string  `json:"rule_code"`
	Name          string  `json:"name"`
	SourceFCT     string  `json:"source_fct"`
	Severity      string  `json:"severity"`
	Enabled       bool    `json:"enabled"`
	ThresholdJSON *string `json:"threshold_json"`
}

func alertRulesFromRequest(input []alertRuleRequest) []domain.AlertRule {
	rules := make([]domain.AlertRule, 0, len(input))
	for _, rule := range input {
		rules = append(rules, domain.AlertRule{RuleCode: rule.RuleCode, Name: rule.Name, SourceFCT: rule.SourceFCT, Severity: rule.Severity, Enabled: rule.Enabled, ThresholdJSON: rule.ThresholdJSON})
	}
	return rules
}

func tenantID(c *gin.Context) string {
	principal, _ := auth.FromContext(c.Request.Context())
	return principal.TenantID
}
