// Package http adapts alert use cases to the existing Gin API contract.
package http

import (
	"context"
	stdhttp "net/http"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type alertService interface {
	List(context.Context, string) ([]domain.Item, error)
	UpdateStatus(context.Context, string, string, string) (bool, error)
}

// Handler exposes alert read and status transition use cases without importing GORM.
type Handler struct{ service alertService }

// NewHandler constructs the alert HTTP adapter.
func NewHandler(service alertService) *Handler { return &Handler{service: service} }

// List returns alerts within the authenticated tenant.
func (handler *Handler) List(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	items, err := handler.service.List(c.Request.Context(), principal.TenantID)
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "ALERT_LIST_FAILED", "failed to list alerts"))
		return
	}
	response.OK(c, items)
}

// UpdateStatus changes an alert to ACK or CLOSED within the authenticated tenant.
func (handler *Handler) UpdateStatus(c *gin.Context, id, status string) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	updated, err := handler.service.UpdateStatus(c.Request.Context(), principal.TenantID, id, status)
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
