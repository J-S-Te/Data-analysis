package domain

import "strings"

// PublicSyncFailure 用途：将已有同步诊断转换为稳定公开分类，不改变任务或来源状态。
// 参数：raw 为持久化的内部诊断，仅用于匹配已知故障特征。
// 返回值：公开故障码和安全中文指引；空诊断返回空，不回显任意原文。
func PublicSyncFailure(raw string) (string, string) {
	text := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case text == "":
		return "", ""
	case strings.Contains(text, "platform machine credential is incomplete"):
		return "SOURCE_CREDENTIAL_INCOMPLETE", "数据源接入凭据不完整，请联系管理员核对应用接入与机器访问配置后再同步"
	case strings.Contains(text, "is not configured"), text == "source_not_configured":
		return "SOURCE_NOT_CONFIGURED", "数据源接入配置尚未完成，请联系管理员核对来源配置后再同步"
	case strings.Contains(text, "context deadline exceeded"), strings.Contains(text, "source timeout"), text == "source_timeout":
		return "SOURCE_TIMEOUT", "数据源响应超时，已有快照继续保留；请稍后查看同步状态，仍失败时联系管理员检查来源服务"
	case strings.Contains(text, "http 401"), strings.Contains(text, "http 403"):
		return "SOURCE_AUTHORIZATION_FAILED", "数据源访问授权未通过，请联系管理员核对机器身份、授权范围与接入状态"
	default:
		return "SOURCE_UNAVAILABLE", "数据源或同步依赖暂时不可用，已有快照继续保留；请稍后刷新，仍失败时联系管理员核对来源和同步任务"
	}
}
