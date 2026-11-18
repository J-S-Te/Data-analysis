package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
)

type pageRefreshCentralClient struct {
	central.Client
	state central.Context
}

// State 返回已绑定的中央会话窗口，不产生真实活动。
// 参数：上下文及绑定为本地隔离测试参数。
// 返回值：当前测试窗口，无错误。
func (c *pageRefreshCentralClient) State(context.Context, string) (central.Context, error) {
	return c.state, nil
}

// TestPageRefreshHTTPRecomputesAuthorization 验证同一 Cookie 刷新角色和范围且保留原期限。
// 参数：t 为内存会话服务和真实 HTTP 中间件链的测试上下文。
// 返回值：无；旧快照、错误分类、重复下游调用或期限变化时失败。
func TestPageRefreshHTTPRecomputesAuthorization(t *testing.T) {
	for _, centralMode := range []bool{false, true} {
		for _, tc := range []struct {
			name, path, marker      string
			upstream, status, calls int
		}{
			{"new roles", "/data_analysis/api/v1/auth/me", "1", 200, 200, 1},
			{"all removed", "/data_analysis/api/v1/auth/me", "1", 403, 403, 1},
			{"dependency outage", "/data_analysis/api/v1/auth/me", "1", 503, 503, 1},
			{"authentication lost", "/data_analysis/api/v1/auth/me", "1", 401, 401, 1},
			{"ordinary cached read", "/data_analysis/api/v1/dashboard/fixture", "", 200, 200, 0},
			{"business marker ignored", "/data_analysis/api/v1/dashboard/fixture", "1", 200, 200, 0},
		} {
			t.Run(tc.name+map[bool]string{true: " central", false: " legacy"}[centralMode], func(t *testing.T) {
				service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
				service.tenantID = "tenant-1"
				service.clientID = "analysis-client"
				service.applicationCode = "data_analysis"
				service.environmentCode = "dev"
				cipher, _ := service.codec.encrypt("valid-access-token")
				binding, _ := service.codec.encrypt("central-binding")
				session := seedIdleTestSession(t, db, func(session *Session) {
					session.AccessTokenCipher = cipher
					session.AccessTokenExpiresAt = time.Now().Add(time.Hour)
					session.OIDCSubject = "kc-user-1"
					if centralMode {
						session.CentralBindingCipher = binding
						session.CentralSessionID = "central-cycle"
					}
				})
				if centralMode {
					service.centralClient = &pageRefreshCentralClient{state: central.Context{SessionID: "central-cycle", TenantID: "tenant-1", SubjectID: "user-1", ClientID: "analysis-client", ApplicationCode: "data_analysis", EnvironmentCode: "dev", PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: (30 * time.Minute).Milliseconds(), AbsoluteRemainingMS: (8 * time.Hour).Milliseconds(), AbsoluteExpiresAt: session.ExpiresAt}}
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Header.Get("X-Authorization-Refresh") != "1" {
						t.Error("platform freshness marker missing")
					}
					w.WriteHeader(tc.upstream)
					if tc.upstream == 200 {
						_ = json.NewEncoder(w).Encode(AuthorizationContext{Subject: "kc-user-1", IdentityID: "user-1", TenantID: "tenant-1", ClientID: "analysis-client", ApplicationCode: "data_analysis", EnvironmentCode: "dev", Roles: []string{"auditor"}, Permissions: []string{"dashboard.report.view"}, DataScopes: []DataScope{{RoleCode: "auditor", ScopeType: "ORG", ScopeID: "org-limited"}}, AuthorizationRevision: 9})
					}
				}))
				defer server.Close()
				service.authorizationContextURL = server.URL
				router := newSessionIdleChain(t, service)
				router.GET("/data_analysis/api/v1/dashboard/fixture", func(c *gin.Context) { c.Status(200) })
				request := httptest.NewRequest("GET", tc.path, nil)
				request.Header.Set("X-Authorization-Refresh", tc.marker)
				request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != tc.status || calls != tc.calls {
					t.Fatalf("status=%d want=%d calls=%d want=%d body=%s", response.Code, tc.status, calls, tc.calls, response.Body.String())
				}
				current := loadIdleTestSession(t, db)
				if tc.upstream == 200 && tc.calls == 1 {
					if current.AuthzRevision != 9 || current.RolesJSON != "[\"auditor\"]" || current.DataScopes()[0].ScopeID != "org-limited" {
						t.Fatalf("new authorization not persisted: %+v", current)
					}
				}
				if !current.ExpiresAt.Equal(session.ExpiresAt) || !current.LastInteractiveOrCreated().Equal(session.LastInteractiveOrCreated()) {
					t.Fatal("page refresh extended session")
				}
				if tc.status == 503 && current.RevokedAt != nil {
					t.Fatal("outage revoked login")
				}
				if tc.status == http.StatusForbidden {
					// 已确认撤权后，后续无刷新标记的请求也不能命中旧授权成功。
					followup := httptest.NewRequest("GET", "/data_analysis/api/v1/dashboard/fixture", nil)
					followup.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
					denied := httptest.NewRecorder()
					router.ServeHTTP(denied, followup)
					if denied.Code == http.StatusOK {
						t.Fatal("ordinary request reused revoked authorization")
					}
				}
			})
		}
	}
}

// TestPageRefreshDatabaseOutageRemainsUnavailable 验证会话查询失败不能被误判为登录失效。
// 参数：t 为独立 SQLite 和正式会话中间件的测试上下文。
// 返回值：无；数据库故障未返回503时失败。
func TestPageRefreshDatabaseOutageRemainsUnavailable(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	seedIdleTestSession(t, db, nil)
	router := newSessionIdleChain(t, service)
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/data_analysis/api/v1/auth/me", nil)
	request.Header.Set("X-Authorization-Refresh", "1")
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatalf("database outage status=%d body=%s", response.Code, response.Body.String())
	}
}
