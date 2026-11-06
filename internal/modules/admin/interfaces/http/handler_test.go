package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type rulesHTTPStub struct {
	managementService
	called        bool
	tenant, actor string
	rules         []domain.AlertRule
	err           error
}

func (s *rulesHTTPStub) ReplaceAlertRules(_ context.Context, tenant, actor string, rules []domain.AlertRule) ([]domain.AlertRule, error) {
	s.called = true
	s.tenant = tenant
	s.actor = actor
	s.rules = rules
	return rules, s.err
}
func (s *rulesHTTPStub) DeleteAlertRule(_ context.Context, tenant, id string) error {
	s.called = true
	s.tenant = tenant
	return s.err
}

func TestRuleHTTPRequiresManagementPermissionAndPreservesVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		principal *auth.Principal
		status    int
	}{
		{"unauthenticated", nil, 401},
		{"admin without effective permission", &auth.Principal{TenantID: "tenant", Roles: []string{"admin"}}, 403},
		{"reader", &auth.Principal{TenantID: "tenant", Permissions: map[string]struct{}{"dictionary.view": {}}}, 403},
		{"authorized manager", &auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"alert.manage": {}}}, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &rulesHTTPStub{}
			handler := NewHandler(stub)
			for _, method := range []string{"PUT", "DELETE"} {
				stub.called = false
				request := httptest.NewRequest(method, "/alert-rules", strings.NewReader(`[{"id":"rule-id","version":7,"rule_code":"CONTRACT_EXPIRY","enabled":false}]`))
				request.Header.Set("Content-Type", "application/json")
				if test.principal != nil {
					request = request.WithContext(auth.WithPrincipal(request.Context(), *test.principal))
				}
				writer := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(writer)
				c.Request = request
				if method == "PUT" {
					handler.PutAlertRules(c)
				} else {
					handler.DeleteAlertRule(c, "rule-id")
				}
				if writer.Code != test.status {
					t.Fatalf("%s status=%d body=%s", method, writer.Code, writer.Body)
				}
				if stub.called != (test.status == 200) {
					t.Fatalf("unauthorized service invocation: %v", stub.called)
				}
				if stub.called && (stub.tenant != "tenant" || (method == "PUT" && (stub.actor != "actor" || stub.rules[0].Version != 7 || stub.rules[0].ID != "rule-id" || stub.rules[0].Enabled))) {
					t.Fatalf("identity/version lost: %+v", stub)
				}
			}
		})
	}
}

func TestRuleHTTPConflictAndDeletionProtection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		method string
		err    error
		status int
		code   string
	}{
		{"PUT", application.ErrRuleVersionConflict, http.StatusConflict, "RULE_VERSION_CONFLICT"},
		{"DELETE", application.ErrRuleHasHistory, http.StatusConflict, "RULE_HAS_HISTORY"},
		{"DELETE", application.ErrRuleNotFound, http.StatusNotFound, "RULE_NOT_FOUND"},
	} {
		stub := &rulesHTTPStub{err: test.err}
		writer := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(writer)
		request := httptest.NewRequest(test.method, "/alert-rules", strings.NewReader(`[]`))
		request.Header.Set("Content-Type", "application/json")
		c.Request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"alert.manage": {}}}))
		if test.method == "PUT" {
			NewHandler(stub).PutAlertRules(c)
		} else {
			NewHandler(stub).DeleteAlertRule(c, "id")
		}
		if writer.Code != test.status || !strings.Contains(writer.Body.String(), test.code) {
			t.Fatalf("unexpected response %d %s", writer.Code, writer.Body)
		}
	}
}

// managementScopeStub 记录租户级配置写入是否真正到达服务层。
type managementScopeStub struct {
	managementService
	calls int
}

func (s *managementScopeStub) TriggerSource(_ context.Context, _ string, sourceID string) (domain.SyncJob, error) {
	s.calls++
	return domain.SyncJob{ID: "job-1", SourceID: sourceID, Status: "queued"}, nil
}

func (s *managementScopeStub) TriggerAllSources(_ context.Context, _ string) (int, int, error) {
	s.calls++
	return 1, 0, nil
}

func (s *managementScopeStub) ReplaceAlertRules(_ context.Context, _, _ string, rules []domain.AlertRule) ([]domain.AlertRule, error) {
	s.calls++
	return rules, nil
}

func (s *managementScopeStub) DeleteAlertRule(_ context.Context, _ string, _ string) error {
	s.calls++
	return nil
}

// TestAdminWritesRequireTenantScope 验证租户级配置写入只允许 TENANT 数据范围。
// 参数：t为handler测试上下文。
// 返回值：无；ORG/SELF 主体未被 403 拒绝、仍调用服务层或 TENANT 主体被误拒时失败。
func TestAdminWritesRequireTenantScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name string
		call func(*Handler, *gin.Context)
	}{
		{"put-alert-rules", func(handler *Handler, c *gin.Context) { handler.PutAlertRules(c) }},
		{"delete-alert-rule", func(handler *Handler, c *gin.Context) { handler.DeleteAlertRule(c, "rule-1") }},
		{"trigger-source", func(handler *Handler, c *gin.Context) { handler.TriggerSource(c, "source-1") }},
		{"trigger-all-sources", func(handler *Handler, c *gin.Context) { handler.TriggerAllSources(c) }},
	} {
		for _, mode := range []string{"", "ORG", "SELF"} {
			stub := &managementScopeStub{}
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			request := httptest.NewRequest(http.MethodPut, "/alert-rules", strings.NewReader(`[]`))
			request.Header.Set("Content-Type", "application/json")
			c.Request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: mode, Permissions: map[string]struct{}{"alert.manage": {}, "aggregation.manage": {}}}))
			tt.call(NewHandler(stub), c)
			if writer.Code != http.StatusForbidden || !strings.Contains(writer.Body.String(), "TENANT_SCOPE_REQUIRED") {
				t.Fatalf("%s scope=%q status=%d body=%s", tt.name, mode, writer.Code, writer.Body)
			}
			if stub.calls != 0 {
				t.Fatalf("%s scope=%q reached service %d times", tt.name, mode, stub.calls)
			}
		}
		// TENANT 绑定（如 dashboard_admin）不受影响：通过范围门禁并真正调用服务层。
		stub := &managementScopeStub{}
		writer := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(writer)
		request := httptest.NewRequest(http.MethodPut, "/alert-rules", strings.NewReader(`[]`))
		request.Header.Set("Content-Type", "application/json")
		c.Request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"alert.manage": {}, "aggregation.manage": {}}}))
		tt.call(NewHandler(stub), c)
		if writer.Code != http.StatusOK || stub.calls != 1 {
			t.Fatalf("%s TENANT scope status=%d calls=%d body=%s", tt.name, writer.Code, stub.calls, writer.Body)
		}
	}
}
