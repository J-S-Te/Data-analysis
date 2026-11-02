package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnDisabledSourcesOnlyEmpty 验证启动告警只命中空 DSN 源，且日志不含 DSN 值。
// 参数：t 为测试上下文。
// 返回值：无，告警数量/源编码/变量名不符或泄漏 DSN 时失败。
func TestWarnDisabledSourcesOnlyEmpty(t *testing.T) {
	const secret = "reader-credential-must-not-leak"
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantOff []string
	}{
		{
			name:    "all empty",
			env:     map[string]string{},
			wantOff: []string{"contract_management", "customer_and_opportunity", "project_management", "settlement"},
		},
		{
			name: "all configured",
			env: map[string]string{
				"CONTRACT_READ_DSN": secret, "CRM_READ_DSN": secret,
				"PROJECT_READ_DSN": secret, "SETTLEMENT_READ_DSN": secret,
			},
			wantOff: nil,
		},
		{
			name:    "only settlement configured",
			env:     map[string]string{"SETTLEMENT_READ_DSN": secret, "CRM_READ_DSN": ""},
			wantOff: []string{"contract_management", "customer_and_opportunity", "project_management"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			getenv := func(key string) string { return tc.env[key] }

			warnDisabledSources(getenv, logger)

			if strings.Contains(buf.String(), secret) {
				t.Fatal("read DSN value leaked into startup warning")
			}
			var warned []string
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("startup warning is not JSON: %v", err)
				}
				if record["level"] != "WARN" || record["msg"] != "aggregation source disabled: read DSN is empty" {
					t.Fatalf("unexpected log record: %v", record)
				}
				code, _ := record["source"].(string)
				if record["env"] != readDSNEnv(code) {
					t.Fatalf("env mismatch for source %q: %v", code, record)
				}
				warned = append(warned, code)
			}
			if len(warned) != len(tc.wantOff) {
				t.Fatalf("warned %v, want %v", warned, tc.wantOff)
			}
			for i, code := range tc.wantOff {
				if warned[i] != code {
					t.Fatalf("warned %v, want %v", warned, tc.wantOff)
				}
			}
		})
	}
}
