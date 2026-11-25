package metricfacts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDecodeProjectAutoConfirmedFacts 核对项目自动确认字段兼容旧快照且保留严格校验。
// 参数：t 为包含合成项目来源响应的测试上下文。
// 返回值：无。
func TestDecodeProjectAutoConfirmedFacts(t *testing.T) {
	const legacy = `{"code":"OK","data":{"schema_version":1,"tenant_id":"tenant-a",
	"source_code":"project_management","snapshot_at":"2026-11-11T12:00:00Z",
	"complete":true,"basis":"确认时长和排班容量","rows":[
	{"kind":"person_utilization","record_id":"p1","organization_id":"org-a",
	"identity_id":"u1","project_id":"","period":"2026-11","status":"CONFIRMED",
	"actual_minutes":60,"capacity_minutes":240,"pending_count":0,"unknown_count":0}]}}`
	const fields = `"auto_confirmed_minutes":30,"auto_confirmed_count":1`
	now := time.Date(2026, 11, 11, 12, 1, 0, 0, time.UTC)
	for _, test := range []struct {
		name, addedFields string
		wantErr           bool
	}{
		{name: "legacy"},
		{name: "auto_confirmed", addedFields: "," + fields},
		{name: "unknown_field", addedFields: "," + fields + `,"unexpected_metric":1`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(legacy, `"unknown_count":0`, `"unknown_count":0`+test.addedFields, 1)
			bundle, err := Decode(strings.NewReader(raw), "tenant-a", "project_management", now)
			if test.wantErr {
				if err != ErrInvalid {
					t.Fatalf("unknown metric should fail strict decoding: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if test.addedFields != "" && (!strings.Contains(string(snapshot), `"auto_confirmed_minutes":30`) ||
				!strings.Contains(string(snapshot), `"auto_confirmed_count":1`)) {
				t.Fatal("project snapshot dropped auto-confirmed facts")
			}
		})
	}
}

// metricTestBundle 创建与当前时间无关的完整合同来源样本。
// 参数：无。
// 返回值：包含八折和未知合同的原始机器响应。
func metricTestBundle() string {
	return `{"code":"OK","data":{"schema_version":1,"tenant_id":"tenant-a",
	"source_code":"contract_management","snapshot_at":"2026-11-11T12:00:00Z",
	"source_updated_at":"2026-11-11T11:00:00Z","collection_started_at":"2026-11-01T00:00:00Z",
	"complete":true,"basis":"已发布价格与冻结合同","rows":[
	{"kind":"discount","record_id":"c1","organization_id":"org-a","identity_id":"u1",
	"project_id":"","period":"2026-11","status":"KNOWN","currency":"CNY",
	"amount_minor":8000,"standard_minor":10000,"bucket":"FROM_80_TO_90"},
	{"kind":"discount","record_id":"c2","organization_id":"org-b","identity_id":"u2",
	"project_id":"","period":"2026-11","status":"UNKNOWN","unknown_reason":"历史无定价基准"}]}}`
}

// TestDecodeMetricBundleBoundary 验证错误来源与畸形完整性不能发布。
// 参数：t 为测试上下文。
// 返回值：无；跨租户、重复事实、错桶或错误水位被接受时失败。
func TestDecodeMetricBundleBoundary(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 1, 0, 0, time.UTC)
	if _, err := Decode(strings.NewReader(metricTestBundle()), "tenant-a", "contract_management", now); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, from, to string }{
		{"tenant", `"tenant-a"`, `"tenant-b"`},
		{"source", `"contract_management"`, `"settlement"`},
		{"incomplete", `"complete":true`, `"complete":false`},
		{"bucket", `"FROM_80_TO_90"`, `"LT_80"`},
		{"duplicate", `"record_id":"c2"`, `"record_id":"c1"`},
		{"future", `2026-11-11T12:00:00Z`, `2026-11-12T12:00:00Z`},
		{"overflow", `"amount_minor":8000`, `"amount_minor":9007199254740992`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(metricTestBundle(), test.from, test.to, 1)
			if _, err := Decode(strings.NewReader(raw), "tenant-a", "contract_management", now); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

// TestSummarizeMetricFactsScope 验证范围先于统计且未知不冒充无折扣。
// 参数：t 为测试上下文。
// 返回值：无；组织、本人或未知范围泄露其它事实时失败。
func TestSummarizeMetricFactsScope(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 1, 0, 0, time.UTC)
	bundle, err := Decode(strings.NewReader(metricTestBundle()), "tenant-a", "contract_management", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		scope          Scope
		known, unknown int64
	}{
		{Scope{Mode: "TENANT"}, 1, 1},
		{Scope{Mode: "ORG", OrganizationIDs: []string{"org-a"}}, 1, 0},
		{Scope{Mode: "SELF", UserID: "u2"}, 0, 1},
		{Scope{Mode: "TEAM", TeamIDs: []string{"org-b"}}, 0, 1},
	} {
		out, err := Summarize(bundle, "contract", "", test.scope)
		if err != nil || out.DiscountKnown != test.known || out.DiscountUnknown != test.unknown {
			t.Fatalf("scope=%s summary=%+v err=%v", test.scope.Mode, out, err)
		}
	}
	if _, err := Summarize(bundle, "contract", "", Scope{}); err == nil {
		t.Fatal("missing trusted scope accepted")
	}
}

// metricInt 返回可空指标字段的整数地址。
// 参数：value 为测试中的明确事实金额或分钟。
// 返回值：独立整数指针。
func metricInt(value int64) *int64 { return &value }

// TestSummarizeRecognizedFactsPreservesCurrencyAndReversal 验证确认冲回与现金不混算。
// 参数：t 为测试上下文。
// 返回值：无；跨币种合计、负确认丢失或待审计入金额时失败。
func TestSummarizeRecognizedFactsPreservesCurrencyAndReversal(t *testing.T) {
	bundle := Bundle{SourceCode: "settlement", Rows: []Row{
		{Kind: "recognized_income", Period: "2026-11", OrganizationID: "a", Currency: "CNY", AmountMinor: metricInt(10000)},
		{Kind: "recognized_income", Period: "2026-11", OrganizationID: "a", Currency: "CNY", AmountMinor: metricInt(-10000)},
		{Kind: "recognized_cost", Period: "2026-11", OrganizationID: "a", Currency: "USD", AmountMinor: metricInt(4000)},
		{Kind: "prepayment_spend", Period: "2026-11", OrganizationID: "a", Currency: "CNY", AmountMinor: metricInt(5000)},
		{Kind: "recognition_pending", Period: "2026-11", OrganizationID: "a"},
		{Kind: "recognized_cost", Period: "2026-11", OrganizationID: "b", Currency: "CNY", AmountMinor: metricInt(9999)},
	}}
	out, err := Summarize(bundle, "finance", "2026-11", Scope{Mode: "ORG", OrganizationIDs: []string{"a"}})
	if err != nil || len(out.Currencies) != 2 || out.Pending != 1 {
		t.Fatalf("summary=%+v err=%v", out, err)
	}
	if out.Currencies[0].Currency != "CNY" || out.Currencies[0].IncomeMinor != 0 ||
		out.Currencies[0].CostMinor != 0 || out.Currencies[0].PrepaymentMinor != 5000 ||
		out.Currencies[1].Currency != "USD" || out.Currencies[1].CostMinor != 4000 {
		t.Fatalf("currency or recognition semantics changed: %+v", out.Currencies)
	}
}

// TestSummarizeReviewAndUtilizationNullDenominator 验证首次终决分母和无容量的空值。
// 参数：t 为测试上下文。
// 返回值：无；待审/旧报告计失败或零分母伪造比率时失败。
func TestSummarizeReviewAndUtilizationNullDenominator(t *testing.T) {
	bundle := Bundle{SourceCode: "project_management", Rows: []Row{
		{Kind: "report_review", Status: "PASSED", Period: "2026-11"},
		{Kind: "report_review", Status: "RETURNED", Period: "2026-11"},
		{Kind: "report_review", Status: "PENDING"},
		{Kind: "report_review", Status: "UNKNOWN"},
		{Kind: "person_utilization", Period: "2026-11", ActualMinutes: metricInt(0),
			CapacityMinutes: metricInt(0), PendingCount: metricInt(0), UnknownCount: metricInt(0)},
	}}
	report, err := Summarize(bundle, "report", "2026-11", Scope{Mode: "TENANT"})
	if err != nil || report.FirstReviewed != 2 || report.FirstPassed != 1 ||
		report.FirstPassRate == nil || *report.FirstPassRate != 50 || report.Pending != 1 || report.Unknown != 1 {
		t.Fatalf("review=%+v err=%v", report, err)
	}
	project, err := Summarize(bundle, "project", "2026-11", Scope{Mode: "TENANT"})
	if err != nil || project.Person.Rate != nil || project.Equipment.Rate != nil {
		t.Fatalf("utilization=%+v err=%v", project, err)
	}
}

// TestContractUnknownPeriodIsNotAnyMonth 验证无生效月合同不进入任意所选月。
// 参数：t为包含已知和未知基准合同的测试上下文。
// 返回值：无；报告例外被改变或合同跨月污染时失败。
func TestContractUnknownPeriodIsNotAnyMonth(t *testing.T) {
	bundle := Bundle{SourceCode: "contract_management", Rows: []Row{
		{Kind: "discount", RecordID: "known-no-date", Status: "KNOWN", Bucket: "AT_100"},
		{Kind: "discount", RecordID: "unknown-no-date", Status: "UNKNOWN"},
		{Kind: "discount", RecordID: "dated", Status: "KNOWN", Period: "2026-11", Bucket: "AT_100"},
	}}
	for _, period := range []string{"2026-10", "2026-11"} {
		got, err := Summarize(bundle, "contract", period, Scope{Mode: "TENANT"})
		known := int64(0)
		if period == "2026-11" {
			known = 1
		}
		if err != nil || got.DiscountKnown != known || got.DiscountUnknown != 0 || got.Unknown != 2 {
			t.Errorf("%s: %+v %v", period, got, err)
		}
	}
	all, err := Summarize(bundle, "contract", "", Scope{Mode: "TENANT"})
	if err != nil || all.DiscountKnown != 2 || all.DiscountUnknown != 1 {
		t.Fatalf("all contracts changed: %+v %v", all, err)
	}
	bundle.SourceCode = "project_management"
	bundle.Rows = []Row{{Kind: "report_review", Status: "UNKNOWN"}, {Kind: "report_review", Status: "PENDING"}}
	report, err := Summarize(bundle, "report", "2026-11", Scope{Mode: "TENANT"})
	if err != nil || report.Unknown != 1 || report.Pending != 1 || report.FirstReviewed != 0 {
		t.Fatalf("report coverage changed: %+v %v", report, err)
	}
}
