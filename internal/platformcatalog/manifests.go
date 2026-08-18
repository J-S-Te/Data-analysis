package platformcatalog

// DataAnalysisManifest 数据看板子系统目录：看板查看/预警/字典/管理权限。
// 与 authz/permission-manifest.yaml 保持一致（V1）。行级数据范围由 dashboard-api 服务层实现。
func DataAnalysisManifest() Manifest {
	permissions := []Permission{
		permission("dashboard.overview.view", "查看经营总览", "dashboard_overview", "view", "LOW"),
		permission("dashboard.contract.view", "查看合同看板", "dashboard_contract", "view", "LOW"),
		permission("dashboard.project.view", "查看项目执行看板", "dashboard_project", "view", "LOW"),
		permission("dashboard.report.view", "查看报告与质量看板", "dashboard_report", "view", "LOW"),
		permission("dashboard.finance.view", "查看财务与经营看板", "dashboard_finance", "view", "MEDIUM"),
		permission("alert.view", "查看预警中心", "alert", "read", "LOW"),
		permission("alert.manage", "配置预警规则", "alert_rule", "manage", "HIGH"),
		permission("dictionary.view", "查阅指标字典", "metric_dictionary", "read", "LOW"),
		permission("aggregation.manage", "管理数据源与同步", "aggregation", "manage", "HIGH"),
	}
	roles := []Role{
		// admin 是平台 onboarding 的约定初始管理员角色（hardcodedInitialSubsystemAdministratorRoles
		// 对未声明清单的子系统默认分配 "admin"）；语义对齐 contract 的超级管理员（全部权限）。
		role("admin", "超级管理员", "平台初始管理员；拥有全部看板、预警、字典与管理权限",
			"dashboard.overview.view", "dashboard.contract.view", "dashboard.project.view", "dashboard.report.view", "dashboard.finance.view", "alert.view", "alert.manage", "dictionary.view", "aggregation.manage"),
		role("boss", "老板/总经理", "查看全部看板与预警（全量数据范围）",
			"dashboard.overview.view", "dashboard.contract.view", "dashboard.project.view", "dashboard.report.view", "dashboard.finance.view", "alert.view", "dictionary.view"),
		role("sales_director", "销售总监", "合同/财务看板（本人及下属数据范围）",
			"dashboard.overview.view", "dashboard.contract.view", "dashboard.finance.view", "alert.view", "dictionary.view"),
		role("tech_director", "技术总监", "项目/报告看板（管辖团队数据范围）",
			"dashboard.overview.view", "dashboard.project.view", "dashboard.report.view", "alert.view", "dictionary.view"),
		role("finance_manager", "财务经理", "财务/合同看板（全量财务视角）",
			"dashboard.overview.view", "dashboard.contract.view", "dashboard.finance.view", "alert.view", "dictionary.view"),
		role("dashboard_admin", "看板系统管理员", "预警规则与数据源配置",
			"dashboard.overview.view", "dictionary.view", "alert.manage", "aggregation.manage"),
	}
	return Manifest{
		Version:     "data-analysis-v1",
		Permissions: permissions,
		Roles:       roles,
		Policy:      Policy{MaxEffectiveRoles: 8},
	}
}

func permission(code, name, resource, action, risk string) Permission {
	return Permission{Code: code, Name: name, ResourceCode: resource, ResourceName: resource, Action: action, RiskLevel: risk}
}

func role(code, name, description string, permissions ...string) Role {
	return Role{Code: code, Name: name, Description: description, Permissions: permissions}
}
