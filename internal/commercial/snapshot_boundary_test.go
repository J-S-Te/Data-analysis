package commercial_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	core "github.com/J-S-Te/license-core"
	lr "github.com/J-S-Te/license-core/runtime"
	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/alertworker"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/embedbridge"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 使用生产签名校验及持久状态，配合隔离密钥
// 和显式时钟。不连接平台同步客户端。
type signedBIRuntime struct {
	t                              *testing.T
	now, expiry                    time.Time
	path                           string
	binding                        lr.Binding
	keys, vendors                  map[string]ed25519.PublicKey
	platformPrivate, vendorPrivate ed25519.PrivateKey
	state                          lr.EnforcementState
	rt                             *lr.FileRuntime
}

func newSignedBIRuntime(t *testing.T, state lr.EnforcementState) *signedBIRuntime {
	t.Helper()
	pp, ps, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f := &signedBIRuntime{t: t, now: now, expiry: now.Add(time.Minute), path: filepath.Join(t.TempDir(), "state.json"), binding: lr.Binding{InstanceID: "bi-expiry-installation", Environment: "production", Application: "data_analysis", ServiceID: "bi-component"}, keys: map[string]ed25519.PublicKey{"platform-test": pp}, vendors: map[string]ed25519.PublicKey{"vendor-test": vp}, platformPrivate: ps, vendorPrivate: vs, state: state}
	f.restart()
	f.apply(1, f.expiry)
	return f
}
func (f *signedBIRuntime) restart() {
	f.t.Helper()
	rt, err := lr.NewFileRuntime(f.path, f.binding, f.keys, f.vendors)
	if err != nil {
		f.t.Fatal(err)
	}
	f.rt = rt
}
func (f *signedBIRuntime) apply(version uint64, expiry time.Time) {
	f.t.Helper()
	raw, err := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "bi-expiry-test", Version: version, CustomerID: "customer", ProductID: core.Product, Environment: f.binding.Environment, InstanceID: f.binding.InstanceID, IssuedAt: f.now.Unix() - 1000, NotBefore: f.now.Unix() - 100, Applications: []core.Application{{Code: "data_analysis", NotBefore: f.now.Unix() - 100, ExpiresAt: expiry.Unix(), Kind: "FULL"}}}, "vendor-test", f.vendorPrivate)
	if err != nil {
		f.t.Fatal(err)
	}
	snapshot, err := lr.SignSnapshot(lr.PlatformSnapshot{Protocol: 1, InstanceID: f.binding.InstanceID, Environment: f.binding.Environment, Application: f.binding.Application, ServiceID: f.binding.ServiceID, Revision: version, EnforcementState: f.state, MigrationEligible: f.state == lr.Pending, CurrentLicenseJWS: raw, HighestLicenseVersion: version, SnapshotIssuedAt: f.now.Unix()}, "platform-test", f.platformPrivate)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.rt.ApplySnapshot(context.Background(), snapshot, f.now); err != nil {
		f.t.Fatal(err)
	}
}
func (f *signedBIRuntime) check(ctx context.Context, operation string) error {
	return f.rt.Evaluate(ctx, core.Operation(operation), f.now)
}

