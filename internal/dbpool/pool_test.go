package dbpool

import (
	"testing"
	"time"
)

// TestBudgetDefaultsAndValidation 验证显式默认预算及非法配置失败关闭。
// 参数：t 为隔离测试上下文。
// 返回值：无；默认值、范围或读取前缀错误时失败。
func TestBudgetDefaultsAndValidation(t *testing.T) {
	got, err := Load("DA_API", func(string) string { return "" })
	if err != nil || got.MaxOpen != 8 || got.MaxIdle != 2 || got.Lifetime != 300*time.Second {
		t.Fatalf("defaults %+v %v", got, err)
	}
	for _, values := range []map[string]string{{"MAX_OPEN": "0"}, {"MAX_OPEN": "-1"}, {"MAX_IDLE": "9"}, {"MAX_OPEN": "101"}, {"LIFETIME_SECONDS": "0"}, {"IDLE_SECONDS": "bad"}} {
		if _, err := Load("DA_API", func(key string) string { return values[key[len("DA_API_DB_"):]] }); err == nil {
			t.Fatalf("accepted %v", values)
		}
	}
}
