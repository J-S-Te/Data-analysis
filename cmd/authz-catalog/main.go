// Command authz-catalog 输出并发布编译进 dashboard-api 二进制的授权目录。
// print 子命令供发布脚本读取内嵌 Claims 兼容哈希并原子写回运行配置；
// publish 子命令把目录推送到基础平台授权目录接口。目录定义见 ../authz/permission-manifest.yaml。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformcatalog"
)

func main() {
	action, application := parseArguments(os.Args[1:])
	if action == "" || application == "" {
		fmt.Fprintln(os.Stderr, "usage: authz-catalog [print|publish] data_analysis")
		os.Exit(2)
	}
	manifest := platformcatalog.DataAnalysisManifest()
	if action == "publish" {
		// 发布凭据只从容器环境变量读取，绝不通过命令行参数传递客户端 Secret。
		var options platformcatalog.Options
		options.Enabled = true
		options.BaseURL = os.Getenv("PLATFORM_BASE_URL")
		options.ApplicationID = os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID")
		options.ClientID = os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID")
		options.ClientSecret = os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")
		if err := platformcatalog.Publish(context.Background(), manifest, options); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("authorization catalog published: application=%s\n", application)
		return
	}
	hash, err := platformcatalog.ClaimsRoleConfigHash(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("application=%s\nclaims_role_config_hash=%s\nmax_effective_roles=%d\n", application, hash, manifest.Policy.MaxEffectiveRoles)
}

func parseArguments(arguments []string) (string, string) {
	if len(arguments) == 1 {
		application := strings.ToLower(strings.TrimSpace(arguments[0]))
		if application == "data_analysis" {
			return "print", application
		}
	}
	if len(arguments) == 2 {
		action := strings.ToLower(strings.TrimSpace(arguments[0]))
		application := strings.ToLower(strings.TrimSpace(arguments[1]))
		if (action == "print" || action == "publish") && application == "data_analysis" {
			return action, application
		}
	}
	return "", ""
}
