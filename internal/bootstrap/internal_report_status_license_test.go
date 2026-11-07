package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/importreport"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// reportStatusLicenseTenant 是隔离测试机器绑定的唯一可信租户。
const reportStatusLicenseTenant = "tenant-license"

// recordingLicenseCheck 记录许可门禁收到的操作分类。
// 只断言"放行/拒绝"无法发现端点被改判成 MutateBusiness 之类的分类漂移，
// 因此保留每一次 Allow 的 operation 精确值供测试核对。
type recordingLicenseCheck struct {
	mu    sync.Mutex
	deny  error
	calls []string
}

// allow 模拟商业许可校验器：先记录分类，再返回测试预设的拒绝原因。
// 参数：ctx 为请求上下文，operation 为商业许可分类。
// 返回值：预设的拒绝原因；未预设时放行。
func (r *recordingLicenseCheck) allow(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, operation)
	return r.deny
}

// operations 返回按调用顺序记录的许可分类快照。
// 参数：无。
// 返回值：分类副本，调用方修改不影响记录。
func (r *recordingLicenseCheck) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// licenseMachine 是机器报表状态端点的隔离测试绑定：验签器与签名私钥。
type licenseMachine struct {
	verifier *importreport.Verifier
	key      ed25519.PrivateKey
}

// newLicenseTestMachine 生成 Ed25519 密钥对并构造完整绑定的机器验签器。
// 参数：t 为测试上下文。
// 返回值：可信租户为 reportStatusLicenseTenant 的验签器与签名私钥。
func newLicenseTestMachine(t *testing.T) licenseMachine {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report-status.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	verifier, err := importreport.NewVerifier(importreport.VerifierOptions{
		Issuer: "platform", Audience: "analysis", PublicKeyPath: path, ClientID: "contract-reader",
		OAuthClientID: "machine-client", TenantID: reportStatusLicenseTenant, CallerEnvironment: "test",
		RequireApplicationID: true, RequireEnvironmentID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return licenseMachine{verifier: verifier, key: key}
}

// token 签发当前时刻有效的合同系统机器令牌。
// 参数：t 为测试上下文。
// 返回值：EdDSA 签名的 Bearer 令牌原文。
func (m licenseMachine) token(t *testing.T) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": "platform", "aud": "analysis", "sub": "contract-reader",
		"oauth_client_id": "machine-client", "tenant_id": reportStatusLicenseTenant,
		"application_id": "app-1", "application_code": "contract_management",
		"environment_id": "env-1", "environment_code": "test",
		"token_use": "application", "scope": []string{"analysis.contract-import.report-status"},
		"iat": now - 1, "nbf": now - 1, "exp": now + 60,
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload := head + "." + base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.key, []byte(payload)))
}

// reportStatusOutboxDDL 复刻生产 bi_audit_outbox 的列（迁移 000016/000028），
// 使机器读取的"事务内审计"在内存库里可断言。
const reportStatusOutboxDDL = `CREATE TABLE bi_audit_outbox (
id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, actor_id TEXT,
action TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL,
request_id TEXT, payload TEXT,
login_ip BLOB, client_ip BLOB, ip_source TEXT NOT NULL DEFAULT 'UNKNOWN',
result TEXT NOT NULL DEFAULT 'SUCCESS', delivery_status TEXT NOT NULL DEFAULT 'pending',
attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at DATETIME NOT NULL, locked_at DATETIME,
delivered_at DATETIME, last_error TEXT, created_at DATETIME NOT NULL)`

