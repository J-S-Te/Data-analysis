package importreport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"gorm.io/gorm"
)

// auditOutboxDDL 复刻生产 bi_audit_outbox 的列（见 migrations/000016 与 000028），
// 使事务内审计可以在内存库里被断言，而不需要真实 MySQL。
// request_id/payload 必须在这里出现：真实库缺这两列时，读取侧（SELECT *）会把它们
// 静默读成零值，写入侧按列名 INSERT 则直接报 Unknown column；迁移 000028 负责补列。
const auditOutboxDDL = `CREATE TABLE bi_audit_outbox (
id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, actor_id TEXT,
action TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL,
request_id TEXT, payload TEXT,
login_ip BLOB, client_ip BLOB, ip_source TEXT NOT NULL DEFAULT 'UNKNOWN',
result TEXT NOT NULL DEFAULT 'SUCCESS', delivery_status TEXT NOT NULL DEFAULT 'pending',
attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at DATETIME NOT NULL, locked_at DATETIME,
delivered_at DATETIME, last_error TEXT, created_at DATETIME NOT NULL)`

// statusAuditDB 构造含审计 outbox 的历史投影库。
// 参数：t 为测试上下文。
// 返回值：仅含合成数据的聚合库。
func statusAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := historyDB(t)
	if err := db.Exec(auditOutboxDDL).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// statusAuditVerifier 构造带 OAuth 客户端与应用/环境绑定的测试验证器与私钥。
// 参数：t 为测试上下文。
// 返回值：验证器与测试私钥。
func statusAuditVerifier(t *testing.T) (*Verifier, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierOptions{
		Issuer: "platform", Audience: "analysis", PublicKeyPath: path, ClientID: "contract-reader",
		OAuthClientID: "machine-client", TenantID: "t", CallerEnvironment: "test",
		RequireApplicationID: true, RequireEnvironmentID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier, key
}

// statusAuditClaims 返回合法机器令牌声明。
// 参数：now 为当前 Unix 时间。
// 返回值：绑定到 machine-client/app-1/env-1 的只读用途声明。
func statusAuditClaims(now int64) map[string]any {
	return map[string]any{
		"iss": "platform", "aud": "analysis", "sub": "contract-reader",
		"oauth_client_id": "machine-client", "tenant_id": "t",
		"application_id": "app-1", "application_code": "contract_management",
		"environment_id": "env-1", "environment_code": "test",
		"token_use": "application", "scope": []string{"analysis.contract-import.report-status"},
		"iat": now - 1, "nbf": now - 1, "exp": now + 60,
	}
}

// statusAuditRows 读取审计 outbox 全部行（用于断言字段与泄漏面）。
// 参数：t 为测试上下文，db 为聚合库。
// 返回值：原始行集合。
func statusAuditRows(t *testing.T, db *gorm.DB) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := db.Table("bi_audit_outbox").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestStatusHandlerWritesMachineReadAudit 验证成功分支写一条事务内审计（BIZ-XAPI-13）：
// 记录动作、资源类型、项 ID、请求追踪号与"精确版本三元组"负载，不含令牌或查询串原文；
// 未命中的精确版本不产生审计。
func TestStatusHandlerWritesMachineReadAudit(t *testing.T) {
	db := statusAuditDB(t)
	verifier, key := statusAuditVerifier(t)
	router := gin.New()
	// 与 bootstrap 的中间件链一致：request_id 由 RequestID 中间件写入上下文。
	router.Use(func(c *gin.Context) { c.Set("request_id", "req-read-1") })
	router.GET("/status", StatusHandler(db, verifier, nil))
	token := signedToken(t, key, statusAuditClaims(time.Now().Unix()))

	// 未命中的精确版本：404 且不写审计（没有成功读取就不该有读取事件）。
	missing := httptest.NewRequest("GET", "/status?item_id=i&source_revision=1&report_release_version=1", nil)
	missing.Header.Set("Authorization", "Bearer "+token)
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing version status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}
	if rows := statusAuditRows(t, db); len(rows) != 0 {
		t.Fatalf("审计 %d 行，未命中的读取不得产生审计", len(rows))
	}

	status := aggregation.ImportReportStatus{TenantID: "t", ItemID: "i", SourceRevision: 2, ReportReleaseVersion: 3, RequiredChannels: "API_ONLY", State: "APPLIED", APIApplied: true, SourceSnapshotID: "synthetic", AggregationRunID: "import-1"}
	if err := db.Create(&status).Error; err != nil {
		t.Fatal(err)
	}
	// 请求同时带上伪造租户与额外查询参数，验证审计只取验签租户且不复制查询串。
	request := httptest.NewRequest("GET", "/status?item_id=i&source_revision=2&report_release_version=3&tenant_id=other&probe=secret", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	rows := statusAuditRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("审计 %d 行，成功读取必须恰好一条", len(rows))
	}
	row := rows[0]
	for column, want := range map[string]string{
		"tenant_id": "t", "action": "DATA_ANALYSIS:REPORT_STATUS_READ",
		"resource_type": "contract_import_item", "resource_id": "i",
		"result": "SUCCESS", "delivery_status": "pending",
		"request_id": "req-read-1",
	} {
		if got, _ := row[column].(string); got != want {
			t.Fatalf("审计列 %s=%q，期望 %q（整行 %v）", column, got, want, row)
		}
	}
	// payload 是本地证据：只允许精确版本三元组，且必须是可解析 JSON。
	payload, _ := row["payload"].(string)
	var evidence map[string]any
	if err := json.Unmarshal([]byte(payload), &evidence); err != nil {
		t.Fatalf("审计 payload 不是合法 JSON：%q（%v）", payload, err)
	}
	if evidence["source_revision"] != float64(2) || evidence["report_release_version"] != float64(3) || evidence["state"] != "APPLIED" {
		t.Fatalf("审计 payload 三元组不正确：%v", evidence)
	}
	// 泄漏面：令牌、查询串原文、伪造租户都不得进入审计行。
	dump := fmt.Sprint(row)
	for _, forbidden := range []string{token, "source_revision=2", "probe=secret", "tenant_id=other", "report_release_version=3", "Bearer"} {
		if strings.Contains(dump, forbidden) {
			t.Fatalf("审计行包含禁止内容 %q：%v", forbidden, row)
		}
	}
}

