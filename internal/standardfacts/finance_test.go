package standardfacts

import (
	"strings"
	"testing"
	"time"
)

// financeFixture 构造不依赖任何业务子系统的标准经营事实文件。
// 参数：无。
// 返回值：具有多币种、账龄和跨月回款边界的完整事实快照。
func financeFixture() Document {
	return Document{SchemaVersion: 1, BatchID: "batch-001", AsOfDate: "2026-11-04", Complete: true,
		Receivables: []Receivable{
			{ExternalID: "r1", Currency: "CNY", OriginalMinor: 10000, AllocatedMinor: 4000, DueDate: "2026-08-01"},
			{ExternalID: "r2", Currency: "USD", OriginalMinor: 20000, DueDate: "2026-12-01"},
		}, Receipts: []Receipt{
			{ExternalID: "p1", Currency: "CNY", AmountMinor: 4000, ReceiptDate: "2026-11-01"},
			{ExternalID: "p2", Currency: "CNY", AmountMinor: 1000, ReceiptDate: "2026-10-31"},
		},
	}
}

// TestAnalyzeFinanceFacts 验证财务指标由真实事实计算且不跨币种或业务月混加。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAnalyzeFinanceFacts(t *testing.T) {
	preview, err := Analyze(financeFixture(), time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if preview.RowCount != 4 || len(preview.Digest) != 64 {
		t.Fatalf("invalid preview: %+v", preview)
	}
	rows := preview.Snapshot.Metrics["currencies"].([]CurrencyTotals)
	if len(rows) != 2 || rows[0].Currency != "CNY" || rows[0].OriginalMinor != 10000 || rows[0].OpenMinor != 6000 || rows[0].Over90Minor != 6000 || rows[0].MonthReceiptsMinor != 4000 || rows[0].CollectionRate == nil || *rows[0].CollectionRate != 40 {
		t.Fatalf("wrong CNY totals: %+v", rows)
	}
	if rows[1].Currency != "USD" || rows[1].OpenMinor != 20000 || rows[1].Over90Minor != 0 {
		t.Fatalf("wrong USD totals: %+v", rows)
	}
	empty := financeFixture()
	empty.Receivables = []Receivable{}
	empty.Receipts = []Receipt{}
	zero, err := Analyze(empty, time.Now())
	if err != nil || !zero.Snapshot.Available || len(zero.Snapshot.Metrics["currencies"].([]CurrencyTotals)) != 0 {
		t.Fatalf("true zero lost: %+v %v", zero, err)
	}
}

// TestRejectInvalidFinanceFacts 验证版本、边界、金额、日期与重复事实在发布前被拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRejectInvalidFinanceFacts(t *testing.T) {
	cases := []func(*Document){
		func(d *Document) { d.SchemaVersion = 2 }, func(d *Document) { d.Complete = false },
		func(d *Document) { d.Receivables[0].OriginalMinor = -1 },
		func(d *Document) { d.Receivables[0].OriginalMinor = maxMinor + 1 },
		func(d *Document) { d.Receivables[0].AllocatedMinor = 10001 },
		func(d *Document) { d.Receivables[0].DueDate = "2026-02-30" },
		func(d *Document) { d.Receivables[0].Currency = "cny" },
		func(d *Document) { d.Receivables = append(d.Receivables, d.Receivables[0]) },
		func(d *Document) { d.Receipts[0].ReceiptDate = "2026-11-05" },
		func(d *Document) { d.AsOfDate = "2099-01-01" },
		func(d *Document) { d.Receipts = make([]Receipt, 1001) },
	}
	for i, mutate := range cases {
		doc := financeFixture()
		mutate(&doc)
		if _, err := Analyze(doc, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	for _, raw := range []string{`{"tenant_id":"other"}`, `{"schema_version":1} {}`, strings.Repeat(" ", MaxFileBytes+1)} {
		if _, err := DecodeDocument(strings.NewReader(raw)); err == nil {
			t.Fatal("unsafe JSON accepted")
		}
	}
}
