package http

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type businessService interface {
	Business(context.Context, string, string) (domain.BusinessSnapshot, error)
}

func (handler *Handler) Business(section string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := auth.FromContext(c.Request.Context())
		if !ok {
			response.Error(c, apperror.ErrUnauthenticated)
			return
		}
		svc, ok := handler.service.(businessService)
		if !ok {
			response.Error(c, apperror.New(503, "BUSINESS_DASHBOARD_UNAVAILABLE", "统计服务不可用"))
			return
		}
		value, err := svc.Business(c.Request.Context(), p.TenantID, section)
		if err != nil {
			if errors.Is(err, application.ErrBusinessScopeUnavailable) {
				response.Error(c, apperror.New(403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE", "当前数据范围无法读取此统计；报告与财务看板暂仅支持经授权的全租户范围。"))
				return
			}
			response.Error(c, apperror.Wrap(err, 500, "BUSINESS_DASHBOARD_FAILED", "统计读取失败，请稍后重试"))
			return
		}
		response.OK(c, value)
	}
}
