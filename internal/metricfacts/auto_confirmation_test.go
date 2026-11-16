package metricfacts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestProjectAutomaticConfirmationCompatibility 验证项目真实自动确认字段可被旧包协议接收并单列。
// 参数：t 为当前测试上下文，所有金额及人员数据均为合成夹具。
// 返回值：无；解码拒绝有效字段、漏记自动确认或接受非法子集时失败。
func TestProjectAutomaticConfirmationCompatibility(t *testing.T) {
	now := time.Now().UTC()
	base := `{"code":"OK","data":{"schema_version":1,"tenant_id":"t","source_code":"project_management","snapshot_at":"` + now.Format(time.RFC3339Nano) + `","complete":true,"basis":"test","rows":[{"kind":"person_utilization","record_id":"r","period":"2026-11","status":"CONFIRMED","actual_minutes":60,"capacity_minutes":120,"pending_count":0,"unknown_count":0,"auto_confirmed_minutes":30,"auto_confirmed_count":1}]}}`
	bundle, err := Decode(strings.NewReader(base), "t", "project_management", now)
	if err != nil {
		t.Fatalf("valid current project producer rejected: %v", err)
	}
	out, err := Summarize(bundle, "project", "2026-11", Scope{Mode: "TENANT"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Person)
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	if values["auto_confirmed_minutes"] != float64(30) || values["auto_confirmed_count"] != float64(1) {
		t.Fatalf("automatic subset missing: %s", raw)
	}
	for _, invalid := range []string{
		strings.Replace(base, `"auto_confirmed_minutes":30`, `"auto_confirmed_minutes":61`, 1),
		strings.Replace(base, `"auto_confirmed_count":1`, `"auto_confirmed_count":-1`, 1),
	} {
		if _, err := Decode(strings.NewReader(invalid), "t", "project_management", now); err == nil {
			t.Fatal("invalid automatic subset accepted")
		}
	}
}
