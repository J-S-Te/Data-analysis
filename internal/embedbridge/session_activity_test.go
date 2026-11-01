package embedbridge

import (
	"strings"
	"testing"
)

// TestInjectSessionActivityUsesExternalScript 验证看板使用同源外链脚本且不放宽CSP。
// 参数：t为Go测试上下文。
// 返回值：无；缺少来源、脚本或出现未转义属性则失败。
func TestInjectSessionActivityUsesExternalScript(t *testing.T) {
	body := []byte("<!doctype html><html><head></head><body>dashboard</body></html>")
	got := string(injectSessionActivity(body, "/data_analysis/api/v1/embed-proxy/capability/", "https://platform.example"))
	if !strings.Contains(got, `src="/data_analysis/api/v1/embed-proxy/capability/__platform_session_activity.js"`) ||
		!strings.Contains(got, `data-platform-origin="https://platform.example"`) ||
		strings.Contains(got, "postMessage") {
		t.Fatal("dashboard must load its activity bridge externally without inline code")
	}
	for _, origin := range []string{"", "https://evil.example/?unexpected=1", "https://user:secret@evil.example"} {
		if string(injectSessionActivity(body, "/proxy/", origin)) != string(body) {
			t.Fatal("invalid parent origin must not create an activity channel")
		}
	}
}
