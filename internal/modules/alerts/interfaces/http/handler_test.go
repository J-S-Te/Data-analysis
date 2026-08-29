package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type alertServiceStub struct {
	listTenantID, updateTenantID, updateID, updateStatus string
	updated                                              bool
}

func (stub *alertServiceStub) List(_ context.Context, tenantID string) ([]domain.Item, error) {
	stub.listTenantID = tenantID
	return []domain.Item{{ID: "alert-1", TenantID: tenantID, Status: "OPEN"}}, nil
}
func (stub *alertServiceStub) Summary(context.Context, string) (domain.Summary, error) {
	return domain.Summary{BySeverity: map[string]int64{}, ByType: map[string]int64{}}, nil
}

func (stub *alertServiceStub) UpdateStatus(_ context.Context, tenantID, id, status string) (bool, error) {
	stub.updateTenantID, stub.updateID, stub.updateStatus = tenantID, id, status
	return stub.updated, nil
}

func TestRoutesKeepTenantScopeAndWriteGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &alertServiceStub{updated: true}
	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{
			TenantID: "tenant-1", Permissions: map[string]struct{}{"alert.view": {}, "alert.manage": {}},
		}))
		c.Next()
	})
	handler := NewHandler(service)
	router.GET("/alerts", middleware.RequirePermission("alert.view"), handler.List)
	router.POST("/alerts/:id/close", middleware.RequireSameOriginWrite("http://localhost:8081"), middleware.RequirePermission("alert.manage"), func(c *gin.Context) {
		handler.UpdateStatus(c, c.Param("id"), "CLOSED")
	})

	listRequest := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || service.listTenantID != "tenant-1" {
		t.Fatalf("list status=%d tenant=%q", listResponse.Code, service.listTenantID)
	}

	writeRequest := httptest.NewRequest(http.MethodPost, "/alerts/alert-1/close", nil)
	writeRequest.Header.Set("Origin", "http://localhost:8081")
	writeRequest.Header.Set("X-CSRF-Token", "1")
	writeResponse := httptest.NewRecorder()
	router.ServeHTTP(writeResponse, writeRequest)
	if writeResponse.Code != http.StatusOK || service.updateTenantID != "tenant-1" || service.updateID != "alert-1" || service.updateStatus != "CLOSED" {
		t.Fatalf("update status=%d tenant=%q id=%q state=%q", writeResponse.Code, service.updateTenantID, service.updateID, service.updateStatus)
	}
}
