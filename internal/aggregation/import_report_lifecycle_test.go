package aggregation

import (
	"context"
	"testing"
)

// readImportReportRun 读取当前租户的发布栅栏行，供生命周期断言使用。
// 参数：t 为测试上下文，r 为隔离聚合库执行器。
// 返回值：最新的运行行；读取失败直接终止测试。
func readImportReportRun(t *testing.T, r *Runner) ImportReportRun {
	t.Helper()
	var run ImportReportRun
	if err := r.aggDB.Where("tenant_id=?", r.tenantID).Take(&run).Error; err != nil {
		t.Fatalf("读取发布栅栏失败: %v", err)
	}
	return run
}

// countImportRows 统计当前租户的历史投影与逐项凭据行数。
// 参数：t 为测试上下文，r 为隔离聚合库执行器，table 为表名。
// 返回值：行数；查询失败直接终止测试。
func countImportRows(t *testing.T, r *Runner, table string) int64 {
	t.Helper()
	var count int64
	if err := r.aggDB.Table(table).Where("tenant_id=?", r.tenantID).Count(&count).Error; err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return count
}

// TestImportReportFailureKeepsSuccessEvidence 验证失败收尾不覆盖已验证的发布成功证据。
// 参数：t 为测试上下文。
// 返回值：无；失败只把本轮 RUNNING 收敛为 FAILED，成功水位/已发布快照原样保留。
func TestImportReportFailureKeepsSuccessEvidence(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	facts := map[string]ImportReportItem{"i2": s.Items[1]}
	successGen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, successGen, s, facts, ""); err != nil {
		t.Fatal(err)
	}
	success := readImportReportRun(t, r)
	if success.LastStatus != importRunSuccess || success.LastSuccessAt == nil || success.PublishedSnapshotID != s.SourceSnapshotID || success.PublishedRunID == "" {
		t.Fatalf("完全应用未推进成功证据 %+v", success)
	}

	// 新一轮在网络读取阶段失败：只允许收敛本轮，不得回退成功水位和已发布快照。
	failedGen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.failImportReport(ctx, failedGen); err != nil {
		t.Fatal(err)
	}
	failed := readImportReportRun(t, r)
	if failed.LastStatus != importRunFailed {
		t.Fatalf("失败轮次状态 %q", failed.LastStatus)
	}
	if failed.LastSuccessAt == nil || !failed.LastSuccessAt.Equal(*success.LastSuccessAt) ||
		failed.PublishedSnapshotID != success.PublishedSnapshotID || failed.PublishedRunID != success.PublishedRunID {
		t.Fatalf("失败收尾覆盖成功证据 before=%+v after=%+v", success, failed)
	}
	if rows := countImportRows(t, r, "contract_import_history"); rows != 2 {
		t.Fatalf("失败轮次改写了历史投影 %d", rows)
	}

	// 已成功代次的迟到失败收尾也不得把 SUCCESS 改成 FAILED。
	if err = r.failImportReport(ctx, successGen); err != nil {
		t.Fatal(err)
	}
	if again := readImportReportRun(t, r); again.LastStatus != importRunFailed || !again.LastSuccessAt.Equal(*success.LastSuccessAt) {
		t.Fatalf("迟到失败收尾改动成功证据 %+v", again)
	}
}

