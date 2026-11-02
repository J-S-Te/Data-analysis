package main

import (
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

// TestOverlapGuardFromEnv 验证 AGG_RUN_OVERLAP_GUARD 的启动期解析：
// 空值默认 warn，enforce 被接受，非法值失败关闭（由 main 记录日志并非零退出）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestOverlapGuardFromEnv(t *testing.T) {
	cases := []struct {
		raw     string
		want    aggregation.RunOverlapGuard
		wantErr bool
	}{
		{raw: "", want: aggregation.RunOverlapGuardWarn},
		{raw: "warn", want: aggregation.RunOverlapGuardWarn},
		{raw: "enforce", want: aggregation.RunOverlapGuardEnforce},
		{raw: " ENFORCE ", want: aggregation.RunOverlapGuardEnforce},
		{raw: "off", wantErr: true},
	}
	for _, tc := range cases {
		getenv := func(key string) string {
			if key != "AGG_RUN_OVERLAP_GUARD" {
				t.Fatalf("unexpected env key %q", key)
			}
			return tc.raw
		}
		got, err := overlapGuardFromEnv(getenv)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("overlapGuardFromEnv(%q) error = nil, want rejection", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("overlapGuardFromEnv(%q) = %q/%v, want %q", tc.raw, got, err, tc.want)
		}
	}
}