func TestSignedMigrationAndEnforcedExpiryRestrictAllBIHTTPMutationsForEveryRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writes := []struct{ method, route, request string }{
		{"POST", "/alerts/:id/ack", "/alerts/alert-1/ack"}, {"POST", "/alerts/:id/close", "/alerts/alert-1/close"},
		{"PUT", "/dictionary", "/dictionary"}, {"POST", "/dictionary/metrics", "/dictionary/metrics"}, {"PUT", "/dictionary/metrics/:code", "/dictionary/metrics/metric-1"}, {"PATCH", "/dictionary/metrics/:code/enabled", "/dictionary/metrics/metric-1/enabled"}, {"DELETE", "/dictionary/metrics/:code", "/dictionary/metrics/metric-1"},
		{"POST", "/admin/sources/:id/trigger", "/admin/sources/source-1/trigger"}, {"POST", "/admin/sources/trigger-all", "/admin/sources/trigger-all"}, {"PUT", "/alert-rules", "/alert-rules"}, {"DELETE", "/alert-rules/:id", "/alert-rules/rule-1"}, {"GET", "/embed/:dashboard", "/embed/contract"}}
	histories := []string{"/alerts", "/alerts/summary", "/dictionary", "/dictionary/metrics/metric-1/versions", "/dashboard/contract", "/dashboard/project", "/dashboard/report", "/dashboard/finance", "/dashboard/contracts", "/dashboard/trend", "/admin/sources", "/alert-rules"}
	for _, state := range []lr.EnforcementState{lr.Pending, lr.Applying, lr.Enforced} {
		t.Run(string(state), func(t *testing.T) {
			f := newSignedBIRuntime(t, state)
			calls := 0
			router := gin.New()
			group := router.Group("/api/v1", commercial.Middleware(f.check))
			// 两个主体均有权限，拒绝必须来自许可证门禁。
			accept := func(c *gin.Context) { calls++; c.Status(200) }
			for _, route := range writes {
				group.Handle(route.method, route.route, middleware.RequirePermission("test.business"), accept)
			}
			for _, route := range histories {
				registered := route
				if strings.Contains(route, "metric-1") {
					registered = "/dictionary/metrics/:code/versions"
				}
				group.GET(registered, middleware.RequirePermission("test.business"), accept)
			}
			request := func(method, path, role string, want int) {
				t.Helper()
				req := httptest.NewRequest(method, "/api/v1"+path, nil)
				req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{TenantID: "tenant-1", UserID: role, Roles: []string{role}, Permissions: map[string]struct{}{"test.business": {}}}))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("%s %s %s: %d %s", role, method, path, w.Code, w.Body.String())
				}
				if want == 403 && !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
					t.Fatal("wrong denial", w.Body.String())
				}
			}
			for _, role := range []string{"super_admin", "ordinary_user"} {
				for _, route := range writes {
					request(route.method, route.request, role, 200)
				}
			}
			f.now = f.expiry
			f.restart()
			before := calls
			for _, role := range []string{"super_admin", "ordinary_user"} {
				for _, route := range writes {
					request(route.method, route.request, role, 403)
				}
			}
			if calls != before {
				t.Fatal("expired mutation reached handler")
			}
			for _, role := range []string{"super_admin", "ordinary_user"} {
				for _, route := range histories {
					request("GET", route, role, 200)
				}
			}
			// 重启及容许范围内时钟回拨不得延长有效期。
			f.now = f.expiry.Add(-time.Second)
			f.restart()
			request("POST", "/admin/sources/trigger-all", "super_admin", 403)
			f.now = f.expiry.Add(time.Second)
			f.apply(2, f.now.Add(time.Hour))
			request("POST", "/admin/sources/trigger-all", "ordinary_user", 200)
		})
	}
}

func TestSignedMigrationExpiryStopsAggregationAndAlertSideEffects(t *testing.T) {
	for _, state := range []lr.EnforcementState{lr.Pending, lr.Applying, lr.Enforced} {
		t.Run(string(state), func(t *testing.T) {
			f := newSignedBIRuntime(t, state)
			f.now = f.expiry
			f.restart()
			taskCheck := func(ctx context.Context) error { return f.check(ctx, commercial.MutateBusiness) }
			calls := 0
			pass := aggregation.Pass{LicenseCheck: taskCheck, SyncSources: func(context.Context) error { calls++; return nil }, SyncJobs: func(context.Context) error { calls++; return nil }, SyncContractDashboard: func(context.Context) error { calls++; return nil }, SyncProjectDashboard: func(context.Context) error { calls++; return nil }}
			if err := pass.RunOnce(context.Background()); err == nil || calls != 0 {
				t.Fatal("expired aggregation ran", err, calls)
			}
			runner := &aggregation.Runner{LicenseCheck: taskCheck}
			for _, run := range []func(context.Context) error{runner.RunOnce, runner.RunQueued, runner.EnsureSyncSources, runner.PrecomputeContractSigning} {
				if err := run(context.Background()); err == nil {
					t.Fatal("expired aggregation reached DB")
				}
			}
			api := aggregation.NewAPISyncRunner(nil, aggregation.APISyncOptions{LicenseCheck: taskCheck})
			for _, run := range []func(context.Context) error{api.SyncContractDashboard, api.SyncProjectDashboard} {
				if err := run(context.Background()); err == nil {
					t.Fatal("expired API aggregation reached DB/upstream")
				}
			}
			store := &biAlertStore{expiry: f.expiry}
			worker := alertworker.New(store)
			worker.LicenseCheck = taskCheck
			if n, err := worker.RunOnce(context.Background()); err == nil || n != 0 || store.reads != 0 || store.writes != 0 {
				t.Fatal("expired alert ran", n, err, store)
			}
			f.now = f.expiry.Add(time.Second)
			f.apply(2, f.now.Add(time.Hour))
			if err := pass.RunOnce(context.Background()); err != nil || calls != 4 {
				t.Fatal("renewed aggregation did not resume", err, calls)
			}
			if n, err := worker.RunOnce(context.Background()); err != nil || n != 1 || store.writes != 1 {
				t.Fatal("renewed alert did not resume", n, err)
			}
			// 加载候选期间到期，必须阻止实际 upsert。
			f.now = f.expiry.Add(time.Hour)
			f.apply(3, f.now.Add(time.Minute))
			store.beforeCandidates = func() { f.now = f.now.Add(time.Minute) }
			before := store.writes
			if n, err := worker.RunOnce(context.Background()); err == nil || n != 0 || store.writes != before {
				t.Fatal("mid-task expiry wrote alerts", n, err)
			}
			// 每个已配置聚合通道都重新检查签名授权。
			f.apply(4, f.now.Add(time.Minute))
			calls = 0
			pass.SyncSources = func(context.Context) error { calls++; f.now = f.now.Add(time.Minute); return nil }
			if err := pass.RunOnce(context.Background()); err == nil || calls != 1 {
				t.Fatal("aggregation continued to later channels after expiry", err, calls)
			}
		})
	}
}

