package main

import "testing"

// TestStandardFinanceNeverReadsSettlementDSN 验证标准来源不读取无关财务凭据。
// 参数：t 为无数据库连接的配置回归上下文。
// 返回值：无；标准来源读取或登记联动财务时失败。
func TestStandardFinanceNeverReadsSettlementDSN(t *testing.T) {
	sources := integrationReadSources(true, func(key string) string {
		if key == "DASHBOARD_FINANCE_SOURCE" {
			return "standard"
		}
		if key == "SETTLEMENT_READ_DSN" {
			t.Error("标准财务读取了联动数据库凭据")
		}
		return ""
	})
	if _, exists := sources["settlement"]; exists {
		t.Fatal("标准来源注册了联动财务")
	}
}

// TestBusinessSourcesRespectModesAndRejectUnsafeConfig 验证启动模式隔离与失败关闭。
// 参数：t 为纯配置测试上下文，不访问外源或数据库。
// 返回值：无；非法凭据、URL及无法覆盖趋势的留存窗口被接受时失败。
func TestBusinessSourcesRespectModesAndRejectUnsafeConfig(t *testing.T) {
	if _, err := businessSourcesFromEnv(false, func(key string) string {
		t.Fatalf("关闭时读取了外源配置 %s", key)
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SETTLEMENT_READ_DSN", "SETTLEMENT_FINANCE_URL",
		"SETTLEMENT_MACHINE_CLIENT_ID", "SETTLEMENT_MACHINE_CLIENT_SECRET"} {
		t.Run("standard/"+key, func(t *testing.T) {
			_, err := businessSourcesFromEnv(true, func(name string) string {
				if name == key {
					t.Error("标准模式读取了联动来源")
				}
				if name == "DASHBOARD_FINANCE_SOURCE" {
					return "standard"
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, values := range []map[string]string{
		{"DASHBOARD_FINANCE_SOURCE": "unknown"},
		{"PROJECT_REPORT_URL": "https://user:secret@example.com/reports"},
		{"PROJECT_REPORT_URL": "http://project/reports"},
		{"SETTLEMENT_FINANCE_URL": "ftp://settlement/finance"},
		{"SETTLEMENT_FINANCE_URL": "http://settlement/finance"},
		{"DASHBOARD_SNAPSHOT_TREND_MONTHS": "23"},
		{"DASHBOARD_SNAPSHOT_RECENT_DAYS": "0"},
		{"DASHBOARD_SNAPSHOT_BATCH_SIZE": "501"},
		{"DASHBOARD_SNAPSHOT_MAX_BATCHES": "21"},
	} {
		if _, err := businessSourcesFromEnv(true, func(key string) string {
			return values[key]
		}); err == nil {
			t.Fatal("接受了非法配置键集合", values)
		}
	}
}
