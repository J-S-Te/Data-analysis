// Package http 将告警用例适配到现有 Gin API 契约。
package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type alertService interface {
	List(context.Context, string, application.ListQuery) (application.ListResult, error)
	Summary(context.Context, string, application.AlertScope) (domain.Summary, error)
	UpdateStatus(context.Context, string, string, string, application.AlertScope) (bool, error)
}

func alertScope(principal auth.Principal) application.AlertScope {
	// 升级前建立的会话可能还没有 ScopeMode；与 dashboard 的兼容口径一致，按
	// TENANT 读取，待授权复核写入真实模式后自动收紧。未知非空模式仍由仓储
	// 失败时安全拒绝，不能被伪造为全租户。
	mode := principal.ScopeMode
	if mode == "" {
		mode = "TENANT"
	}
	ids := make([]string, 0, len(principal.OrganizationIDs)+len(principal.TeamIDs))
	ids = append(ids, principal.OrganizationIDs...)
	ids = append(ids, principal.TeamIDs...)
	return application.AlertScope{Mode: mode, OrgUnitIDs: ids}
}

// Handler 暴露告警读取及状态转换用例，不导入 GORM。
type Handler struct{ service alertService }

// NewHandler 构造告警 HTTP 适配器。
func NewHandler(service alertService) *Handler { return &Handler{service: service} }

// List 返回已认证租户内的告警。
func (handler *Handler) List(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	page, err := parsePage(c)
	if err != nil {
		response.Error(c, apperror.New(stdhttp.StatusBadRequest, "ALERT_INVALID_QUERY", err.Error()))
		return
	}
	page.Scope = alertScope(principal)
	items, err := handler.service.List(c.Request.Context(), principal.TenantID, page)
	if err != nil {
		if errors.Is(err, application.ErrInvalidQuery) {
			response.Error(c, apperror.New(stdhttp.StatusBadRequest, "ALERT_INVALID_QUERY", err.Error()))
			return
		}
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "ALERT_LIST_FAILED", "failed to list alerts"))
		return
	}
	response.OK(c, items)
}

func parsePage(c *gin.Context) (application.ListQuery, error) {
	query := application.ListQuery{Page: 1, PageSize: 50, Severity: strings.ToUpper(strings.TrimSpace(c.Query("severity"))), Status: strings.ToUpper(strings.TrimSpace(c.Query("status"))), Keyword: strings.TrimSpace(c.Query("q"))}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return query, errors.New("page must be a positive integer")
		}
		query.Page = value
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return query, errors.New("page_size must be 1..100")
		}
		query.PageSize = value
	}
	return query, nil
}

// Summary 返回当前租户预警聚合，不泄露其他租户记录。
func (handler *Handler) Summary(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	summary, err := handler.service.Summary(c.Request.Context(), principal.TenantID, alertScope(principal))
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "ALERT_SUMMARY_FAILED", "failed to aggregate alerts"))
		return
	}
	response.OK(c, summary)
}

// UpdateStatus 在已认证租户内将告警变为 ACK 或 CLOSED。
func (handler *Handler) UpdateStatus(c *gin.Context, id, status string) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	updated, err := handler.service.UpdateStatus(c.Request.Context(), principal.TenantID, id, status, alertScope(principal))
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "ALERT_UPDATE_FAILED", "failed to update alert"))
		return
	}
	if !updated {
		response.Error(c, apperror.New(stdhttp.StatusNotFound, "ALERT_NOT_FOUND", "预警不存在"))
		return
	}
	response.OK(c, gin.H{"id": id, "status": status})
}
