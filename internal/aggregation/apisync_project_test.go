package aggregation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"
)

// projectDashboardTestDB 建一张与迁移 000034 对齐的 api_project_dashboard 表，
// 让接口同步的持久化形状可以在 sqlite 上断言，不依赖真实 MySQL。
func projectDashboardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newScanTestDB(t)
	if err := db.Exec(`CREATE TABLE api_project_dashboard (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id TEXT, snapshot_at DATETIME, project_count INTEGER,
		in_flight_projects INTEGER, risk_projects INTEGER, service_items INTEGER,
		status_counts_json TEXT, report_stage_counts_json JSON, source TEXT)`).Error; err != nil {
		t.Fatalf("create api_project_dashboard: %v", err)
	}
	return db
}

func projectDashboardRunner(t *testing.T, db *gorm.DB, url string) *APISyncRunner {
	t.Helper()
	return &APISyncRunner{
		db: db,
		options: APISyncOptions{
			ProjectInternalURL: url,
			ProjectCredential:  MachineCredential{ClientID: "project-reader"},
			TenantID:           "tenant-1",
			HTTPTimeout:        time.Second,
		},
		tokens: map[string]cachedMachineToken{"project-reader": {value: "machine-token", expiresAt: time.Now().Add(time.Hour)}},
	}
}

type storedProjectDashboard struct {
	StatusCountsJSON      *string `gorm:"column:status_counts_json"`
	ReportStageCountsJSON *string `gorm:"column:report_stage_counts_json"`
}

func latestStoredProjectDashboard(t *testing.T, db *gorm.DB) storedProjectDashboard {
	t.Helper()
	var row storedProjectDashboard
	if err := db.Table("api_project_dashboard").Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("read api_project_dashboard: %v", err)
	}
	return row
}

// TestSyncProjectDashboardPersistsReportStageCountsThreeStates 固定三种上游形态：
// 键缺失写 SQL NULL（未采集）、空对象写 {}（已采集无分桶）、有值写 JSON。
// status_counts_json 的既有行为保持不变。
func TestSyncProjectDashboardPersistsReportStageCountsThreeStates(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantAbsent bool
		wantJSON   map[string]int
	}{
		{
			name:       "legacy pm without field",
			payload:    `{"data":{"tenant_id":"tenant-1","project_count":2,"status_counts":{"已完成":1,"待实施":1}}}`,
			wantAbsent: true,
		},
		{
			name:     "collected empty counts",
			payload:  `{"data":{"tenant_id":"tenant-1","project_count":0,"status_counts":{},"report_stage_counts":{}}}`,
			wantJSON: map[string]int{},
		},
		{
			name:     "collected counts",
			payload:  `{"data":{"tenant_id":"tenant-1","project_count":3,"status_counts":{"待实施":3},"report_stage_counts":{"RECTIFYING":2,"":1}}}`,
			wantJSON: map[string]int{"RECTIFYING": 2, "": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := projectDashboardTestDB(t)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if got := request.Header.Get("X-DA-Tenant-ID"); got != "tenant-1" {
					t.Errorf("X-DA-Tenant-ID = %q", got)
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(response, tc.payload)
			}))
			defer server.Close()
			runner := projectDashboardRunner(t, db, server.URL)
			if err := runner.SyncProjectDashboard(context.Background()); err != nil {
				t.Fatalf("SyncProjectDashboard() error = %v", err)
			}
			row := latestStoredProjectDashboard(t, db)
			if row.StatusCountsJSON == nil {
				t.Fatal("status_counts_json must keep its existing non-null behavior")
			}
			if tc.wantAbsent {
				if row.ReportStageCountsJSON != nil {
					t.Fatalf("missing report_stage_counts stored as %q, want SQL NULL", *row.ReportStageCountsJSON)
				}
				return
			}
			if row.ReportStageCountsJSON == nil {
				t.Fatal("collected report_stage_counts stored as SQL NULL")
			}
			var stored map[string]int
			if err := json.Unmarshal([]byte(*row.ReportStageCountsJSON), &stored); err != nil {
				t.Fatalf("stored report_stage_counts %q: %v", *row.ReportStageCountsJSON, err)
			}
			if len(stored) != len(tc.wantJSON) {
				t.Fatalf("stored report_stage_counts = %v, want %v", stored, tc.wantJSON)
			}
			for stage, count := range tc.wantJSON {
				if stored[stage] != count {
					t.Fatalf("stored report_stage_counts = %v, want %v", stored, tc.wantJSON)
				}
			}
		})
	}
}

// TestSyncProjectDashboardRejectsCrossTenantResponse 保持既有租户校验：
// 跨租户响应不得落库（新增列不能顺带放宽这条边界）。
func TestSyncProjectDashboardRejectsCrossTenantResponse(t *testing.T) {
	db := projectDashboardTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(response, `{"data":{"tenant_id":"tenant-2","report_stage_counts":{"RECTIFYING":1}}}`)
	}))
	defer server.Close()
	runner := projectDashboardRunner(t, db, server.URL)
	if err := runner.SyncProjectDashboard(context.Background()); err == nil {
		t.Fatal("cross-tenant dashboard persisted")
	}
	var count int64
	if err := db.Table("api_project_dashboard").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cross-tenant snapshot rows = %d", count)
	}
}
