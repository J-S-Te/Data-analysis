// Package integrationsync 定义数析外部业务同步总开关，不控制许可消费者或本地事实。
package integrationsync

import "errors"

const EnvironmentKey = "DASHBOARD_INTEGRATION_SYNC_ENABLED"

// EnabledFromEnv 读取兼容默认开启的外部业务同步模式。
// 参数：getenv 为运行环境读取函数，不用于输出凭据。
// 返回值：缺省或true时启用，false时关闭；其他值返回配置错误。
func EnabledFromEnv(getenv func(string) string) (bool, error) {
	switch getenv(EnvironmentKey) {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("invalid DASHBOARD_INTEGRATION_SYNC_ENABLED: must be true or false")
	}
}
