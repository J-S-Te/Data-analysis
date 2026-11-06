package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAlertWorkerRejectsInvalidIntegrationFlag 验证预警进程在许可及数据库初始化前拒绝非法总开关。
// 参数：t 为测试上下文，子进程只运行预期配置失败分支。
// 返回值：无；非法配置未失败或触达后续初始化时失败。
func TestAlertWorkerRejectsInvalidIntegrationFlag(t *testing.T) {
	if os.Getenv("DA_ALERT_FLAG_TEST_CHILD") == "1" {
		main()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAlertWorkerRejectsInvalidIntegrationFlag$")
	command.Env = append(os.Environ(), "DA_ALERT_FLAG_TEST_CHILD=1", "DASHBOARD_INTEGRATION_SYNC_ENABLED=invalid", "DASHBOARD_MYSQL_DSN=")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "integration sync configuration invalid") {
		t.Fatalf("startup did not reject flag: err=%v output=%s", err, output)
	}
	if strings.Contains(string(output), "commercial license configuration invalid") || strings.Contains(string(output), "DASHBOARD_MYSQL_DSN is required") {
		t.Fatalf("later initialization reached: %s", output)
	}
}
