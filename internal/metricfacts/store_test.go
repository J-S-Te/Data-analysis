package metricfacts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// metricStoreFixture 构造独立内存数据库，不连接现行业务库。
// 参数：t为测试上下文。
// 返回值：手工建表的真实SQL连接。
func metricStoreFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE dashboard_metric_snapshot(tenant_id TEXT,source_code TEXT,source_snapshot_at DATETIME,collected_at DATETIME,payload_json TEXT,PRIMARY KEY(tenant_id,source_code))`,
		`CREATE TABLE sync_source(tenant_id TEXT,subsystem_code TEXT,enabled BOOLEAN,last_status TEXT,last_success_at DATETIME)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

// TestMetricStoreAtomicAndLatest 验证事务回滚、较旧快照与失败来源不能伪造最新成功。
// 参数：t为测试上下文。
// 返回值：无；回滚发布、旧快照覆盖或缺失表被解释为无数据时失败。
func TestMetricStoreAtomicAndLatest(t *testing.T) {
	db := metricStoreFixture(t)
	now := time.Date(2026, 11, 11, 12, 1, 0, 0, time.UTC)
	bundle, err := Decode(strings.NewReader(metricTestBundle()), "tenant-a", "contract_management", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return Save(tx, bundle, now) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sync_source VALUES(?,?,?,?,?)`, "tenant-a", "contract_metrics", true, "FAILED", now).Error; err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: db, Now: func() time.Time { return now }}
	out, err := store.Read(context.Background(), "tenant-a", "contract", "", Scope{Mode: "TENANT"})
	if err != nil || !out.Available || !out.Stale || out.DiscountKnown != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	older := bundle
	older.SnapshotAt = "2026-11-11T11:59:00Z"
	if err := db.Transaction(func(tx *gorm.DB) error { return Save(tx, older, now) }); err == nil {
		t.Fatal("older source published")
	}
	missing, err := store.Read(context.Background(), "tenant-b", "contract", "", Scope{Mode: "TENANT"})
	if err != nil || missing.Available {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	if err := db.Exec(`DROP TABLE dashboard_metric_snapshot`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "tenant-a", "contract", "", Scope{Mode: "TENANT"}); err == nil {
		t.Fatal("storage failure hidden")
	}
}

// TestMetricHTTPRequiresSectionPermissionAndScope 验证接口不能借其它看板权限读取财务。
// 参数：t为测试上下文。
// 返回值：无；权限、范围或严格查询边界失效时失败。
func TestMetricHTTPRequiresSectionPermissionAndScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := metricStoreFixture(t)
	for _, test := range []struct {
		query, permission, mode string
		status                  int
	}{
		{"section=contract", "dashboard.contract.view", "TENANT", 200},
		{"section=finance", "dashboard.contract.view", "TENANT", 403},
		{"section=contract", "dashboard.contract.view", "", 403},
		{"section=contract&section=finance", "dashboard.contract.view", "TENANT", 400},
		{"section=contract&tenant_id=other", "dashboard.contract.view", "TENANT", 400},
		{"section=contract&period=2026-13", "dashboard.contract.view", "TENANT", 400},
	} {
		router := gin.New()
		group := router.Group("/api")
		(&Handler{Store: &Store{DB: db}, IntegrationEnabled: true}).Register(group)
		request := httptest.NewRequest(http.MethodGet, "/api/dashboard/metrics?"+test.query, nil)
		request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
			TenantID: "tenant-a", UserID: "u1", ScopeMode: test.mode,
			Permissions: map[string]struct{}{test.permission: {}},
		}))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("query=%s status=%d body=%s", test.query, recorder.Code, recorder.Body.String())
		}
		if recorder.Code == 200 {
			var result struct {
				Data Summary `json:"data"`
			}
			if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Data.Available {
				t.Fatal("unpublished source reported available")
			}
		}
	}
}