// licenseTestDB 构造隔离内存库并注册数据访问计数回调。
// 参数：t 为测试上下文。
// 返回值：已建报表状态与审计表的库，以及统计 gorm 数据访问次数的计数器。
// 计数器是"拒绝时不触达数据"的可判定证据：不依赖被查数据是否存在。
func licenseTestDB(t *testing.T) (*gorm.DB, *int32) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { pool.Close() })
	if err := db.AutoMigrate(&aggregation.ImportReportStatus{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(reportStatusOutboxDDL).Error; err != nil {
		t.Fatal(err)
	}
	var accesses int32
	count := func(*gorm.DB) { atomic.AddInt32(&accesses, 1) }
	type registerFunc func(name string, fn func(*gorm.DB)) error
	for _, register := range []registerFunc{
		func(name string, fn func(*gorm.DB)) error {
			return db.Callback().Query().Before("gorm:query").Register(name, fn)
		},
		func(name string, fn func(*gorm.DB)) error {
			return db.Callback().Row().Before("gorm:row").Register(name, fn)
		},
		func(name string, fn func(*gorm.DB)) error {
			return db.Callback().Raw().Before("gorm:raw").Register(name, fn)
		},
	} {
		if err := register("b5:count_data_access", count); err != nil {
			t.Fatal(err)
		}
	}
	return db, &accesses
}

// licenseTestRouter 按 app.New 的装配方式挂载机器端点：根路由 + 生产挂载函数，
// 不含 base 前缀，也不挂组级商业许可中间件。
// 参数：db 为报表状态库，verifier 为机器验签器，check 为许可检查器。
// 返回值：与生产一致的根路由。
// 说明：许可结算沿用 app.go 中 status handler 的同一行绑定
// （licenseCheck.Allow(ctx, commercial.ReadHistory)）；该 handler 内校验是
// 这个机器端点唯一的商业许可门禁。
func licenseTestRouter(db *gorm.DB, verifier *importreport.Verifier, check commercial.Check) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID(), gin.Recovery())
	license := func(ctx context.Context) error { return check.Allow(ctx, commercial.ReadHistory) }
	mountInternalReportStatus(router, importreport.StatusHandler(db, verifier, license))
	return router
}

// TestInternalReportStatusDeniedLicenseSkipsData 验证机器报表状态端点在许可拒绝时
// 返回实现定义的 503 REPORT_STATUS_LICENSE_UNAVAILABLE（不是 403），且在任何数据
// 访问之前终止：数据访问计数为 0、不写读取审计、响应体不含报表状态字段。
func TestInternalReportStatusDeniedLicenseSkipsData(t *testing.T) {
	db, accesses := licenseTestDB(t)
	testMachine := newLicenseTestMachine(t)
	license := &recordingLicenseCheck{deny: errors.New("license expired")}
	router := licenseTestRouter(db, testMachine.verifier, commercial.Check(license.allow))

	request := httptest.NewRequest(http.MethodGet, InternalReportStatusPath+"?item_id=item-1&source_revision=1&report_release_version=2", nil)
	request.Header.Set("Authorization", "Bearer "+testMachine.token(t))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want %d: %s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"REPORT_STATUS_LICENSE_UNAVAILABLE"`) {
		t.Fatalf("body=%s want code REPORT_STATUS_LICENSE_UNAVAILABLE", body)
	}
	if got := atomic.LoadInt32(accesses); got != 0 {
		t.Fatalf("data access count=%d want 0: license denial must not reach the report store", got)
	}
	for _, leaked := range []string{`"data"`, `"state"`, "applied_channels", "source_snapshot_id", "aggregation_run_id"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("body=%s leaked report status field %q", body, leaked)
		}
	}
	var audits int64
	if err := db.Table("bi_audit_outbox").Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("audit rows=%d want 0: denied request must not record a read proof", audits)
	}
	ops := license.operations()
	if len(ops) != 1 || ops[0] != commercial.ReadHistory {
		t.Fatalf("license operations=%v want exactly [%s]", ops, commercial.ReadHistory)
	}
}

// TestInternalReportStatusAllowedLicenseReadsHistory 验证许可放行时端点按
// ReadHistory 结算且恰好结算一次，并返回可判定字段正常的成功响应。
func TestInternalReportStatusAllowedLicenseReadsHistory(t *testing.T) {
	db, accesses := licenseTestDB(t)
	testMachine := newLicenseTestMachine(t)
	status := aggregation.ImportReportStatus{
		TenantID: reportStatusLicenseTenant, ItemID: "item-1", SourceRevision: 1, ReportReleaseVersion: 2,
		RequiredChannels: "API", State: "APPLIED", SourceSnapshotID: "snap-1", AggregationRunID: "run-1",
		APIApplied: true, SourceFactsApplied: true,
	}
	if err := db.Create(&status).Error; err != nil {
		t.Fatal(err)
	}
	// 播种本身也走 gorm；计数清零后，"放行路径确实读到数据"才是本次请求的证据。
	atomic.StoreInt32(accesses, 0)
	license := &recordingLicenseCheck{}
	router := licenseTestRouter(db, testMachine.verifier, commercial.Check(license.allow))

	request := httptest.NewRequest(http.MethodGet, InternalReportStatusPath+"?item_id=item-1&source_revision=1&report_release_version=2", nil)
	request.Header.Set("Authorization", "Bearer "+testMachine.token(t))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	ops := license.operations()
	if len(ops) != 1 {
		t.Fatalf("license operations=%v want exactly one ReadHistory settlement", ops)
	}
	if ops[0] != commercial.ReadHistory {
		t.Fatalf("license operation=%q want %q", ops[0], commercial.ReadHistory)
	}
	if got := atomic.LoadInt32(accesses); got == 0 {
		t.Fatal("data access count=0: allowed request did not read the report store, denial-path counter may be vacuous")
	}
	var body struct {
		Code string `json:"code"`
		Data struct {
			TenantID        string   `json:"tenant_id"`
			ItemID          string   `json:"item_id"`
			State           string   `json:"state"`
			SchemaVersion   int      `json:"schema_version"`
			AppliedChannels []string `json:"applied_channels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v: %s", err, recorder.Body.String())
	}
	if body.Code != "OK" || body.Data.ItemID != "item-1" || body.Data.State != "APPLIED" || body.Data.SchemaVersion != 1 {
		t.Fatalf("body=%s want OK envelope with item-1/APPLIED/schema_version 1", recorder.Body.String())
	}
	if len(body.Data.AppliedChannels) == 0 {
		t.Fatalf("body=%s want applied_channels evidence", recorder.Body.String())
	}
	var audits int64
	if err := db.Table("bi_audit_outbox").Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit rows=%d want 1: successful machine read records exactly one read proof", audits)
	}
}

