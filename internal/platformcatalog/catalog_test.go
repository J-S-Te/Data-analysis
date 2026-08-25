package platformcatalog

import "testing"

// TestValidateOptionsRejectsOnboardingPlaceholders 确保未完成接入的占位符在发起
// HTTP 请求前被拦截，避免平台返回的 401/403 掩盖真实的本地配置问题。
func TestValidateOptionsRejectsOnboardingPlaceholders(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		options Options
	}{
		{
			name: "application ID",
			options: Options{
				BaseURL: "http://platform-api:8080", ApplicationID: "PENDING_ONBOARDING",
				ClientID: "publisher", ClientSecret: "secret",
			},
		},
		{
			name: "client secret",
			options: Options{
				BaseURL: "http://platform-api:8080", ApplicationID: "app-1",
				ClientID: "publisher", ClientSecret: "REPLACE_WITH_CLIENT_SECRET",
			},
		},
	}
	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := validateOptions(testCase.options); err == nil {
				t.Fatal("validateOptions accepted an onboarding placeholder")
			}
		})
	}
}