// TestImportReportSourceOutageKeepsVerifiedItem 验证源读取失败不撤销已验证的逐项应用证据，
// 也不推进成功水位。
// 参数：t 为测试上下文。
// 返回值：无；同内容项保持 APPLIED，本轮状态为 BLOCKED。
func TestImportReportSourceOutageKeepsVerifiedItem(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	facts := map[string]ImportReportItem{"i2": s.Items[1]}
	gen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, facts, ""); err != nil {
		t.Fatal(err)
	}
	applied, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil || applied.State != importItemApplied {
		t.Fatalf("首轮应用 %+v %v", applied, err)
	}
	success := readImportReportRun(t, r)

	gen, err = r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, nil, "SOURCE_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}
	blocked := readImportReportRun(t, r)
	if blocked.LastStatus != importRunBlocked {
		t.Fatalf("源故障轮次状态 %q", blocked.LastStatus)
	}
	if blocked.LastSuccessAt == nil || !blocked.LastSuccessAt.Equal(*success.LastSuccessAt) ||
		blocked.PublishedSnapshotID != success.PublishedSnapshotID {
		t.Fatalf("源故障推进了成功水位 before=%+v after=%+v", success, blocked)
	}
	status, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != importItemApplied || !status.SourceFactsApplied || status.AggregationRunID != applied.AggregationRunID ||
		status.AppliedAt == nil || !status.AppliedAt.Equal(*applied.AppliedAt) {
		t.Fatalf("源故障撤销已验证证据 before=%+v after=%+v", applied, status)
	}

	// 源可读但事实与本轮摘要矛盾属于真实不一致：必须如实降级，不得沿用旧证据冒充应用。
	contradict := reportFixture()
	contradict.Items[1].AmountMinor++
	gen, err = r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, map[string]ImportReportItem{"i2": contradict.Items[1]}, ""); err != nil {
		t.Fatal(err)
	}
	downgraded, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if downgraded.State != importItemWaiting || downgraded.SourceFactsApplied {
		t.Fatalf("源事实矛盾未降级 %+v", downgraded)
	}
	if after := readImportReportRun(t, r); after.LastStatus != importRunWaiting ||
		after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*success.LastSuccessAt) {
		t.Fatalf("降级轮次推进了成功水位 %+v", after)
	}
}

// TestImportReportSameVersionContentChangeIsAuditedRepublish 验证同版本异内容可重发布且幂等。
// 参数：t 为测试上下文。
// 返回值：无；内容变化产生重发布审计，重复相同内容不再累加。
func TestImportReportSameVersionContentChangeIsAuditedRepublish(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	first, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, first, s, map[string]ImportReportItem{"i2": s.Items[1]}, ""); err != nil {
		t.Fatal(err)
	}
	original, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}

	s.Items[1].AmountMinor++
	second, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, second, s, nil, ""); err != nil {
		t.Fatalf("同版本异内容使整轮永久失败: %v", err)
	}
	superseded, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.SupersedeCount != 1 || superseded.PriorPayloadDigest != original.PayloadDigest {
		t.Fatalf("同版本异内容缺少审计 %+v", superseded)
	}
	if run := readImportReportRun(t, r); run.LastStatus != importRunWaiting {
		t.Fatalf("内容变化轮次状态 %q", run.LastStatus)
	}

	// 幂等：同样的内容再次发布不累加重发布次数，也不产生重复行。
	third, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, third, s, nil, ""); err != nil {
		t.Fatal(err)
	}
	repeated, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.SupersedeCount != 1 || repeated.PriorPayloadDigest != superseded.PriorPayloadDigest {
		t.Fatalf("幂等重放累加审计 %+v", repeated)
	}
	if rows := countImportRows(t, r, "contract_import_report_status"); rows != 2 {
		t.Fatalf("幂等重放产生重复凭据 %d", rows)
	}
	if rows := countImportRows(t, r, "contract_import_history"); rows != 2 {
		t.Fatalf("幂等重放产生重复历史 %d", rows)
	}
}

