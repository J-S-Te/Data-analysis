package oidc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
)

// TestActivityMergeUsesCentralAuthority 验证双模式合并后中央活动仍服从权威窗口。
// 参数：t为测试上下文，外部平台使用独立HTTP测试服务器替代。
// 返回值：无；错误分流、本地空闲覆盖中央或活动改变原截止时失败。
func TestActivityMergeUsesCentralAuthority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name         string
		status, want int
		localIdle    time.Duration
		sessionID    string
	}{
		{"central_valid_local_idle_expired", 200, 200, 31 * time.Minute, "central-cycle"},
		{"central_unavailable", 503, 503, time.Minute, "central-cycle"},
		{"central_inactive", 401, 401, time.Minute, "central-cycle"},
		{"central_cycle_conflict", 200, 401, time.Minute, "other-cycle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
			service.centralClient = newActivityMergeClient(t, test.status, test.sessionID)
			service.tenantID, service.clientID = "tenant-1", "analysis-rp"
			service.applicationCode, service.environmentCode = "data_analysis", "dev"
			binding, err := service.codec.encrypt("test-central-binding")
			if err != nil {
				t.Fatal(err)
			}
			last := time.Now().UTC().Add(-test.localIdle)
			seedIdleTestSession(t, db, func(session *Session) {
				session.CentralBindingCipher, session.CentralSessionID = binding, "central-cycle"
				session.LastInteractiveAt = &last
			})
			before := loadIdleTestSession(t, db)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/data_analysis/auth/activity", nil)
			c.Request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
			NewHandler(service, HTTPOptions{CookieName: "data_analysis_session"}).Activity(c)
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.want, recorder.Body.String())
			}
			after := loadIdleTestSession(t, db)
			if !after.LastInteractiveAt.Equal(*before.LastInteractiveAt) || !after.ExpiresAt.Equal(before.ExpiresAt) {
				t.Fatal("central activity changed local interaction or original absolute expiry")
			}
			if test.want == 200 {
				var payload sessionEnvelope
				if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Data.IdleTimeoutSeconds != 1800 || payload.Data.IdleRemainingMS != 1200000 {
					t.Fatalf("activity did not return central idle window: %+v", payload.Data)
				}
			}
		})
	}
}

// newActivityMergeClient 为活动回归提供真实中央HTTP客户端及隔离协议响应。
// 参数：t为测试上下文，status为平台响应码，sessionID为返回的中央周期。
// 返回值：只访问本地HTTP测试服务器的客户端；配置错误使测试失败。
func newActivityMergeClient(t *testing.T, status int, sessionID string) central.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth2/session/activity" ||
			r.Header.Get("Authorization") != "SessionBinding test-central-binding" {
			t.Error("activity used wrong protocol endpoint or binding")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 200 {
			_ = json.NewEncoder(w).Encode(central.Context{
				SessionID: sessionID, TenantID: "tenant-1", SubjectID: "user-1",
				ClientID: "analysis-rp", ApplicationCode: "data_analysis", EnvironmentCode: "dev",
				PolicyVersion: 2, IdleTimeoutSeconds: 1800, IdleRemainingMS: 1200000,
				AbsoluteExpiresAt:   time.Now().UTC().Add(6 * time.Hour),
				AbsoluteRemainingMS: (6 * time.Hour).Milliseconds(),
			})
		}
	}))
	t.Cleanup(server.Close)
	client, err := central.NewHTTPClient(server.URL, server.URL, "analysis-rp", server.URL+"/callback")
	if err != nil {
		t.Fatal(err)
	}
	return client
}
