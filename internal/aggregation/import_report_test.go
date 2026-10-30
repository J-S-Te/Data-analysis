package aggregation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reportFixture 构造完全合成的历史合同发布摘要。
// 参数：无。
// 返回值：两项需要不同发布通道的完整摘要。
func reportFixture() ImportReportSnapshot {
	return ImportReportSnapshot{SchemaVersion: 1, TenantID: "t", SourceSnapshotID: strings.Repeat("a", 64), Complete: true, Items: []ImportReportItem{
		{ItemID: "i1", SourceRevision: 1, ReportReleaseVersion: 1, RequiredChannels: "API_ONLY", ImportMode: "ARCHIVE_ONLY", ReportMode: "ARCHIVE_SEPARATE", ContractID: "c1", ContractVersion: 1, Status: "completed", StartDate: nil, OwnerUserID: "u", OwnerOrgID: "o", AmountMinor: 100, Currency: "CNY", CreatedAt: "2020-01-01T00:00:00Z"},
		{ItemID: "i2", SourceRevision: 2, ReportReleaseVersion: 3, RequiredChannels: "API_AND_SOURCE_FACTS", ImportMode: "HISTORICAL_CLOSED", ReportMode: "HISTORICAL_SEPARATE", ContractID: "c2", ContractVersion: 5, Status: "completed", OwnerUserID: "u", OwnerOrgID: "o", AmountMinor: 200, Currency: "USD", CreatedAt: "2020-01-01T00:00:00Z"},
	}}
}

// reportDB 建立只供本测试使用的隔离内存聚合库。
// 参数：t 为测试上下文。
// 返回值：含报告发布表的执行器。
func reportDB(t *testing.T) *Runner {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&ImportReportRun{}, &ImportReportStatus{}, &ImportHistory{}); err != nil {
		t.Fatal(err)
	}
	return &Runner{aggDB: db, tenantID: "t"}
}

// TestImportReportReleaseWatermark 验证逐项精确版本发布及迟到/失败/部分覆盖。
// 参数：t 为测试上下文。
// 返回值：无，任何渠道不能以源放行或最大版本冒充已应用。
func TestImportReportReleaseWatermark(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	gen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, nil, "SOURCE_NOT_CONFIGURED"); err != nil {
		t.Fatal(err)
	}
	a, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i1", 1, 1)
	if err != nil || a.State != "APPLIED" || len(a.AppliedChannels) != 1 {
		t.Fatalf("API-only %+v %v", a, err)
	}
	b, _ := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if b.State != "WAITING_SOURCE" || b.SourceFactsApplied {
		t.Fatalf("缺DSN假完成: %+v", b)
	}
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, map[string]ImportReportItem{}, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if b.State != "WAITING_SOURCE" {
		t.Fatalf("部分覆盖 %+v", b)
	}
	old := gen
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, old, s, nil, ""); err == nil {
		t.Fatal("迟到发布被接受")
	}
	if err = r.publishImportReport(ctx, gen, s, nil, "SOURCE_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}
	b, _ = ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if b.State != "BLOCKED" {
		t.Fatalf("源失败 %+v", b)
	}
	gen, _ = r.beginImportReport(ctx)
	if err = r.publishImportReport(ctx, gen, s, map[string]ImportReportItem{"i2": s.Items[1]}, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if b.State != "APPLIED" || len(b.AppliedChannels) != 2 || b.AggregationRunID == "" {
		t.Fatalf("同版本恢复 %+v", b)
	}
	if _, err = ReadImportReportStatus(ctx, r.aggDB, "other", "i2", 2, 3); err == nil {
		t.Fatal("跨租户水位泄漏")
	}
	if _, err = ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 1, 3); err == nil {
		t.Fatal("错误修订得到成功")
	}
	gen, _ = r.beginImportReport(ctx)
	s.Items[1].AmountMinor++
	if err = r.publishImportReport(ctx, gen, s, nil, ""); err == nil {
		t.Fatal("同版本异内容被覆盖")
	}
}

