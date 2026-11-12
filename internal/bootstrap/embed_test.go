package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// TestNativeModeSkipsBridgeInitialization 验证原生模式不要求 URL、秘密或独立来源。
// 参数：t 为测试上下文。
// 返回值：无。
func TestNativeModeSkipsBridgeInitialization(t *testing.T) {
	bridge, err := newDashboardEmbedBridge(nil, Config{EmbedPublicOrigin: "invalid"}, nil)
	if bridge != nil || err != nil {
		t.Fatalf("disabled bridge=%v error=%v", bridge, err)
	}
	for _, cfg := range []Config{
		{DashboardEmbedEnabled: true},
		{DashboardEmbedEnabled: true, MetabaseInternalURL: "http://metabase:3000"},
		{DashboardEmbedEnabled: true, MetabaseInternalURL: "http://metabase:3000", MetabaseEmbeddingSecret: "test-only", PublicOrigin: "https://app.example.com", EmbedPublicOrigin: "https://app.example.com:8443"},
	} {
		if _, err := newDashboardEmbedBridge(nil, cfg, nil); err == nil {
			t.Fatal("enabled mode accepted missing credentials or unsafe origin")
		}
	}
}

// TestDashboardEmbedReadyRequiresCompleteConfig 固定能力降级边界：
// 开关打开但缺少任一嵌入前提时，不得向浏览器宣告可嵌入。
func TestDashboardEmbedReadyRequiresCompleteConfig(t *testing.T) {
	complete := Config{
		DashboardEmbedEnabled:   true,
		MetabaseInternalURL:     "http://metabase:3000",
		MetabaseEmbeddingSecret: "test-only",
		EmbedPublicOrigin:       "https://embed.example.com",
		DashboardIDs: map[string]string{
			"overview": "2", "contract": "2", "project": "3", "report": "4", "finance": "5",
		},
	}
	if !dashboardEmbedReady(complete) {
		t.Fatal("complete embed config should be ready")
	}
	if dashboardEmbedReady(Config{DashboardEmbedEnabled: false, DashboardIDs: complete.DashboardIDs}) {
		t.Fatal("disabled embed must not be ready")
	}
	for name, mutate := range map[string]func(*Config){
		"missing-url":    func(cfg *Config) { cfg.MetabaseInternalURL = "" },
		"missing-secret": func(cfg *Config) { cfg.MetabaseEmbeddingSecret = "" },
		"missing-origin": func(cfg *Config) { cfg.EmbedPublicOrigin = "" },
		"missing-id":     func(cfg *Config) { cfg.DashboardIDs["finance"] = "" },
	} {
		cfg := complete
		cfg.DashboardIDs = map[string]string{}
		for key, value := range complete.DashboardIDs {
			cfg.DashboardIDs[key] = value
		}
		mutate(&cfg)
		if dashboardEmbedReady(cfg) {
			t.Fatalf("%s: incomplete embed config must not be ready", name)
		}
	}
}

type embedTestAuthenticator struct{ principal auth.Principal }

// Authenticate 以隔离测试主体模拟已验证会话，不接触真实身份服务。
// 参数：ctx 为请求上下文，token 为测试会话 Cookie。
// 返回值：固定测试主体或缺少会话错误。
func (a embedTestAuthenticator) Authenticate(ctx context.Context, token string) (auth.Principal, error) {
	if token != "valid-session" {
		return auth.Principal{}, errors.New("invalid session")
	}
	return a.principal, nil
}

// TestNativeEmbedRoutesKeepGates 验证关闭能力在认证、权限与许可后明确拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestNativeEmbedRoutesKeepGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		path                       string
		cookie, permission, denied bool
		want                       int
		code                       string
	}{
		{path: "/embed/finance", want: 401, code: "UNAUTHENTICATED"},
		{path: "/embed/finance", cookie: true, want: 403, code: "FORBIDDEN"},
		{path: "/embed/unknown", cookie: true, permission: true, want: 403, code: "FORBIDDEN"},
		{path: "/embed/finance", cookie: true, permission: true, denied: true, want: 403, code: "COMMERCIAL_LICENSE_DENIED"},
		{path: "/embed/finance", cookie: true, permission: true, want: 503, code: "DASHBOARD_EMBED_DISABLED"},
		{path: "/embed-proxy/old-token", want: 401, code: "UNAUTHENTICATED"},
		{path: "/embed-proxy/old-token/app.js", cookie: true, denied: true, want: 403, code: "COMMERCIAL_LICENSE_DENIED"},
		{path: "/embed-proxy/old-token/app.js", cookie: true, want: 503, code: "DASHBOARD_EMBED_DISABLED"},
	} {
		principal := auth.Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]struct{}{}}
		if tc.permission {
			principal.Permissions["dashboard.finance.view"] = struct{}{}
		}
		check := commercial.Check(func(context.Context, string) error {
			if tc.denied {
				return errors.New("expired")
			}
			return nil
		})
		router := gin.New()
		base := router.Group("/data_analysis")
		api := base.Group("/api/v1", middleware.SessionAuth(embedTestAuthenticator{principal}, "session"), commercial.Middleware(check))
		mountDashboardEmbedRoutes(api, base, nil, Config{}, check)
		req := httptest.NewRequest("GET", "/data_analysis/api/v1"+tc.path, nil)
		if tc.cookie {
			req.AddCookie(&http.Cookie{Name: "session", Value: "valid-session"})
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != tc.want || !strings.Contains(recorder.Body.String(), tc.code) {
			t.Fatalf("%s: status=%d body=%s", tc.path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "embed_url") {
			t.Fatal("disabled endpoint issued a capability")
		}
	}
}
