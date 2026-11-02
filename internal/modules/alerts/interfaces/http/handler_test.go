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

// TestAlertScopeFailsClosedOnTeamNamespaceOverlap 验证命名空间断言（BIZ-DATA-09）：
// TEAM 模式同时携带组织单元授权时，告警范围收敛为“TEAM + 空集合”，
// 仓储据此生成恒空条件（1 = 0），不会借用组织授权看到任何告警行。
func TestAlertScopeFailsClosedOnTeamNamespaceOverlap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &alertServiceStub{updated: true}
	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{
			TenantID: "tenant-1", ScopeMode: "TEAM", OrganizationIDs: []string{"org-1"}, TeamIDs: []string{"team-1"},
			Permissions: map[string]struct{}{"alert.view": {}, "alert.manage": {}},
		}))
		c.Next()
	})
	handler := NewHandler(service)
	router.GET("/alerts", middleware.RequirePermission("alert.view"), handler.List)
	router.GET("/alerts/summary", middleware.RequirePermission("alert.view"), handler.Summary)
	router.POST("/alerts/:id/close", middleware.RequireSameOriginWrite("http://localhost:8081"), middleware.RequirePermission("alert.manage"), func(c *gin.Context) {
		handler.UpdateStatus(c, c.Param("id"), "CLOSED")
	})

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/alerts", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	if service.listQuery.Scope.Mode != "TEAM" || len(service.listQuery.Scope.OrgUnitIDs) != 0 {
		t.Fatalf("list scope=%+v, want TEAM with no ids", service.listQuery.Scope)
	}

	summaryResponse := httptest.NewRecorder()
	router.ServeHTTP(summaryResponse, httptest.NewRequest(http.MethodGet, "/alerts/summary", nil))
	if summaryResponse.Code != http.StatusOK {
		t.Fatalf("summary status=%d body=%s", summaryResponse.Code, summaryResponse.Body.String())
	}
	if service.summaryScope.Mode != "TEAM" || len(service.summaryScope.OrgUnitIDs) != 0 {
		t.Fatalf("summary scope=%+v, want TEAM with no ids", service.summaryScope)
	}

	writeRequest := httptest.NewRequest(http.MethodPost, "/alerts/alert-1/close", nil)
	writeRequest.Header.Set("Origin", "http://localhost:8081")
	writeRequest.Header.Set("X-CSRF-Token", "1")
	writeResponse := httptest.NewRecorder()
	router.ServeHTTP(writeResponse, writeRequest)
	if writeResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", writeResponse.Code, writeResponse.Body.String())
	}
	if service.updateScope.Mode != "TEAM" || len(service.updateScope.OrgUnitIDs) != 0 {
		t.Fatalf("update scope=%+v, want TEAM with no ids", service.updateScope)
	}
}

// TestAlertScopeKeepsOrgUnionAndTeamOnlySemantics 固化统一映射后告警侧的取值：
// ORG 取组织与团队并集，TEAM 只取 TeamIDs。
func TestAlertScopeKeepsOrgUnionAndTeamOnlySemantics(t *testing.T) {
	org := alertScope(auth.Principal{ScopeMode: "ORG", OrganizationIDs: []string{"org-1"}, TeamIDs: []string{"team-1"}})
	if org.Mode != "ORG" || len(org.OrgUnitIDs) != 2 {
		t.Fatalf("org scope=%+v, want ORG with two ids", org)
	}
	team := alertScope(auth.Principal{ScopeMode: "TEAM", TeamIDs: []string{"team-1"}})
	if team.Mode != "TEAM" || len(team.OrgUnitIDs) != 1 || team.OrgUnitIDs[0] != "team-1" {
		t.Fatalf("team scope=%+v, want TEAM with team-1 only", team)
	}
}