// TestImportReportSnapshotRejectsIncomplete 拒绝缺完整标记及不合法摘要。
// 参数：t 为测试上下文。
// 返回值：无，错误摘要不得覆盖已发布投影。
func TestImportReportSnapshotRejectsIncomplete(t *testing.T) {
	for _, change := range []func(*ImportReportSnapshot){func(s *ImportReportSnapshot) { s.Complete = false }, func(s *ImportReportSnapshot) { s.TenantID = "other" }, func(s *ImportReportSnapshot) { s.Items = append(s.Items, s.Items[0]) }, func(s *ImportReportSnapshot) { s.Items[0].RequiredChannels = "NONE" }, func(s *ImportReportSnapshot) { s.Items[0].ReportMode = "INCLUDE_RESTATED" }} {
		s := reportFixture()
		change(&s)
		wire, _ := json.Marshal(map[string]any{"code": "OK", "data": s})
		if _, err := readImportReport(strings.NewReader(string(wire)), "t"); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
}

// TestImportReportHTTPSyncRejectsIncompleteAndUsesDedicatedScope 验证真实HTTP读取的用途与原子覆盖边界。
// 参数：t 为测试上下文；HTTP服务只提供合成摘要和合成令牌。
// 返回值：无，不完整/超限摘要不改变既有投影。
func TestImportReportHTTPSyncRejectsIncompleteAndUsesDedicatedScope(t *testing.T) {
	r := reportDB(t)
	snapshot := reportFixture()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			req.ParseForm()
			id, secret, ok := req.BasicAuth()
			if !ok || id != "import-reader" || secret != "synthetic" || req.Form.Get("scope") != "contract.import.report.read" {
				t.Error("wrong purpose credential")
			}
			token := compactTestJWT(`{"sub":"import-reader","token_use":"application","scope":["contract.import.report.read"]}`)
			json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 300})
			return
		}
		calls++
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing machine authorization")
		}
		json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": snapshot})
	}))
	defer server.Close()
	api := NewAPISyncRunner(r.aggDB, APISyncOptions{TenantID: "t", MachineTokenURL: server.URL + "/token", ContractCredential: MachineCredential{ClientID: "import-reader", ClientSecret: "synthetic", Scope: "contract.import.report.read"}})
	if err := r.SyncImportReport(context.Background(), api, server.URL+"/source"); err != nil {
		t.Fatal(err)
	}
	var before int64
	r.aggDB.Table("contract_import_history").Count(&before)
	snapshot.Complete = false
	if err := r.SyncImportReport(context.Background(), api, server.URL+"/source"); err == nil {
		t.Fatal("incomplete snapshot published")
	}
	var after int64
	r.aggDB.Table("contract_import_history").Count(&after)
	if before != 2 || after != 2 || calls != 2 {
		t.Fatal("failed summary altered history")
	}
	snapshot = reportFixture()
	for len(snapshot.Items) <= 1000 {
		snapshot.Items = append(snapshot.Items, snapshot.Items[0])
	}
	raw, _ := json.Marshal(map[string]any{"code": "OK", "data": snapshot})
	if _, err := readImportReport(bytes.NewReader(raw), "t"); err == nil {
		t.Fatal("oversized item set accepted")
	}
}

// TestImportReportOriginalDateTimezone 验证时间戳时区不会改变业务月份或阻塞等价事实。
// 参数：t 为测试上下文。
// 返回值：无，原DATE不受创建时间跨月影响，等价UTC事实可以完成。
func TestImportReportOriginalDateTimezone(t *testing.T) {
	r := reportDB(t)
	s := reportFixture()
	date := "2020-01-31"
	s.Items[1].StartDate = &date
	s.Items[1].CreatedAt = "2020-02-01T00:00:00+08:00"
	fact := s.Items[1]
	fact.CreatedAt = "2020-01-31T16:00:00Z"
	generation, _ := r.beginImportReport(context.Background())
	if err := r.publishImportReport(context.Background(), generation, s, map[string]ImportReportItem{"i2": fact}, ""); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadImportReportStatus(context.Background(), r.aggDB, "t", "i2", 2, 3)
	if st.State != "APPLIED" {
		t.Fatalf("equivalent timestamp blocked %+v", st)
	}
	var row ImportHistory
	if err := r.aggDB.Where("tenant_id='t' AND item_id='i2'").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.StartDate == nil || *row.StartDate != date {
		t.Fatalf("original date changed %+v", row.StartDate)
	}
}
