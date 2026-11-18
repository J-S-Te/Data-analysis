package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/authorizationrefresh"
)

// TestLegacyConcurrentAuthorizationNeverRestoresWideScope 验证同revision的普通旧读不能覆盖强制新窄授权。
// 参数：t为真实认证器、独立SQLite事务与本地平台HTTP服务的测试上下文。
// 返回值：无；迟到结果恢复旧权限、重复远程读取或改变原期限时失败。
func TestLegacyConcurrentAuthorizationNeverRestoresWideScope(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	service.tenantID, service.clientID, service.applicationCode, service.environmentCode = "tenant-1", "analysis-client", "data_analysis", "dev"
	service.checkEvery = time.Minute
	cipher, err := service.codec.encrypt("valid-access")
	if err != nil {
		t.Fatal(err)
	}
	original := seedIdleTestSession(t, db, func(session *Session) {
		session.AccessTokenCipher = cipher
		session.AuthorizationCheckedAt = time.Now().Add(-2 * time.Minute)
		session.AuthzRevision = 9
	})
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	oldStarted, releaseOld, freshStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		value := AuthorizationContext{Subject: "kc-user-1", IdentityID: "user-1", TenantID: "tenant-1", ClientID: "analysis-client", ApplicationCode: "data_analysis", EnvironmentCode: "dev", Roles: []string{"sales"}, Permissions: []string{"dashboard.overview.view"}, DataScopes: []DataScope{{RoleCode: "sales", ScopeType: "TENANT", ScopeID: "tenant-1", EnvironmentCode: "dev"}}, AuthorizationRevision: 9}
		if r.Header.Get("X-Authorization-Refresh") == "1" {
			close(freshStarted)
			value.Roles = []string{"auditor"}
			value.Permissions = []string{"dashboard.report.view"}
			value.DataScopes = []DataScope{{RoleCode: "auditor", ScopeType: "ORG", ScopeID: "org-limited", EnvironmentCode: "dev"}}
		} else {
			close(oldStarted)
			<-releaseOld
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	service.authorizationContextURL = server.URL
	oldDone, freshDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := service.Authenticate(context.Background(), idleTestRawToken); oldDone <- err }()
	select {
	case <-oldStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary read did not reach authorization source")
	}
	request := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	request.Header.Set("X-Authorization-Refresh", "1")
	ctx := authorizationrefresh.FromRequest(context.Background(), request)
	oldHoldsConnection := raw.Stats().InUse > 0
	freshLaunched := make(chan struct{})
	go func() { close(freshLaunched); _, err := service.Authenticate(ctx, idleTestRawToken); freshDone <- err }()
	<-freshLaunched
	freshFinished := false
	// 事务持有唯一连接时新请求排队；旧实现已释放连接，必须先完成R2再释放R1。
	if !oldHoldsConnection {
		select {
		case <-freshStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("fresh source read did not start")
		}
		select {
		case err := <-freshDone:
			if err != nil {
				t.Fatal(err)
			}
			freshFinished = true
		case <-time.After(5 * time.Second):
			t.Fatal("fresh read did not finish")
		}
	}
	close(releaseOld)
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary read did not finish")
	}
	if !freshFinished {
		select {
		case err := <-freshDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("fresh read did not finish after transaction")
		}
	}
	current, err := service.Authenticate(context.Background(), idleTestRawToken)
	if err != nil || current.HasPermission("dashboard.overview.view") || !current.HasPermission("dashboard.report.view") || len(current.OrganizationIDs) != 1 || current.OrganizationIDs[0] != "org-limited" || calls.Load() != 2 {
		t.Fatalf("late read restored wide authorization: principal=%+v calls=%d error=%v", current, calls.Load(), err)
	}
	persisted := loadIdleTestSession(t, db)
	if !persisted.ExpiresAt.Equal(original.ExpiresAt) || !persisted.LastInteractiveOrCreated().Equal(original.LastInteractiveOrCreated()) {
		t.Fatal("serialization extended original deadlines")
	}
}
