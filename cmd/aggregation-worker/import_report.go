package main

import (
	"errors"
	"net/url"
	"os"
	"strconv"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

type importReportOptions struct {
	Enabled    bool
	URL        string
	TokenURL   string
	Credential aggregation.MachineCredential
}

// importReportOptionsFromEnv 读取默认关闭的独立历史摘要同步配置。
// 参数：无，配置来自CONTRACT_IMPORT_SYNC_*及SUMMARY_URL环境变量。
// 返回值：已校验的配置；启用时缺少独立用途凭据或完整URL返回错误。
func importReportOptionsFromEnv() (importReportOptions, error) {
	o := importReportOptions{}
	if raw := os.Getenv("CONTRACT_IMPORT_SYNC_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return o, errors.New("invalid CONTRACT_IMPORT_SYNC_ENABLED")
		}
		o.Enabled = enabled
	}
	if !o.Enabled {
		return o, nil
	}
	o.URL = os.Getenv("CONTRACT_IMPORT_SUMMARY_URL")
	o.TokenURL = os.Getenv("CONTRACT_IMPORT_SYNC_TOKEN_URL")
	o.Credential = aggregation.MachineCredential{ClientID: os.Getenv("CONTRACT_IMPORT_SYNC_CLIENT_ID"),
		ClientSecret: os.Getenv("CONTRACT_IMPORT_SYNC_CLIENT_SECRET"),
		Scope:        firstNonEmpty(os.Getenv("CONTRACT_IMPORT_SYNC_SCOPE"), "contract.import.report.read"),
	}
	if o.Credential.ClientID == "" || o.Credential.ClientSecret == "" || o.Credential.Scope != "contract.import.report.read" || os.Getenv("OIDC_TENANT_ID") == "" {
		return o, errors.New("incomplete contract import report credential")
	}
	if o.Credential.ClientID == os.Getenv("CONTRACT_MACHINE_CLIENT_ID") || o.Credential.ClientID == os.Getenv("PROJECT_MACHINE_CLIENT_ID") {
		return o, errors.New("contract import report requires an independent client")
	}
	for _, raw := range []string{o.URL, o.TokenURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return o, errors.New("contract import report requires complete HTTP URLs")
		}
	}
	return o, nil
}
