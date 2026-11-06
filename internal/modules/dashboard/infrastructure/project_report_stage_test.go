package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// stringPtr 返回字符串地址，用来区分 SQL NULL（nil）与空 JSON 文本。
func stringPtr(value string) *string { return &value }

// TestTenantProjectSnapshotReportStageCounts 覆盖 TENANT 读路径的三态：
// NULL=未采集（nil）、JSON=已采集（含空对象）、非法 JSON=既有 invalid snapshot 错误。
func TestTenantProjectSnapshotReportStageCounts(t *testing.T) {
	cases := []struct {
		name       string
		reportJSON *string
		wantNil    bool
		want       map[string]int
		wantErr    bool
	}{
		{name: "legacy null means not collected", wantNil: true},
		{name: "collected empty object", reportJSON: stringPtr("{}"), want: map[string]int{}},
		{name: "collected counts", reportJSON: stringPtr(`{"RECTIFYING":2,"":1}`), want: map[string]int{"RECTIFYING": 2, "": 1}},
		{name: "invalid json keeps error semantics", reportJSON: stringPtr("{broken"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := freshnessTestDB(t)
			tenant := "tenant-report-stage"
			storedAt := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05.000")
			if tc.reportJSON == nil {
				if err := db.Exec(`INSERT INTO api_project_dashboard
					(tenant_id, snapshot_at, project_count, in_flight_projects, risk_projects, service_items, status_counts_json)
					VALUES (?, ?, 3, 1, 1, 3, '{"待实施":3}')`, tenant, storedAt).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := db.Exec(`INSERT INTO api_project_dashboard
				(tenant_id, snapshot_at, project_count, in_flight_projects, risk_projects, service_items, status_counts_json, report_stage_counts_json)
				VALUES (?, ?, 3, 1, 1, 3, '{"待实施":3}', ?)`, tenant, storedAt, *tc.reportJSON).Error; err != nil {
				t.Fatal(err)
			}
			repository := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "boss", ScopeMode: "TENANT"})
			snapshot, available, err := repository.LatestProject(ctx, tenant)
			if tc.wantErr {
				if !errors.Is(err, application.ErrInvalidProjectSnapshot) {
					t.Fatalf("error = %v, want invalid project snapshot", err)
				}
				return
			}
			if err != nil || !available {
				t.Fatalf("available=%v err=%v", available, err)
			}
			if snapshot.StatusCounts["待实施"] != 3 {
				t.Fatalf("status counts = %v", snapshot.StatusCounts)
			}
			if tc.wantNil {
				if snapshot.ReportStageCounts != nil {
					t.Fatalf("NULL must stay nil (not collected), got %v", snapshot.ReportStageCounts)
				}
				return
			}
			if snapshot.ReportStageCounts == nil {
				t.Fatal("collected counts decoded as nil")
			}
			if len(snapshot.ReportStageCounts) != len(tc.want) {
				t.Fatalf("report stage counts = %v, want %v", snapshot.ReportStageCounts, tc.want)
			}
			for stage, count := range tc.want {
				if snapshot.ReportStageCounts[stage] != count {
					t.Fatalf("report stage counts = %v, want %v", snapshot.ReportStageCounts, tc.want)
				}
			}
		})
	}
}

// TestScopedProjectReportStageCountsGroupsAndKeepsLegacyBucket 覆盖 ORG 实时聚合：
// 分组与 status 聚合同源，旧行空串保留 ” 桶，范围未命中时是已采集的空映射。
func TestScopedProjectReportStageCountsGroupsAndKeepsLegacyBucket(t *testing.T) {
	db := freshnessTestDB(t)
	tenant := "tenant-report-stage-scoped"
	publishTestProjection(t, db, tenant, 3)
	now := time.Now().UTC().Add(-2 * time.Hour)
	for _, row := range []struct {
		id, org, status, stage string
	}{
		{"p-1", "org-a", "待实施", "RECTIFYING"},
		{"p-2", "org-a", "待实施", "RECTIFYING"},
		{"p-3", "org-a", "已完成", ""},
	} {
		if err := db.Exec(`INSERT INTO dim_project (tenant_id,project_id,status,report_stage,owner_org_id,snapshot_id,risk,in_flight,service_items,source_updated_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			tenant, row.id, row.status, row.stage, row.org, "snap", false, false, 1, now, now, now).Error; err != nil {
			t.Fatal(err)
		}
	}
	repository := NewGORMRepositoryWithLocation(db, scopedContractBusinessLocation(t))
	inOrg := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "director", ScopeMode: "ORG", OrganizationIDs: []string{"org-a"}})
	snapshot, available, err := repository.LatestProject(inOrg, tenant)
	if err != nil || !available {
		t.Fatalf("available=%v err=%v", available, err)
	}
	if snapshot.ProjectCount != 3 || snapshot.StatusCounts["待实施"] != 2 || snapshot.StatusCounts["已完成"] != 1 {
		t.Fatalf("project=%+v", snapshot)
	}
	want := map[string]int{"RECTIFYING": 2, "": 1}
	if len(snapshot.ReportStageCounts) != len(want) {
		t.Fatalf("report stage counts = %v, want %v", snapshot.ReportStageCounts, want)
	}
	for stage, count := range want {
		if snapshot.ReportStageCounts[stage] != count {
			t.Fatalf("report stage counts = %v, want %v", snapshot.ReportStageCounts, want)
		}
	}
	// 已发布投影下的未命中范围是“可信的空”，不是未采集：report_stage 必须是
	// 非 nil 空映射，前端据此显示 0 而不是“未采集”。
	unknownOrg := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "director", ScopeMode: "ORG", OrganizationIDs: []string{"org-x"}})
	empty, available, err := repository.LatestProject(unknownOrg, tenant)
	if err != nil || !available {
		t.Fatalf("available=%v err=%v", available, err)
	}
	if empty.ReportStageCounts == nil || len(empty.ReportStageCounts) != 0 {
		t.Fatalf("unmatched scope report stage counts = %#v, want collected empty map", empty.ReportStageCounts)
	}
}
