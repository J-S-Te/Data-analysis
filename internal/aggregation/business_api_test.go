package aggregation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

type businessAPIReader interface {
	readBusinessSnapshot(context.Context, string, MachineCredential, string) (domain.BusinessSnapshot, error)
}

// TestBusinessAPIRequiresVerifiedCompleteSnapshot 验证报告消费者只接受完整可信来源。
// 参数：t 为合成HTTP与内存令牌缓存的开发回归上下文。
// 返回值：无；缺实现、跨租户、畸形数值、未来水位或错误响应被接受时失败。
func TestBusinessAPIRequiresVerifiedCompleteSnapshot(t *testing.T) {
	stamp := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	valid := fmt.Sprintf(`{"available":true,"tenant_id":"t","snapshot_at":%q,`+
		`"metrics":{"active_reports":0,"void_reports":0,"status_counts":{},`+
		`"cycle_p50_days":null,"cycle_samples":0,"reviewed_reports":0},`+
		`"missing_metrics":["first_pass_rate","open_complaints"],"basis":"source ledger"}`, stamp)
	cases := []struct {
		name, body string
		status     int
		allowed    bool
	}{
		{"complete-zero", valid, 200, true},
		{"other-tenant", strings.Replace(valid, `"tenant_id":"t"`, `"tenant_id":"other"`, 1), 200, false},
		{"missing-count", strings.Replace(valid, `"active_reports":0,`, "", 1), 200, false},
		{"negative-count", strings.Replace(valid, `"active_reports":0`, `"active_reports":-1`, 1), 200, false},
		{"inconsistent-count", strings.Replace(valid, `"status_counts":{}`, `"status_counts":{"ISSUED":1}`, 1), 200, false},
		{"future-watermark", strings.Replace(valid, stamp, time.Now().Add(time.Hour).Format(time.RFC3339Nano), 1), 200, false},
		{"trailing-json", valid + ` {}`, 200, false},
		{"dependency-failed", `{"secret":"must-not-be-in-errors"}`, 503, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer qa-token" {
					t.Error("未使用独立机器身份")
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			r := NewAPISyncRunner(nil, APISyncOptions{TenantID: "t"})
			r.tokens["report"] = cachedMachineToken{value: "qa-token", expiresAt: time.Now().Add(time.Hour)}
			reader, ok := any(r).(businessAPIReader)
			if !ok {
				t.Fatal("缺少报告/财务机器API消费者")
			}
			value, err := reader.readBusinessSnapshot(context.Background(), server.URL,
				MachineCredential{ClientID: "report", Scope: "dashboard.project.read"}, "report")
			if (err == nil) != tc.allowed || tc.allowed && !value.Available {
				t.Fatalf("allowed=%t available=%t err=%v", tc.allowed, value.Available, err)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-be-in-errors") {
				t.Fatal("上游响应泄漏到错误信息")
			}
		})
	}
}
