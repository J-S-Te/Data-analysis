package metricfacts

import (
	"testing"
	"time"
)

// TestUnknownHistoricalSpendDoesNotBlockConfirmedFacts 验证旧付款缺快照单列未知。
// 参数：t为测试上下文。
// 返回值：无；未知金额混入确认或阻断合法完整快照时失败。
func TestUnknownHistoricalSpendDoesNotBlockConfirmedFacts(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	amount, tax := int64(120), int64(0)
	bundle := Bundle{SchemaVersion: 1, TenantID: "t", SourceCode: "settlement",
		SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "真实确认与未知旧付款分列",
		Rows: []Row{
			{Kind: "recognized_income", RecordID: "known", Period: "2026-11",
				Status: "CONFIRMED", Currency: "CNY", AmountMinor: &amount, TaxMinor: &tax},
			{Kind: "reimbursement_unknown", RecordID: "old-payment", Period: "2026-11",
				Status: "UNKNOWN", Currency: "CNY", AmountMinor: &amount,
				UnknownReason: "MISSING_APPROVAL_SNAPSHOT"},
		}}
	if err := Validate(bundle, "t", "settlement", now); err != nil {
		t.Fatal(err)
	}
	out, err := Summarize(bundle, "finance", "2026-11", Scope{Mode: "TENANT"})
	if err != nil || out.Unknown != 1 || len(out.Currencies) != 1 ||
		out.Currencies[0].IncomeMinor != 120 || out.Currencies[0].SpendMinor != 0 ||
		out.Currencies[0].PrepaymentMinor != 0 {
		t.Fatalf("incorrect accounting split: %+v %v", out, err)
	}
	bundle.Rows[1].UnknownReason = ""
	if Validate(bundle, "t", "settlement", now) == nil {
		t.Fatal("unknown without reason accepted")
	}
}
