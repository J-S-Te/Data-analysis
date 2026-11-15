package metricfacts

import "testing"

// TestMetricFactsUsesSharedOrganizationNamespace 验证组织并集和团队命名空间守卫。
// 参数：t为测试上下文。
// 返回值：无；少算授权组织或混合团队命名空间未拒绝时失败。
func TestMetricFactsUsesSharedOrganizationNamespace(t *testing.T) {
	bundle := Bundle{SourceCode: "contract_management", Rows: []Row{
		{Kind: "discount", Status: "UNKNOWN", OrganizationID: "team-org"},
	}}
	out, err := Summarize(bundle, "contract", "", Scope{Mode: "ORG",
		OrganizationIDs: []string{"a"}, TeamIDs: []string{"team-org"}})
	if err != nil || out.DiscountUnknown != 1 {
		t.Fatalf("shared ORG grant lost: %+v %v", out, err)
	}
	if _, err := Summarize(bundle, "contract", "", Scope{Mode: "TEAM",
		OrganizationIDs: []string{"a"}, TeamIDs: []string{"team-org"}}); err == nil {
		t.Fatal("ambiguous TEAM namespace accepted")
	}
}
