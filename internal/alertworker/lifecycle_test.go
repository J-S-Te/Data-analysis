package alertworker

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

// lifecycleDB 建立只含告警规则、告警项的最小隔离库（列与迁移 000026+000027 对齐）。
// 参数：t 为测试上下文。
// 返回值：可读写 alert_rule/alert_item 的 sqlite 内存库；不连接任何真实数据库。
func lifecycleDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := ruleTestDB(t)
	if err := db.Exec(`CREATE TABLE alert_item (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL,
 alert_type TEXT NOT NULL,
 rule_code TEXT NOT NULL,
 severity TEXT NOT NULL DEFAULT 'MEDIUM',
 owner_org_id TEXT NOT NULL DEFAULT '',
 target_ref TEXT NOT NULL,
 title TEXT NOT NULL,
 due_date DATETIME NULL,
 status TEXT NOT NULL DEFAULT 'OPEN',
 closed_at DATETIME NULL,
 closed_reason TEXT NOT NULL DEFAULT '',
 miss_streak INTEGER NOT NULL DEFAULT 0,
 hit_streak INTEGER NOT NULL DEFAULT 0,
 last_hit_at DATETIME NULL,
 reopen_count INTEGER NOT NULL DEFAULT 0,
 acknowledged_at DATETIME NULL,
 acknowledged_by TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

type lifecycleRow struct {
	ID             string  `gorm:"column:id;primaryKey"`
	TenantID       string  `gorm:"column:tenant_id"`
	RuleCode       string  `gorm:"column:rule_code"`
	TargetRef      string  `gorm:"column:target_ref"`
	Status         string  `gorm:"column:status"`
	ClosedAt       *string `gorm:"column:closed_at"`
	ClosedReason   string  `gorm:"column:closed_reason"`
	MissStreak     int     `gorm:"column:miss_streak"`
	HitStreak      int     `gorm:"column:hit_streak"`
	LastHitAt      *string `gorm:"column:last_hit_at"`
	ReopenCount    int     `gorm:"column:reopen_count"`
	AcknowledgedAt *string `gorm:"column:acknowledged_at"`
	AcknowledgedBy string  `gorm:"column:acknowledged_by"`
}

func lifecycleAlertsTable(db *gorm.DB) *gorm.DB { return db.Table("alert_item") }

// seedLifecycleAlert 插入一条指定状态的告警项。
// 参数：t 为测试上下文，db 为隔离库，其余为告警身份与初始状态。
// 返回值：无；插入失败直接终止测试。
func seedLifecycleAlert(t *testing.T, db *gorm.DB, tenant, rule, target, status, closedReason string) {
	t.Helper()
	now := time.Date(2026, time.October, 8, 0, 0, 0, 0, time.UTC)
	var closedAt any
	if status == "CLOSED" {
		closedAt = now
	}
	if err := db.Exec(`INSERT INTO alert_item
(id, tenant_id, alert_type, rule_code, severity, owner_org_id, target_ref, title, due_date, status, closed_at, closed_reason, created_at, updated_at)
VALUES (?, ?, ?, ?, 'HIGH', 'org-a', ?, ?, NULL, ?, ?, ?, ?, ?)`,
		tenant+"-"+rule+"-"+target, tenant, rule, rule, target, target, status, closedAt, closedReason, now, now).Error; err != nil {
		t.Fatalf("seed alert %s/%s: %v", tenant, target, err)
	}
}

// markLifecycleAcknowledged 模拟人工确认证据：ACK 时写入 acknowledged_at/by。
// 参数：t 为测试上下文，db 为隔离库，tenant/rule/target 为告警身份。
// 返回值：无。
func markLifecycleAcknowledged(t *testing.T, db *gorm.DB, tenant, rule, target, actor string) {
	t.Helper()
	at := time.Date(2026, time.October, 8, 6, 0, 0, 0, time.UTC)
	if err := db.Exec(`UPDATE alert_item SET acknowledged_at = ?, acknowledged_by = ? WHERE tenant_id = ? AND rule_code = ? AND target_ref = ?`,
		at, actor, tenant, rule, target).Error; err != nil {
		t.Fatalf("标记确认证据 %s/%s: %v", tenant, target, err)
	}
}

// readLifecycleAlert 读取告警项当前状态。
// 参数：t 为测试上下文，db 为隔离库，tenant/rule/target 为告警身份。
// 返回值：状态行；读取失败直接终止测试。
func readLifecycleAlert(t *testing.T, db *gorm.DB, tenant, rule, target string) lifecycleRow {
	t.Helper()
	var row lifecycleRow
	if err := lifecycleAlertsTable(db).Where("tenant_id = ? AND rule_code = ? AND target_ref = ?", tenant, rule, target).Take(&row).Error; err != nil {
		t.Fatalf("读取告警 %s/%s/%s: %v", tenant, rule, target, err)
	}
	return row
}

// activeAlert 构造一条本轮命中的告警快照。
// 参数：target 为告警目标。
// 返回值：命中快照。
func activeAlert(target string) Alert {
	return Alert{ID: "x-" + target, TenantID: "tenant-a", AlertType: RuleContractExpiry, RuleCode: RuleContractExpiry, TargetRef: target}
}

// TestReconcileAlertsKeepsExpiredContractOpen 验证条件仍成立（含刚到期未续签）的告警
// 保持 OPEN，并刷新命中计数；不会再因为"离开提醒窗口"被关闭。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsKeepsExpiredContractOpen(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	evaluatedAt := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-expired", "OPEN", "")

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{activeAlert("contract-expired")}, evaluatedAt,
		ReconcileOptions{ConfirmRounds: 2, ReopenPolicy: ReopenPolicyAuto}); err != nil {
		t.Fatal(err)
	}
	row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-expired")
	if row.Status != "OPEN" || row.ClosedAt != nil || row.ClosedReason != "" {
		t.Fatalf("条件成立告警被关闭 %+v", row)
	}
	if row.HitStreak != 1 || row.MissStreak != 0 || row.LastHitAt == nil {
		t.Fatalf("命中计数未刷新 %+v", row)
	}
}

// TestReconcileAlertsRequiresConfirmRoundsBeforeAutoConditionCleared 验证续签/条件消失后
// 必须连续两轮未命中才关闭为 AUTO_CONDITION_CLEARED；单轮抖动只累加 miss_streak。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsRequiresConfirmRoundsBeforeAutoConditionCleared(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	first := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-renewed", "OPEN", "")
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-still-due", "OPEN", "")
	// 同一规则仍有一条命中，安全阀不会介入，收敛正常进行。
	hits := []Alert{activeAlert("contract-still-due")}
	options := ReconcileOptions{ConfirmRounds: 2, ReopenPolicy: ReopenPolicyAuto}

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, hits, first, options); err != nil {
		t.Fatal(err)
	}
	row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-renewed")
	if row.Status != "OPEN" || row.MissStreak != 1 {
		t.Fatalf("第一轮未命中就关闭了告警 %+v", row)
	}

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, hits, first.Add(time.Hour), options); err != nil {
		t.Fatal(err)
	}
	row = readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-renewed")
	if row.Status != "CLOSED" || row.ClosedReason != closedReasonAutoConditionCleared || row.ClosedAt == nil {
		t.Fatalf("第二轮未命中未按条件消失关闭 %+v", row)
	}

	// 第三轮继续未命中：已关闭记录不被重复改写，关闭原因保持稳定。
	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, hits, first.Add(2*time.Hour), options); err != nil {
		t.Fatal(err)
	}
	if row = readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-renewed"); row.ClosedReason != closedReasonAutoConditionCleared {
		t.Fatalf("已关闭告警被重复改写 %+v", row)
	}

	// 条件重新命中：按策略复开并累加复开次数，人工确认证据保留。
	markLifecycleAcknowledged(t, db, "tenant-a", RuleContractExpiry, "contract-renewed", "user-7")
	back := []Alert{activeAlert("contract-still-due"), activeAlert("contract-renewed")}
	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, back, first.Add(3*time.Hour), options); err != nil {
		t.Fatal(err)
	}
	row = readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-renewed")
	if row.Status != "OPEN" || row.ClosedAt != nil || row.ClosedReason != "" {
		t.Fatalf("条件消失关闭的告警未复开 %+v", row)
	}
	if row.ReopenCount != 1 || row.MissStreak != 0 || row.HitStreak < 1 || row.LastHitAt == nil {
		t.Fatalf("复开计数不正确 %+v", row)
	}
	if row.AcknowledgedAt == nil || row.AcknowledgedBy != "user-7" {
		t.Fatalf("复开清空了人工确认证据 %+v", row)
	}
}

// TestReconcileAlertsReopensLegacyAutoWindowExit 验证历史 AUTO_WINDOW_EXIT 关闭的告警
// 仍可被条件重新命中复开，复开次数累加。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsReopensLegacyAutoWindowExit(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	now := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-legacy", "CLOSED", closedReasonAutoWindowExit)

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{activeAlert("contract-legacy")}, now,
		ReconcileOptions{ConfirmRounds: 2}); err != nil {
		t.Fatal(err)
	}
	row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-legacy")
	if row.Status != "OPEN" || row.ClosedReason != "" || row.ClosedAt != nil || row.ReopenCount != 1 {
		t.Fatalf("历史自动关闭未复开 %+v", row)
	}
}

// TestReconcileAlertsNeverReopensManualClose 验证人工关闭永不被规则评估复开，且不写入
// 命中计数之外的任何状态。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsNeverReopensManualClose(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	now := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-manual", "CLOSED", "MANUAL")

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{activeAlert("contract-manual")}, now,
		ReconcileOptions{ConfirmRounds: 2, ReopenPolicy: ReopenPolicyAuto}); err != nil {
		t.Fatal(err)
	}
	row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-manual")
	if row.Status != "CLOSED" || row.ClosedReason != "MANUAL" || row.ClosedAt == nil {
		t.Fatalf("人工关闭被规则复开 %+v", row)
	}
	if row.ReopenCount != 0 {
		t.Fatalf("人工关闭被计入复开次数 %+v", row)
	}
}

// TestReconcileAlertsManualReopenPolicyKeepsClosed 验证 ALERT_REOPEN_POLICY=manual 时
// 即使条件重新命中也不自动复开。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsManualReopenPolicyKeepsClosed(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	now := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-auto", "CLOSED", closedReasonAutoWindowExit)

	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{activeAlert("contract-auto")}, now,
		ReconcileOptions{ConfirmRounds: 2, ReopenPolicy: ReopenPolicyManual}); err != nil {
		t.Fatal(err)
	}
	if row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-auto"); row.Status != "CLOSED" || row.ReopenCount != 0 {
		t.Fatalf("manual 复开策略下仍自动复开 %+v", row)
	}
}

// TestReconcileAlertsIgnoresEmptyActiveSet 验证空命中集合不做任何收敛（含不累加
// miss_streak），避免把"本轮没算出来"当成"条件全部消失"。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsIgnoresEmptyActiveSet(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	now := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-a", "OPEN", "")
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-b", "ACK", "")

	for round := 0; round < 3; round++ {
		if err := store.ReconcileAlerts(ctx, RuleContractExpiry, nil, now.Add(time.Duration(round)*time.Hour),
			ReconcileOptions{ConfirmRounds: 2}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"contract-a", "contract-b"} {
		row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, target)
		if row.Status != "OPEN" && row.Status != "ACK" {
			t.Fatalf("空命中集合关闭了告警 %s %+v", target, row)
		}
		if row.MissStreak != 0 {
			t.Fatalf("空命中集合累加了未命中计数 %s %+v", target, row)
		}
	}
}

// TestReconcileAlertsKeepsTenantAndRuleIsolation 验证收敛只影响同租户同规则，
// 其它规则/租户的告警不被改写。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsKeepsTenantAndRuleIsolation(t *testing.T) {
	ctx := context.Background()
	db := lifecycleDB(t)
	store := NewGormStore(db, "tenant-a")
	now := time.Date(2026, time.October, 9, 3, 0, 0, 0, time.UTC)
	seedLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-in", "OPEN", "")
	seedLifecycleAlert(t, db, "tenant-b", RuleContractExpiry, "contract-in", "OPEN", "")
	seedLifecycleAlert(t, db, "tenant-a", "OTHER_RULE", "contract-in", "OPEN", "")

	options := ReconcileOptions{ConfirmRounds: 1, ReopenPolicy: ReopenPolicyAuto}
	for round := 0; round < 2; round++ {
		if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{activeAlert("contract-other")}, now.Add(time.Duration(round)*time.Hour), options); err != nil {
			t.Fatal(err)
		}
	}
	if row := readLifecycleAlert(t, db, "tenant-a", RuleContractExpiry, "contract-in"); row.Status != "CLOSED" {
		t.Fatalf("本租户本规则未收敛 %+v", row)
	}
	if row := readLifecycleAlert(t, db, "tenant-b", RuleContractExpiry, "contract-in"); row.Status != "OPEN" || row.MissStreak != 0 {
		t.Fatalf("跨租户告警被改动 %+v", row)
	}
	var otherRule lifecycleRow
	if err := lifecycleAlertsTable(db).Where("tenant_id = ? AND rule_code = ?", "tenant-a", "OTHER_RULE").Take(&otherRule).Error; err != nil {
		t.Fatal(err)
	}
	if otherRule.Status != "OPEN" || otherRule.MissStreak != 0 {
		t.Fatalf("其它规则告警被收敛 %+v", otherRule)
	}
}

// TestReconcileAlertsRejectsCrossTenantActive 验证收敛不接受跨租户或跨规则命中集合。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconcileAlertsRejectsCrossTenantActive(t *testing.T) {
	ctx := context.Background()
	store := NewGormStore(nil, "tenant-a")
	options := ReconcileOptions{ConfirmRounds: 2}
	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{{TenantID: "tenant-b", RuleCode: RuleContractExpiry}}, time.Now(), options); err == nil {
		t.Fatal("跨租户收敛被接受")
	}
	if err := store.ReconcileAlerts(ctx, RuleContractExpiry, []Alert{{TenantID: "tenant-a", RuleCode: "OTHER"}}, time.Now(), options); err == nil {
		t.Fatal("跨规则收敛被接受")
	}
	if err := store.ReconcileAlerts(ctx, "", nil, time.Now(), options); err == nil {
		t.Fatal("空规则收敛被接受")
	}
	if err := NewGormStore(nil, "  ").ReconcileAlerts(ctx, RuleContractExpiry, nil, time.Now(), options); err == nil {
		t.Fatal("空租户收敛被接受")
	}
}