type biAlertStore struct {
	reads, writes    int
	expiry           time.Time
	beforeCandidates func()
}

func (s *biAlertStore) ListEnabledRules(context.Context) ([]alertworker.Rule, error) {
	s.reads++
	return []alertworker.Rule{{Code: alertworker.RuleContractExpiry}}, nil
}
func (s *biAlertStore) FindContractExpiryCandidates(context.Context, time.Time, int) ([]alertworker.ContractExpiryCandidate, error) {
	s.reads++
	if s.beforeCandidates != nil {
		s.beforeCandidates()
	}
	return []alertworker.ContractExpiryCandidate{{TenantID: "tenant-1", TargetRef: "contract-1", DueDate: s.expiry}}, nil
}
func (s *biAlertStore) UpsertAlerts(context.Context, []alertworker.Alert) error {
	s.writes++
	return nil
}

func TestSignedPendingExpiryRejectsRealPreviouslyIssuedEmbedTokensWithoutConsumptionOrUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newSignedBIRuntime(t, lr.Pending)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "embed.sqlite")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.AutoMigrate(&oidc.EmbedToken{}); err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1); w.WriteHeader(200) }))
	defer upstream.Close()
	bridge, err := embedbridge.New(db, embedbridge.Options{LicenseCheck: f.check, MetabaseInternalURL: upstream.URL, EmbeddingSecret: "isolated-test-secret", DashboardIDs: map[string]string{"contract": "42"}, PublicOrigin: "https://platform.test", EmbedPublicOrigin: "https://embed.invalid", TokenTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	request := func(run func(*gin.Context), role string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "https://embed.invalid/", nil)
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{TenantID: "tenant-1", UserID: role, Roles: []string{role}, ScopeMode: "TENANT", Permissions: map[string]struct{}{"dashboard.contract.view": {}}}))
		run(c)
		return w
	}
	var consumedToken, unconsumedToken string
	for _, role := range []string{"super_admin", "ordinary_user"} {
		w := request(func(c *gin.Context) { bridge.Issue(c, "contract") }, role)
		if w.Code != 200 {
			t.Fatal("valid issuance failed", w.Code, w.Body.String())
		}
		var payload struct {
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Data.Token == "" {
			t.Fatal("missing token")
		}
		if consumedToken == "" {
			consumedToken = payload.Data.Token
		} else {
			unconsumedToken = payload.Data.Token
		}
	}
	w := request(func(c *gin.Context) { bridge.Proxy(c, consumedToken) }, "super_admin")
	if w.Code != 302 {
		t.Fatal("valid grant not consumed", w.Code, w.Body.String())
	}
	w = request(func(c *gin.Context) { bridge.ProxyResource(c, consumedToken, "/app/dist/app.js") }, "super_admin")
	if w.Code != 200 || upstreamCalls.Load() != 1 {
		t.Fatal("valid resource proxy failed", w.Code, upstreamCalls.Load())
	}
	f.now = f.expiry
	f.restart()
	for _, role := range []string{"super_admin", "ordinary_user"} {
		for _, run := range []func(*gin.Context){func(c *gin.Context) { bridge.Issue(c, "contract") }, func(c *gin.Context) { bridge.Proxy(c, unconsumedToken) }, func(c *gin.Context) { bridge.ProxyResource(c, consumedToken, "/app/dist/app.js") }} {
			w := request(run, role)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatal("expired embed escaped", w.Code, w.Body.String())
			}
		}
	}
	var count int64
	if err := db.Model(&oidc.EmbedToken{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("expiry persisted extra grants", count, err)
	}
	var unconsumed oidc.EmbedToken
	if err := db.Where("token_hash = ?", oidc.TokenHash(unconsumedToken)).First(&unconsumed).Error; err != nil || unconsumed.ConsumedAt != nil {
		t.Fatal("expiry consumed old grant", err)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatal("expiry forwarded resource", upstreamCalls.Load())
	}
}
