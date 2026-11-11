package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/infrastructure"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const acceptanceHTTPTenant = "dashboard-http-qa"

// acceptanceHTTPDB 应用真实迁移并打开仅由私有文件指定的隔离测试库。
// 参数：t 为测试上下文，配置只接受回环data_analysis_test_*_test数据库。
// 返回值：隔离数据库；缺配置跳过，边界、迁移或连接错误终止。
func acceptanceHTTPDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := os.Getenv("DASHBOARD_ACCEPTANCE_TEST_DSN_FILE")
	if path == "" {
		t.Skip("explicit dashboard acceptance private DSN required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private DSN failed")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") ||
		!strings.HasPrefix(cfg.DBName, "data_analysis_test_") || !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("explicit isolated loopback schema required")
	}
	if _, err := migration.Run(context.Background(), cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("isolated HTTP database connection failed")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

// acceptanceHTTPExec 执行确定的合成夹具SQL。
// 参数：t/db 为测试与隔离库，query/args 为固定SQL及合成数据。
// 返回值：无；SQL失败终止。
func acceptanceHTTPExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatal(err)
	}
}

// acceptanceHTTPSeed 创建分页、混币、真零报告和失败后财务旧快照。
// 参数：t/db 为测试与隔离聚合库，now 为当前可信时间。
// 返回值：无；数据创建失败终止。
func acceptanceHTTPSeed(t *testing.T, db *gorm.DB, now time.Time) {
	t.Helper()
	old := now.Add(-72 * time.Hour)
	for _, source := range []string{"contract_management", "contract_dashboard", "project_management", "project_dashboard", "settlement"} {
		status, stamp := "SUCCESS", now
		if source == "settlement" {
			status, stamp = "FAILED", old
		}
		acceptanceHTTPExec(t, db, `INSERT INTO sync_source
		(id,tenant_id,subsystem_code,db_host,db_schema,read_account_ref,enabled,last_status,
		last_success_at,last_run_at,created_at,updated_at) VALUES (?,?,?,'synthetic','qa','qa',1,?,?,?,?,?)`,
			fmt.Sprintf("http-qa-%d", len(source))+source[:3], acceptanceHTTPTenant, source,
			status, stamp, now, now, now)
	}
	for i := 1; i <= 21; i++ {
		id := fmt.Sprintf("qa-%02d", i)
		acceptanceHTTPExec(t, db, `INSERT INTO dim_contract
		(tenant_id,contract_id,origin_kind,contract_number,title,contract_type,service_type,status,
		amount_minor,currency,owner_user_id,owner_org_id,updated_at,start_date)
		VALUES (?,?,'NORMAL',?,?,'service','qa','active',10000,'CNY','qa-user','qa-org',?,'2026-11-01')`,
			acceptanceHTTPTenant, id, "QA-C-"+id, "验收合同"+id, now)
		row := aggregation.ProjectProjection{TenantID: acceptanceHTTPTenant, ID: id,
			SnapshotID: "http-qa-snapshot", Name: "验收项目" + id, Status: "实施中", Manager: "合成经理",
			OwnerOrgID: "qa-org", ServiceItems: 2, ReportStage: "RECTIFYING", InFlight: true,
			CreatedAt: now, UpdatedAt: now, SourceUpdatedAt: now}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	state := aggregation.ProjectProjectionState{TenantID: acceptanceHTTPTenant,
		ProjectionCode: aggregation.ProjectProjectionCode, Status: "SUCCESS", LastSuccessAt: &now,
		PublishedRunID: "http-qa-run", PublishedSnapshotID: "http-qa-snapshot",
		PublishedChecksumSHA256: strings.Repeat("a", 64), PublishedTotal: 21,
		PublishedSourceSnapshotAt: &now, PublishedSourceUpdatedAt: &now}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	acceptanceHTTPExec(t, db, `INSERT INTO api_contract_dashboard
	(tenant_id,snapshot_at,total_contracts,active_contracts,total_amount_minor,currency_totals_json)
	VALUES (?,?,21,21,0,'[{"currency":"CNY","amount_minor":210000,"contracts":20},{"currency":"USD","amount_minor":9000,"contracts":1}]')`, acceptanceHTTPTenant, now)
	acceptanceHTTPExec(t, db, `INSERT INTO api_project_dashboard
	(tenant_id,snapshot_at,project_count,in_flight_projects,service_items,status_counts_json,report_stage_counts_json)
	VALUES (?,?,21,21,42,'{"实施中":21}','{"RECTIFYING":21}')`, acceptanceHTTPTenant, now)
	for _, item := range []struct {
		currency string
		amount   int64
	}{{"CNY", 210000}, {"USD", 9000}} {
		acceptanceHTTPExec(t, db, `INSERT INTO fct_contract_signing
		(tenant_id,period_month,region,currency,sign_amount_minor,contract_count,metric_dict_version)
		VALUES (?,?,'qa',?,?,1,'qa')`, acceptanceHTTPTenant, now.Format("2006-01"), item.currency, item.amount)
	}
	acceptanceHTTPSeedBusiness(t, db, now, old)
}

// acceptanceHTTPSeedBusiness 写入独立预期的可用零报告及分币种财务旧快照。
// 参数：t/db 为测试与隔离库，now/old 为成功发布与停更的时间。
// 返回值：无；写入失败终止。
func acceptanceHTTPSeedBusiness(t *testing.T, db *gorm.DB, now, old time.Time) {
	t.Helper()
	for _, section := range []string{"report", "finance"} {
		value := domain.BusinessSnapshot{Available: true, TenantID: acceptanceHTTPTenant,
			SnapshotAt: now.Format(time.RFC3339Nano), SourceSnapshotAt: now.Format(time.RFC3339Nano),
			SourceCode: "project_management", MissingMetrics: []string{}, Basis: "explicit isolated fixture",
			Metrics: map[string]any{"active_reports": 0, "void_reports": 0, "status_counts": map[string]int{},
				"cycle_samples": 0, "cycle_p50_days": nil, "reviewed_reports": 0}}
		if section == "finance" {
			value.SourceCode, value.SourceSnapshotAt = "settlement", old.Format(time.RFC3339Nano)
			value.Freshness = domain.DataFreshness{Stale: true, StaleReason: "上次同步失败",
				SyncStatus: "FAILED", SyncedAt: old.Format(time.RFC3339Nano)}
			value.Metrics = map[string]any{"as_of_date": old.Format("2006-01-02"), "month": old.Format("2006-01"),
				"currencies": []map[string]any{
					{"currency": "CNY", "original_minor": 10000000, "allocated_minor": 2000000,
						"open_minor": 5000000, "over_90_minor": 1000000, "month_receipts_minor": 2000000, "collection_rate": 20},
					{"currency": "USD", "original_minor": 0, "allocated_minor": 0,
						"open_minor": 0, "over_90_minor": 0, "month_receipts_minor": 90000, "collection_rate": nil}}}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		acceptanceHTTPExec(t, db, `INSERT INTO business_dashboard_snapshot
		(tenant_id,section,snapshot_at,payload) VALUES (?,?,?,?)`, acceptanceHTTPTenant, section, now, string(raw))
	}
}

// TestDashboardAcceptanceHTTPArtifacts 经真实GORM与HTTP处理器生成脱敏浏览器响应。
// 参数：t 为隔离测试上下文；输出仅包含合成业务数据及合成主体，不代表真实登录。
// 返回值：无；API语义、租户隔离、分页或范围拒绝失败时终止。
func TestDashboardAcceptanceHTTPArtifacts(t *testing.T) {
	db := acceptanceHTTPDB(t)
	cleanup := func() {
		for _, table := range []string{"dim_contract", "dim_project", "project_projection_state", "sync_source",
			"api_contract_dashboard", "api_project_dashboard", "fct_contract_signing", "business_dashboard_snapshot"} {
			if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", acceptanceHTTPTenant).Error; err != nil {
				t.Error(err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	acceptanceHTTPSeed(t, db, time.Now().UTC())
	location := time.FixedZone("Asia/Shanghai", 8*3600)
	handler := NewHandler(application.NewService(infrastructure.NewGORMRepositoryWithLocation(db, location)))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	for path, serve := range map[string]gin.HandlerFunc{"contract": handler.Contract, "project": handler.Project,
		"contracts": handler.Contracts, "projects": handler.Projects, "trend": handler.Trend,
		"report": handler.Business("report"), "finance": handler.Business("finance")} {
		permission := "dashboard." + path + ".view"
		if path == "contracts" {
			permission = "dashboard.contract.view"
		}
		if path == "projects" {
			permission = "dashboard.project.view"
		}
		if path == "trend" {
			permission = "dashboard.overview.view"
		}
		router.GET("/dashboard/"+path, middleware.RequirePermission(permission), serve)
	}
	artifacts := map[string]map[string]json.RawMessage{}
	for _, scope := range []string{"TENANT", "ORG"} {
		principal := auth.Principal{TenantID: acceptanceHTTPTenant, UserID: "qa-user", ScopeMode: scope,
			OrganizationIDs: []string{"qa-org"}, Permissions: map[string]struct{}{}}
		for _, name := range []string{"contract", "project", "overview", "report", "finance"} {
			principal.Permissions["dashboard."+name+".view"] = struct{}{}
		}
		artifacts[scope] = map[string]json.RawMessage{}
		for _, path := range []string{"/dashboard/contract", "/dashboard/project", "/dashboard/report",
			"/dashboard/finance", "/dashboard/trend", "/dashboard/contracts?page=1&page_size=20",
			"/dashboard/contracts?page=2&page_size=20", "/dashboard/projects?page=1&page_size=20",
			"/dashboard/projects?page=2&page_size=20"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := http.StatusOK
			if scope == "ORG" && (path == "/dashboard/report" || path == "/dashboard/finance") {
				want = http.StatusForbidden
			}
			if response.Code != want || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("%s %s status=%d want=%d body=%s", scope, path, response.Code, want, response.Body.String())
			}
			artifacts[scope][path] = append(json.RawMessage(nil), response.Body.Bytes()...)
		}
	}
	acceptanceHTTPAssertArtifacts(t, artifacts)
	out := os.Getenv("DASHBOARD_ACCEPTANCE_ARTIFACT_FILE")
	if out == "" || !filepath.IsAbs(out) || !strings.Contains(filepath.ToSlash(out), "/.ai/qa/dashboard-completion-20261111/") {
		t.Fatal("explicit task-owned artifact path required")
	}
	raw, err := json.MarshalIndent(map[string]any{"boundary": "synthetic principal and isolated MySQL; real GORM/service/HTTP; no Keycloak login", "responses": artifacts}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// acceptanceHTTPAssertArtifacts 检查独立业务预期而非只检查HTTP成功码。
// 参数：t 为测试上下文，artifacts 为实际handler生成的两种范围响应。
// 返回值：无；分页、真零、混币或范围错误时失败。
func acceptanceHTTPAssertArtifacts(t *testing.T, artifacts map[string]map[string]json.RawMessage) {
	t.Helper()
	for _, key := range []string{"contracts", "projects"} {
		var envelope struct {
			Data struct {
				Items []json.RawMessage `json:"items"`
				Total int               `json:"total"`
				Page  int               `json:"page"`
			} `json:"data"`
		}
		if err := json.Unmarshal(artifacts["TENANT"]["/dashboard/"+key+"?page=2&page_size=20"], &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data.Total != 21 || envelope.Data.Page != 2 || len(envelope.Data.Items) != 1 {
			t.Fatalf("%s page2=%+v", key, envelope.Data)
		}
	}
	var report struct {
		Data domain.BusinessSnapshot `json:"data"`
	}
	if err := json.Unmarshal(artifacts["TENANT"]["/dashboard/report"], &report); err != nil ||
		!report.Data.Available || report.Data.Metrics["active_reports"] != float64(0) {
		t.Fatalf("report zero unavailable err=%v snapshot=%+v", err, report.Data)
	}
	var trend struct {
		Data []domain.TrendPoint `json:"data"`
	}
	if err := json.Unmarshal(artifacts["TENANT"]["/dashboard/trend"], &trend); err != nil ||
		len(trend.Data) != 1 || len(trend.Data[0].CurrencyTotals) != 2 || trend.Data[0].ContractAmountMinor != 0 {
		t.Fatalf("mixed trend collapsed err=%v values=%+v", err, trend.Data)
	}
}