// TestEmbedProxyLicenseOperationIsMutateBusiness 验证原生模式嵌入代理的两条路径
// 都按 MutateBusiness 结算商业许可：组级中间件与处理器内校验都不把嵌入能力
// 当成历史读取。放行路径必须记录两次（组装级 + 处理器内），才能证明处理器
// 不是只依赖组级门禁。
func TestEmbedProxyLicenseOperationIsMutateBusiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		deny        bool
		wantCode    int
		wantBody    string
		wantChecked int
	}{
		{name: "group gate denies", deny: true, wantCode: http.StatusForbidden, wantBody: "COMMERCIAL_LICENSE_DENIED", wantChecked: 1},
		{name: "handler gate denies after group", wantCode: http.StatusServiceUnavailable, wantBody: "DASHBOARD_EMBED_DISABLED", wantChecked: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal := auth.Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]struct{}{}}
			license := &recordingLicenseCheck{}
			if tc.deny {
				license.deny = errors.New("license expired")
			}
			check := commercial.Check(license.allow)
			router := gin.New()
			base := router.Group("/data_analysis")
			api := base.Group("/api/v1", middleware.SessionAuth(embedTestAuthenticator{principal}, "session"), commercial.Middleware(check))
			mountDashboardEmbedRoutes(api, base, nil, Config{}, check)

			request := httptest.NewRequest(http.MethodGet, "/data_analysis/api/v1/embed-proxy/old-token/app.js", nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: "valid-session"})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantCode || !strings.Contains(recorder.Body.String(), tc.wantBody) {
				t.Fatalf("status=%d body=%s want %d %s", recorder.Code, recorder.Body.String(), tc.wantCode, tc.wantBody)
			}
			ops := license.operations()
			if len(ops) != tc.wantChecked {
				t.Fatalf("license operations=%v want %d settlements", ops, tc.wantChecked)
			}
			for _, op := range ops {
				if op != commercial.MutateBusiness {
					t.Fatalf("license operation=%q want %q", op, commercial.MutateBusiness)
				}
			}
		})
	}
}
