// Package http adapts the metric dictionary use case to Gin.
package http

import (
	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// Handler exposes the read-only metric dictionary.
type Handler struct{ catalog *application.CatalogService }

// NewHandler constructs the dictionary HTTP adapter.
func NewHandler(catalog *application.CatalogService) *Handler { return &Handler{catalog: catalog} }

// Get returns the currently published metric dictionary.
func (handler *Handler) Get(c *gin.Context) { response.OK(c, handler.catalog.Get()) }