// TestImportReportRevisionRollbackRepublishes 验证上游修订回退不再让整轮永久失败。
// 参数：t 为测试上下文。
// 返回值：无；回退版本被应用为当前内容，较高旧水位标记 SUPERSEDED_BY_REPUBLISH。
func TestImportReportRevisionRollbackRepublishes(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	higher, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, higher, s, map[string]ImportReportItem{"i2": s.Items[1]}, ""); err != nil {
		t.Fatal(err)
	}

	rollback := reportFixture()
	rollback.SourceSnapshotID = "b" + rollback.SourceSnapshotID[1:]
	rollback.Items[1].SourceRevision = 1
	rollback.Items[1].ReportReleaseVersion = 1
	rollback.Items[1].AmountMinor = 150
	gen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, rollback, map[string]ImportReportItem{"i2": rollback.Items[1]}, ""); err != nil {
		t.Fatalf("修订回退使整轮永久失败: %v", err)
	}
	run := readImportReportRun(t, r)
	if run.LastStatus != importRunSuccess || run.PublishedSnapshotID != rollback.SourceSnapshotID {
		t.Fatalf("回退版本未被发布 %+v", run)
	}
	current, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 1, 1)
	if err != nil || current.State != importItemApplied || len(current.AppliedChannels) != 2 {
		t.Fatalf("回退版本应用 %+v %v", current, err)
	}
	retired, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i2", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != importItemWithdrawn || retired.ErrorCode != "SUPERSEDED_BY_REPUBLISH" || len(retired.AppliedChannels) != 0 {
		t.Fatalf("较高旧水位未标记被替代 %+v", retired)
	}
}

// TestImportReportWithdrawnItemsCanBeReplayed 验证撤回项退出水位并可被重放纠正。
// 参数：t 为测试上下文。
// 返回值：无；快照中消失的项变为 WITHDRAWN 且不再冒充应用，重新出现时恢复 APPLIED。
func TestImportReportWithdrawnItemsCanBeReplayed(t *testing.T) {
	ctx := context.Background()
	r := reportDB(t)
	s := reportFixture()
	facts := map[string]ImportReportItem{"i2": s.Items[1]}
	gen, err := r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, facts, ""); err != nil {
		t.Fatal(err)
	}

	onlySecond := ImportReportSnapshot{SchemaVersion: 1, TenantID: "t", SourceSnapshotID: "c" + s.SourceSnapshotID[1:], Complete: true, Items: []ImportReportItem{s.Items[1]}}
	gen, err = r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, onlySecond, facts, ""); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.State != importItemWithdrawn || withdrawn.ErrorCode != "ITEM_WITHDRAWN" || withdrawn.WithdrawnAt == nil || len(withdrawn.AppliedChannels) != 0 {
		t.Fatalf("撤回项未退出水位 %+v", withdrawn)
	}
	if rows := countImportRows(t, r, "contract_import_history"); rows != 1 {
		t.Fatalf("撤回项仍出现在历史投影 %d", rows)
	}

	// 重放：同一项重新出现时必须能纠正回 APPLIED，而不是永久保留撤回或产生重复行。
	gen, err = r.beginImportReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.publishImportReport(ctx, gen, s, facts, ""); err != nil {
		t.Fatal(err)
	}
	replayed, err := ReadImportReportStatus(ctx, r.aggDB, "t", "i1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != importItemApplied || replayed.WithdrawnAt != nil || len(replayed.AppliedChannels) != 1 {
		t.Fatalf("撤回项重放未纠正 %+v", replayed)
	}
	if rows := countImportRows(t, r, "contract_import_report_status"); rows != 2 {
		t.Fatalf("重放产生重复凭据 %d", rows)
	}
	if rows := countImportRows(t, r, "contract_import_history"); rows != 2 {
		t.Fatalf("重放未恢复历史投影 %d", rows)
	}
}

// TestImportReportStatusReadRequiresExactVersion 验证撤回不影响精确版本读取边界。
// 参数：t 为测试上下文。
// 返回值：无；非本次发布的项和版本仍返回记录不存在，不推断最大水位。
func TestImportReportStatusReadRequiresExactVersion(t *testing.T) {
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
	if _, err = ReadImportReportStatus(ctx, r.aggDB, "t", "i1", 2, 1); err == nil {
		t.Fatal("错误修订得到成功")
	}
	if _, err = ReadImportReportStatus(ctx, r.aggDB, "t", "i1", 1, 2); err == nil {
		t.Fatal("错误发布版本得到成功")
	}
}
