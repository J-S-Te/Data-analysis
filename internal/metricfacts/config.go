package metricfacts

import (
	"errors"
	"strconv"
	"strings"
)

// EnabledFromEnv 解析新增指标采集开关，兼容旧环境未配置时关闭。
// 参数：getenv为受控环境读取函数。
// 返回值：明确启用状态；非法值返回不包含配置原值的错误。
func EnabledFromEnv(getenv func(string) string) (bool, error) {
	raw := strings.TrimSpace(getenv("METRIC_FACTS_SYNC_ENABLED"))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("METRIC_FACTS_SYNC_ENABLED必须是明确布尔值")
	}
	return value, nil
}
