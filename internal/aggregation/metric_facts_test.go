package aggregation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMetricFactsSourceRegistrationOptIn 验证新增指标来源显式启用且各自登记。
// 参数：t为测试上下文。
// 返回值：无；旧配置意外启用或缺少独立来源时失败。
func TestMetricFactsSourceRegistrationOptIn(t *testing.T) {
	opts := APISyncOptions{ContractInternalURL: "http://contract/internal/v1/dashboard",
		ProjectInternalURL:   "http://project/internal/v1/dashboard",
		SettlementFinanceURL: "http://finance/internal/v1/dashboard/finance"}
	r := &Runner{ProjectAPI: NewAPISyncRunner(nil, opts)}
	if _, ok := r.configuredSources()["contract_metrics"]; ok {
		t.Fatal("old configuration silently enabled")
	}
	opts.MetricFactsEnabled = true
	r.ProjectAPI = NewAPISyncRunner(nil, opts)
	for _, code := range []string{"contract_metrics", "project_metrics", "finance_metrics"} {
		if _, ok := r.configuredSources()[code]; !ok {
			t.Fatalf("missing %s", code)
		}
	}
}

// TestMetricFactsHTTPVerifiedAndNoRedirect 验证完整来源、独立凭据和拒绝重定向。
// 参数：t为测试上下文，仅启动合成回环HTTP来源。
// 返回值：无；错误租户、尾随数据、错误来源或重定向被发布时失败。
func TestMetricFactsHTTPVerifiedAndNoRedirect(t *testing.T) {
	stamp := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"code":"OK","data":{"schema_version":1,"tenant_id":"t","source_code":"project_management","snapshot_at":%q,"complete":true,"basis":"actual confirmed facts","rows":[]}}`, stamp)
	for _, test := range []struct {
		name, payload string
		status        int
		allowed       bool
	}{
		{"complete-empty", body, 200, true},
		{"wrong-tenant", strings.Replace(body, `"tenant_id":"t"`, `"tenant_id":"other"`, 1), 200, false},
		{"trailing", body + " {}", 200, false},
		{"dependency-error", `{"private":"never-in-error"}`, 503, false},
		{"redirect", body, 302, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			followed := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { followed = true }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/internal/v1/dashboard/metrics" || req.Header.Get("Authorization") != "Bearer qa-metrics" {
					t.Error("wrong endpoint or credential")
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.payload)
			}))
			defer server.Close()
			r := NewAPISyncRunner(nil, APISyncOptions{TenantID: "t"})
			r.tokens["metrics"] = cachedMachineToken{value: "qa-metrics", expiresAt: time.Now().Add(time.Hour)}
			_, err := r.readMetricSnapshot(context.Background(), server.URL+"/internal/v1/dashboard",
				MachineCredential{ClientID: "metrics", Scope: "dashboard.project.read"}, "project_management")
			if (err == nil) != test.allowed || followed {
				t.Fatalf("allowed=%t followed=%t err=%v", test.allowed, followed, err)
			}
			if err != nil && strings.Contains(err.Error(), "never-in-error") {
				t.Fatal("source error leaked")
			}
		})
	}
}
