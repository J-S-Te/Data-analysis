package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type alertServiceStub struct {
	listTenantID, updateTenantID, updateID, updateStatus string
	listQuery                                            application.ListQuery
	listScope, summaryScope, updateScope                 application.AlertScope
	updated                                              bool
}

func (stub *alertServiceStub) List(_ context.Context, tenantID string, query application.ListQuery) (application.ListResult, error) {
	stub.listTenantID = tenantID
	stub.listQuery = query
	return application.ListResult{Items: []domain.Item{{ID: "alert-1", TenantID: tenantID, Status: "OPEN"}}, Page: query.Page, PageSize: query.PageSize, Total: 1}, nil
}
func (stub *alertServiceStub) Summary(_ context.Context, _ string, scope application.AlertScope) (domain.Summary, error) {
	stub.summaryScope = scope
	return domain.Summary{BySeverity: map[string]int64{}, ByType: map[string]int64{}}, nil
}

func (stub *alertServiceStub) UpdateStatus(_ context.Context, tenantID, id, status string, scope application.AlertScope) (bool, error) {
	stub.updateTenantID, stub.updateID, stub.updateStatus, stub.updateScope = tenantID, id, status, scope
	return stub.updated, nil
}

func TestRoutesKeepTenantScopeAndWriteGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &alertServiceStub{updated: true}
	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{
			TenantID: "tenant-1", ScopeMode: "ORG", OrganizationIDs: []string{"org-1"}, TeamIDs: []string{"team-1"},
			Permissions: map[string]struct{}{"alert.view": {}, "alert.manage": {}},
		}))
		c.Next()
	})
	handler := NewHandler(service)
	router.GET("/alerts", middleware.RequirePermission("alert.view"), handler.List)
	router.POST("/alerts/:id/close", middleware.RequireSameOriginWrite("http://localhost:8081"), middleware.RequirePermission("alert.manage"), func(c *gin.Context) {
		handler.UpdateStatus(c, c.Param("id"), "CLOSED")
	})

	listRequest := httptest.NewRequest(http.MethodGet, "/alerts?page=2&page_size=25&severity=high&status=open&q=risk", nil)
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || service.listTenantID != "tenant-1" || service.listQuery.Page != 2 || service.listQuery.PageSize != 25 || service.listQuery.Severity != "HIGH" || service.listQuery.Status != "OPEN" ||
		service.listQuery.Scope.Mode != "ORG" || len(service.listQuery.Scope.OrgUnitIDs) != 2 {
		t.Fatalf("list status=%d tenant=%q query=%+v body=%s", listResponse.Code, service.listTenantID, service.listQuery, listResponse.Body.String())
	}

	writeRequest := httptest.NewRequest(http.MethodPost, "/alerts/alert-1/close", nil)
	writeRequest.Header.Set("Origin", "http://localhost:8081")
	writeRequest.Header.Set("X-CSRF-Token", "1")
	writeResponse := httptest.NewRecorder()
	router.ServeHTTP(writeResponse, writeRequest)
	if writeResponse.Code != http.StatusOK || service.updateTenantID != "tenant-1" || service.updateID != "alert-1" || service.updateStatus != "CLOSED" ||
		service.updateScope.Mode != "ORG" || len(service.updateScope.OrgUnitIDs) != 2 {
		t.Fatalf("update status=%d tenant=%q id=%q state=%q scope=%+v", writeResponse.Code, service.updateTenantID, service.updateID, service.updateStatus, service.updateScope)
	}
}
