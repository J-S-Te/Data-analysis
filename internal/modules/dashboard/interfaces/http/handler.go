// Package http adapts dashboard use cases to the existing Gin API contract.
package http

import (
	"context"
	"errors"
	stdhttp "net/http"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// dashboardQueryService is the local application boundary consumed by this
// HTTP adapter. It remains module-local so other subsystems cannot couple to
// dashboard use cases by importing it.
type dashboardQueryService interface {
	Contract(context.Context, string) (domain.ContractSnapshot, error)
	Project(context.Context, string) (domain.ProjectSnapshot, error)
}

// Handler exposes dashboard read use cases through Gin without importing GORM.
type Handler struct{ service dashboardQueryService }

// NewHandler constructs the dashboard HTTP adapter.
func NewHandler(service dashboardQueryService) *Handler { return &Handler{service: service} }

// Contract returns the current tenant's contract dashboard snapshot.
func (handler *Handler) Contract(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	snapshot, err := handler.service.Contract(c.Request.Context(), principal.TenantID)
	if err != nil {
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "CONTRACT_DASHBOARD_FAILED", "failed to load contract dashboard"))
		return
	}
	response.OK(c, snapshot)
}

// Project returns the current tenant's project dashboard snapshot.
func (handler *Handler) Project(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	snapshot, err := handler.service.Project(c.Request.Context(), principal.TenantID)
	if err != nil {
		if errors.Is(err, application.ErrInvalidProjectSnapshot) {
			response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "PROJECT_DASHBOARD_INVALID", "invalid project dashboard snapshot"))
			return
		}
		response.Error(c, apperror.Wrap(err, stdhttp.StatusInternalServerError, "PROJECT_DASHBOARD_FAILED", "failed to load project dashboard"))
		return
	}
	response.OK(c, snapshot)
}
