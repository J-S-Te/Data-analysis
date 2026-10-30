package importreport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// historyDB 构造独立内存历史投影库。
// 参数：t 为测试上下文。
// 返回值：仅含合成数据的聚合库。
func historyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { pool.Close() })
	if err = db.AutoMigrate(&aggregation.ImportHistory{}, &aggregation.ImportReportStatus{}, &aggregation.ImportReportRun{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestImportedHistoryScopeAndOriginalDates 防止范围泄漏及原日期/币种混加。
// 参数：t 为测试上下文。
// 返回值：无，所有分栏保留原年月、模式、币种及未归期。
func TestImportedHistoryScopeAndOriginalDates(t *testing.T) {
	db := historyDB(t)
	date := "2020-01-31"
	rows := []aggregation.ImportHistory{
		{TenantID: "t", ItemID: "a", StartDate: &date, Currency: "CNY", AmountMinor: 100, OwnerUserID: "u", OwnerOrgID: "o", ImportMode: "ARCHIVE_ONLY", ReportMode: "ARCHIVE_SEPARATE", Status: "completed"},
		{TenantID: "t", ItemID: "b", Currency: "USD", AmountMinor: 300, OwnerUserID: "v", OwnerOrgID: "p", ImportMode: "HISTORICAL_CLOSED", ReportMode: "HISTORICAL_SEPARATE", Status: "completed"},
		{TenantID: "t", ItemID: "c", StartDate: &date, Currency: "USD", AmountMinor: 250, OwnerUserID: "u", OwnerOrgID: "o", ImportMode: "ARCHIVE_ONLY", ReportMode: "ARCHIVE_SEPARATE", Status: "completed"},
		{TenantID: "other", ItemID: "x", Currency: "USD", AmountMinor: 900, OwnerUserID: "u", OwnerOrgID: "o"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, user        string
		orgs              []string
		count, unassigned int
		denied            bool
	}{
		{"TENANT", "u", nil, 3, 1, false}, {"ORG", "u", []string{"o"}, 2, 0, false}, {"TEAM", "u", []string{"p"}, 1, 1, false}, {"SELF", "u", nil, 2, 0, false}, {"NONE", "u", nil, 0, 0, true}, {"UNKNOWN", "u", nil, 0, 0, true}, {"ORG", "u", nil, 0, 0, true}, {"", "u", nil, 0, 0, true},
	} {
		ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t", UserID: tc.user, ScopeMode: tc.mode, OrganizationIDs: tc.orgs})
		result, err := History(ctx, db)
		if tc.denied {
			if err == nil {
				t.Fatalf("范围 %q 被接受", tc.mode)
			}
			continue
		}
		if err != nil || len(result.Periods) != tc.count || result.UnassignedCount != int64(tc.unassigned) {
			t.Fatalf("范围%s %+v %v", tc.mode, result, err)
		}
		for _, period := range result.Periods {
			if period.Period != "2020-01" && period.Period != "UNASSIGNED" {
				t.Fatalf("原日期丢失 %+v", period)
			}
			if period.AmountMinor != 100 && period.AmountMinor != 300 && period.AmountMinor != 250 {
				t.Fatal("跨租户或币种混加")
			}
		}
	}
}

// signedToken 使用临时测试密钥生成受限机器令牌。
// 参数：t 为测试上下文，key 为测试私钥，claims 为测试声明。
// 返回值：签名后的JWT。
func signedToken(t *testing.T, key ed25519.PrivateKey, claims map[string]any) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	raw, _ := json.Marshal(claims)
	payload := head + "." + base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
}

// TestStatusMachineTrustBoundary 验证状态接口的签名、租户及用途绑定。
// 参数：t 为测试上下文。
// 返回值：无，浏览器或跨用途令牌不获得任何版本证明。
func TestStatusMachineTrustBoundary(t *testing.T) {
	db := historyDB(t)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	path := filepath.Join(t.TempDir(), "public.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600)
	verifier, err := NewVerifier(VerifierOptions{Issuer: "platform", Audience: "analysis", PublicKeyPath: path, ClientID: "contract-reader", TenantID: "t", CallerEnvironment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/status", StatusHandler(db, verifier, nil))
	now := time.Now().Unix()
	claims := map[string]any{"iss": "platform", "aud": "analysis", "sub": "contract-reader", "oauth_client_id": "x", "tenant_id": "t", "application_code": "contract_management", "environment_code": "test", "token_use": "application", "scope": []string{"analysis.contract-import.report-status"}, "iat": now - 1, "nbf": now - 1, "exp": now + 60}
	for _, tc := range []struct {
		key    string
		value  any
		status int
	}{
		{"", nil, http.StatusNotFound}, {"tenant_id", "other", 401}, {"scope", []string{"dashboard.contract.read"}, 401}, {"application_code", "data_analysis", 401}, {"aud", "wrong", 401}, {"environment_code", "prod", 401}, {"token_use", "access_token", 401},
	} {
		copy := map[string]any{}
		for k, v := range claims {
			copy[k] = v
		}
		if tc.key != "" {
			copy[tc.key] = tc.value
		}
		req := httptest.NewRequest("GET", "/status?item_id=i&source_revision=1&report_release_version=1", nil)
		req.Header.Set("Authorization", "Bearer "+signedToken(t, key, copy))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s got %d body=%s", tc.key, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "contract_id") {
			t.Fatal("客户明细泄漏")
		}
	}
	st := aggregation.ImportReportStatus{TenantID: "t", ItemID: "i", SourceRevision: 1, ReportReleaseVersion: 1, RequiredChannels: "API_ONLY", State: "APPLIED", APIApplied: true, SourceSnapshotID: "synthetic", AggregationRunID: "import-1"}
	if err := db.Create(&st).Error; err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/status?item_id=i&source_revision=1&report_release_version=1&tenant_id=other", nil)
	req.Header.Set("Authorization", "Bearer "+signedToken(t, key, claims))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"schema_version":1`) || !strings.Contains(w.Body.String(), `"applied_channels":["API_APPLIED"]`) || !strings.Contains(w.Body.String(), `"tenant_id":"t"`) {
		t.Fatalf("actual successful proof %d %s", w.Code, w.Body.String())
	}

}

// TestHistoryMarksFailedPublicationStale 保证旧历史在新同步失败时明确陈旧。
// 参数：t 为测试上下文。
// 返回值：无，失败状态不得被查询时间或旧成功时间改写成最新。
func TestHistoryMarksFailedPublicationStale(t *testing.T) {
	db := historyDB(t)
	now := time.Now().UTC()
	run := aggregation.ImportReportRun{TenantID: "t", LastStatus: "FAILED", LastError: "SUMMARY_OR_SOURCE_UNAVAILABLE", LastSuccessAt: &now, PublishedSnapshotID: "snapshot", PublishedRunID: "import-1"}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t", ScopeMode: "TENANT"})
	value, err := History(ctx, db)
	if err != nil || !value.Stale || value.SyncStatus != "FAILED" || value.AggregationRunID != "import-1" || value.SyncedAt == nil {
		t.Fatalf("failed freshness %+v %v", value, err)
	}
}
