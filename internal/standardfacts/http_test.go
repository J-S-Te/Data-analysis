package standardfacts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// factsHTTPFixture 挂载真实导入处理器并注入可控会话边界。
// 参数：t 为上下文，principal 为已认证主体，source 为显式来源。
// 返回值：真实 HTTP 路由和隔离仓储。
func factsHTTPFixture(t *testing.T, principal *auth.Principal, source string) (*gin.Engine, *Store) {
	t.Helper()
	store, _ := factsStoreFixture(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/v1", func(c *gin.Context) {
		if principal != nil {
			c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), *principal))
		}
	})
	(&Handler{Store: store, FinanceSource: source}).Register(api)
	return router, store
}

// TestFactsHTTPPreviewPublish 验证真实接口的预览、发布、未发布状态与文件边界。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFactsHTTPPreviewPublish(t *testing.T) {
	principal := &auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"aggregation.manage": {}}}
	router, store := factsHTTPFixture(t, principal, "standard")
	raw, _ := json.Marshal(financeFixture())
	request := httptest.NewRequest("POST", "/api/v1/admin/standard-finance/preview", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("preview %d %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data Preview `json:"data"`
	}
	json.Unmarshal(recorder.Body.Bytes(), &envelope)
	status, _ := store.Status(context.Background(), "tenant")
	if status.Published {
		t.Fatal("preview published facts")
	}
	publish, _ := json.Marshal(PublishRequest{Document: financeFixture(), Digest: envelope.Data.Digest, ExpectedVersion: envelope.Data.ExpectedVersion})
	request = httptest.NewRequest("POST", "/api/v1/admin/standard-finance/publish", bytes.NewReader(publish))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("publish %d %s", recorder.Code, recorder.Body.String())
	}
	snapshot, err := store.Read(context.Background(), "tenant")
	if err != nil || !snapshot.Available {
		t.Fatalf("published snapshot %+v %v", snapshot, err)
	}
	for _, body := range []string{`{"tenant_id":"other"}`, strings.Repeat("x", MaxFileBytes+1)} {
		request = httptest.NewRequest("POST", "/api/v1/admin/standard-finance/preview", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != 400 && recorder.Code != 413 {
			t.Fatalf("unsafe upload accepted: %d", recorder.Code)
		}
	}
}

// TestFactsHTTPAuthorization 验证租户只来自会话且非全租户范围不能导入或读取状态。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFactsHTTPAuthorization(t *testing.T) {
	for _, tc := range []struct {
		principal *auth.Principal
		source    string
		want      int
	}{
		{nil, "standard", 401},
		{&auth.Principal{TenantID: "t", UserID: "u", ScopeMode: "TENANT"}, "standard", 403},
		{&auth.Principal{TenantID: "t", UserID: "u", ScopeMode: "ORG", Permissions: map[string]struct{}{"aggregation.manage": {}}}, "standard", 403},
		{&auth.Principal{TenantID: "t", UserID: "u", ScopeMode: "SELF", Permissions: map[string]struct{}{"aggregation.manage": {}}}, "standard", 403},
	} {
		router, _ := factsHTTPFixture(t, tc.principal, tc.source)
		for _, path := range []string{"status", "template", "preview", "publish"} {
			method := "GET"
			if path == "preview" || path == "publish" {
				method = "POST"
			}
			req := httptest.NewRequest(method, "/api/v1/admin/standard-finance/"+path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s got=%d want=%d", path, rec.Code, tc.want)
			}
		}
	}
}

// TestFactsHTTPRejectsMissingAmounts 验证缺失金额或null不会被隐式解释为真实零值。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFactsHTTPRejectsMissingAmounts(t *testing.T) {
	principal := &auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"aggregation.manage": {}}}
	router, _ := factsHTTPFixture(t, principal, "standard")
	for _, row := range []string{
		`{"external_id":"r1","currency":"CNY","due_date":"2026-12-01"}`,
		`{"external_id":"r1","currency":"CNY","original_minor":null,"allocated_minor":0,"due_date":"2026-12-01"}`,
	} {
		raw := `{"schema_version":1,"batch_id":"b","as_of_date":"2026-11-04","complete":true,"receivables":[` + row + `],"receipts":[]}`
		req := httptest.NewRequest("POST", "/api/v1/admin/standard-finance/preview", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("missing amount accepted as zero: %d %s", rec.Code, rec.Body.String())
		}
	}
}
