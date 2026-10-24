package platformaudit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AUD-IP-2026：审计上报必须区分登录地址、操作地址与来源标记，并兼容旧 user_login_ip。
func TestReportEmitsLoginClientAndSourceIP(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/token"):
			_, _ = io.WriteString(w, `{"access_token":"token","token_type":"Bearer","scope":"audit.ingest","expires_in":300}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/audit/events"):
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode: %v", err)
			}
			captured = payload
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"code":"OK","data":{"event_id":"e","status":"ACCEPTED"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()

	reporter := NewReporter(server.URL, "c", "s", "data_analysis", "dev")
	if err := reporter.Report(context.Background(), Event{
		ActorID: "user-1", Action: "DATA_ANALYSIS:POST:rules", ResourceType: "rule", RequestID: "r-1", CorrelationID: "r-1",
		Result: "SUCCESS", LoginIP: "203.0.113.10", ClientIP: "10.8.0.5", IPSource: "TRUSTED_PROXY",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	for key, want := range map[string]string{
		"login_ip": "203.0.113.10", "user_login_ip": "203.0.113.10",
		"client_ip": "10.8.0.5", "ip_source": "TRUSTED_PROXY",
	} {
		if got, _ := captured[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
