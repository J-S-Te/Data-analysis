package aggregation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// emptyMetricStream 独立构造已提交空来源的规范流，用于 HTTP 消费边界测试。
// 参数：tenant/basis/stamp 为合成来源元数据。
// 返回值：带正确空摘要和结尾 LF 的完整流。
func emptyMetricStream(tenant, basis, stamp string) string {
	metadata := metricfacts.Bundle{SchemaVersion: 1, TenantID: tenant, SourceCode: "contract_management", SnapshotAt: stamp, Complete: true, Basis: basis, Rows: []metricfacts.Row{}}
	begin, _ := json.Marshal(map[string]any{"type": "begin", "metadata": metadata})
	digest := sha256.Sum256(nil)
	end, _ := json.Marshal(map[string]any{"type": "end", "row_count": 0, "sha256": hex.EncodeToString(digest[:])})
	return string(begin) + "\n" + string(end) + "\n"
}

// TestMetricStreamFallbackOnlyAbsent 验证流错误不能通过回退旧接口掩盖。
// 参数：t 为测试上下文；仅使用合成回环 HTTP 与内存 SQL。
// 返回值：无；错误回退、重定向、凭据/租户遗漏或损坏被接受时失败。
func TestMetricStreamFallbackOnlyAbsent(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	good := emptyMetricStream("stream-http", "complete", now)
	tests := []struct {
		name              string
		status            int
		body, media       string
		fallback, success bool
	}{
		{"stream", 200, good, "application/x-ndjson", false, true},
		{"not-found", 404, "", "", true, true}, {"method", 405, "", "", true, true},
		{"unavailable", 503, "", "", false, false}, {"unauthorized", 401, "", "", false, false},
		{"forbidden", 403, "", "", false, false}, {"redirect", 302, "", "", false, false},
		{"invalid-body", 200, "{}\n", "application/x-ndjson", false, false},
		{"invalid-media", 200, good, "application/json", false, false},
		{"trailing", 200, good + "{}\n", "application/x-ndjson", false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&metricfacts.SnapshotRow{}, &metricfacts.GenerationRow{}, &metricfacts.FactRow{}); err != nil {
				t.Fatal(err)
			}
			legacyCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer synthetic-stream-token" || req.Header.Get("X-DA-Tenant-ID") != "stream-http" {
					t.Error("machine identity lost")
				}
				if strings.HasSuffix(req.URL.Path, "/stream") {
					w.Header().Set("Content-Type", test.media)
					w.WriteHeader(test.status)
					_, _ = fmt.Fprint(w, test.body)
					return
				}
				legacyCalls++
				_, _ = fmt.Fprintf(w, `{"code":"OK","data":{"schema_version":1,"tenant_id":"stream-http","source_code":"contract_management","snapshot_at":%q,"complete":true,"basis":"legacy","rows":[]}}`, now)
			}))
			defer server.Close()
			api := NewAPISyncRunner(db, APISyncOptions{TenantID: "stream-http"})
			api.tokens["stream-test"] = cachedMachineToken{value: "synthetic-stream-token", expiresAt: time.Now().Add(time.Hour)}
			staged, legacy, err := api.stageMetricSnapshot(context.Background(), server.URL+"/internal/v1/dashboard", MachineCredential{ClientID: "stream-test", Scope: "dashboard.contract.read"}, "contract_management", func(action func(*gorm.DB) error) error { return db.Transaction(action) })
			if (err == nil) != test.success || (legacyCalls == 1) != test.fallback {
				t.Fatalf("success=%v fallback calls=%d err=%v", err == nil, legacyCalls, err)
			}
			if err == nil && (staged == nil) == (legacy == nil) {
				t.Fatal("protocol result is ambiguous")
			}
		})
	}
}
