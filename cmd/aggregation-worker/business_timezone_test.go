package main

import "testing"

// TestBusinessLocationFromEnvFailsClosed 验证聚合进程启动期读取 DASHBOARD_TIMEZONE：
// 未配置/空值默认 Asia/Shanghai（与看板“今天/到期”一致），显式 IANA 名生效，
// 非法值返回错误由 main 记录日志并非零退出。
// 参数：t 为测试上下文。
// 返回值：无；默认值、显式值或非法值行为不符时失败。
func TestBusinessLocationFromEnvFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "unset defaults to Asia/Shanghai", env: map[string]string{}, want: "Asia/Shanghai"},
		{name: "empty defaults to Asia/Shanghai", env: map[string]string{"DASHBOARD_TIMEZONE": ""}, want: "Asia/Shanghai"},
		{name: "explicit valid zone", env: map[string]string{"DASHBOARD_TIMEZONE": "America/New_York"}, want: "America/New_York"},
		{name: "invalid zone must fail startup", env: map[string]string{"DASHBOARD_TIMEZONE": "Mars/Olympus_Mons"}, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			getenv := func(key string) string { return testCase.env[key] }
			location, err := businessLocationFromEnv(getenv)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("businessLocationFromEnv() error = nil, want invalid DASHBOARD_TIMEZONE rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("businessLocationFromEnv() error = %v", err)
			}
			if location.String() != testCase.want {
				t.Fatalf("location = %v, want %s", location, testCase.want)
			}
		})
	}
}
