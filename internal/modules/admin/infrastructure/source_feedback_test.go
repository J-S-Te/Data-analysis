package infrastructure

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSourceFeedbackPublishesSafeFailureClass 用途：验证数据源公开回执分类故障且不回显内部诊断。
// 参数：t 为不连接数据库的真实仓储映射测试上下文。
// 返回值：无；分类丢失或地址、凭据、底层异常进入回执时失败。
func TestSourceFeedbackPublishesSafeFailureClass(t *testing.T) {
	for _, tc := range []struct{ raw, code, want string }{
		{"platform machine credential is incomplete", "SOURCE_CREDENTIAL_INCOMPLETE", "接入凭据"},
		{"project internal URL is not configured", "SOURCE_NOT_CONFIGURED", "接入配置"},
		{"Get http://private.internal:8080: context deadline exceeded", "SOURCE_TIMEOUT", "超时"},
		{"machine token returned HTTP 403", "SOURCE_AUTHORIZATION_FAILED", "授权"},
		{"unexpected upstream detail password=private-value", "SOURCE_UNAVAILABLE", "暂时不可用"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			value := (syncSourceRecord{ID: "source", LastStatus: "FAILED", LastError: &tc.raw}).toDomain()
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["last_error_code"] != tc.code || value.LastError == nil || !strings.Contains(*value.LastError, tc.want) {
				t.Fatalf("公开分类缺失: %s", raw)
			}
			if strings.Contains(string(raw), tc.raw) || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "password") {
				t.Fatalf("公开回执包含内部诊断: %s", raw)
			}
		})
	}
}