// TestStatusHandlerAuditWithoutRequestIDWritesNull 验证没有 request_id 时审计仍然写入，
// 且 request_id 落 NULL 而不是空串（迁移 000028 的可空列语义）。
func TestStatusHandlerAuditWithoutRequestIDWritesNull(t *testing.T) {
	db := statusAuditDB(t)
	verifier, key := statusAuditVerifier(t)
	router := gin.New()
	router.GET("/status", StatusHandler(db, verifier, nil))
	status := aggregation.ImportReportStatus{TenantID: "t", ItemID: "i", SourceRevision: 1, ReportReleaseVersion: 1, RequiredChannels: "API_ONLY", State: "APPLIED", APIApplied: true, SourceSnapshotID: "synthetic", AggregationRunID: "import-1"}
	if err := db.Create(&status).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/status?item_id=i&source_revision=1&report_release_version=1", nil)
	request.Header.Set("Authorization", "Bearer "+signedToken(t, key, statusAuditClaims(time.Now().Unix())))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	rows := statusAuditRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("审计 %d 行，成功读取必须恰好一条", len(rows))
	}
	if value, exists := rows[0]["request_id"]; exists && value != nil {
		t.Fatalf("没有 request_id 时必须落 NULL，实际 %#v", value)
	}
}

// TestStatusHandlerAuditFailureKeepsReadResponse 验证审计不可用时只读证明仍然返回，
// 不因审计库缺失把已获得的精确版本证明变成 503（响应不受影响）。
func TestStatusHandlerAuditFailureKeepsReadResponse(t *testing.T) {
	// 注意：这里刻意为不建 bi_audit_outbox，模拟审计库不可用。
	db := historyDB(t)
	verifier, key := statusAuditVerifier(t)
	router := gin.New()
	router.GET("/status", StatusHandler(db, verifier, nil))
	status := aggregation.ImportReportStatus{TenantID: "t", ItemID: "i", SourceRevision: 1, ReportReleaseVersion: 1, RequiredChannels: "API_ONLY", State: "APPLIED", APIApplied: true, SourceSnapshotID: "synthetic", AggregationRunID: "import-1"}
	if err := db.Create(&status).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/status?item_id=i&source_revision=1&report_release_version=1", nil)
	request.Header.Set("Authorization", "Bearer "+signedToken(t, key, statusAuditClaims(time.Now().Unix())))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"state":"APPLIED"`) {
		t.Fatalf("审计失败不得影响只读响应：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
