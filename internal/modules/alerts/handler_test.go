package alerts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type alertStoreStub struct {
	items          []alerts.AlertItem
	listTenantID   string
	updateID       string
	updateTenantID string
	updateStatus   string
	updateClosedAt *time.Time
	updateFound    bool
}

func (s *alertStoreStub) ListByTenant(
	_ context.Context,
	tenantID string,
	_ int,
) ([]alerts.AlertItem, error) {
	s.listTenantID = tenantID
	return s.items, nil
}

func (s *alertStoreStub) UpdateStatus(
	_ context.Context,
	id, tenantID, status string,
	_ time.Time,
	closedAt *time.Time,
) (bool, error) {
	s.updateID = id
	s.updateTenantID = tenantID
	s.updateStatus = status
	s.updateClosedAt = closedAt
	return s.updateFound, nil
}

func TestAlertsHTTPRoutesListAndCloseWithinAuthenticatedTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &alertStoreStub{
		items:       []alerts.AlertItem{{ID: "alert-1", TenantID: "tenant-1", Status: "OPEN"}},
		updateFound: true,
	}
	router := alertRouter(store, auth.Principal{
		TenantID:    "tenant-1",
		Permissions: map[string]struct{}{"alert.view": {}, "alert.manage": {}},
	})

	listResponse := performRequest(router, http.MethodGet, "/alerts")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("GET /alerts status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}
	if store.listTenantID != "tenant-1" {
		t.Fatalf("list tenant = %q, want tenant-1", store.listTenantID)
	}
	var listEnvelope struct {
		Code string             `json:"code"`
		Data []alerts.AlertItem `json:"data"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listEnvelope); err != nil {
		t.Fatalf("decode GET /alerts response: %v", err)
	}
	if listEnvelope.Code != "OK" || len(listEnvelope.Data) != 1 || listEnvelope.Data[0].ID != "alert-1" {
		t.Fatalf("unexpected GET /alerts response: %+v", listEnvelope)
	}

	closeRequest := httptest.NewRequest(http.MethodPost, "/alerts/alert-1/close", nil)
	closeRequest.Header.Set("Origin", "http://localhost:8081")
	closeRequest.Header.Set("X-CSRF-Token", "1")
	closeResponse := httptest.NewRecorder()
	router.ServeHTTP(closeResponse, closeRequest)
	if closeResponse.Code != http.StatusOK {
		t.Fatalf("POST /alerts/:id/close status = %d, body = %s", closeResponse.Code, closeResponse.Body.String())
	}
	if store.updateID != "alert-1" || store.updateTenantID != "tenant-1" || store.updateStatus != "CLOSED" {
		t.Fatalf("update call = id:%q tenant:%q status:%q", store.updateID, store.updateTenantID, store.updateStatus)
	}
	if store.updateClosedAt == nil {
		t.Fatal("closed_at = nil, want close timestamp")
	}
}

func TestAlertsHTTPWriteRequiresSameOriginAndPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &alertStoreStub{updateFound: true}
	router := alertRouter(store, auth.Principal{
		TenantID:    "tenant-1",
		Permissions: map[string]struct{}{"alert.view": {}},
	})

	response := performRequest(router, http.MethodPost, "/alerts/alert-1/close")
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing same-origin headers status = %d, want 403", response.Code)
	}
	if store.updateID != "" {
		t.Fatalf("store update called for rejected request: %q", store.updateID)
	}

	request := httptest.NewRequest(http.MethodPost, "/alerts/alert-1/close", nil)
	request.Header.Set("Origin", "http://localhost:8081")
	request.Header.Set("X-CSRF-Token", "1")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing alert.manage permission status = %d, want 403", response.Code)
	}
	if store.updateID != "" {
		t.Fatalf("store update called without permission: %q", store.updateID)
	}
}

func TestAlertsHTTPReturnsNotFoundWhenTenantCannotSeeTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &alertStoreStub{updateFound: false}
	router := alertRouter(store, auth.Principal{
		TenantID:    "tenant-1",
		Permissions: map[string]struct{}{"alert.manage": {}},
	})
	request := httptest.NewRequest(http.MethodPost, "/alerts/missing/close", nil)
	request.Header.Set("Origin", "http://localhost:8081")
	request.Header.Set("X-CSRF-Token", "1")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing alert status = %d, body = %s", response.Code, response.Body.String())
	}
}

func alertRouter(store alerts.Store, principal auth.Principal) *gin.Engine {
	handler := alerts.NewHandlerWithStore(store)
	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	})
	router.GET("/alerts", middleware.RequirePermission("alert.view"), handler.List)
	router.POST(
		"/alerts/:id/close",
		middleware.RequireSameOriginWrite("http://localhost:8081"),
		middleware.RequirePermission("alert.manage"),
		func(c *gin.Context) { handler.UpdateStatus(c, c.Param("id"), "CLOSED") },
	)
	return router
}

func performRequest(router http.Handler, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
