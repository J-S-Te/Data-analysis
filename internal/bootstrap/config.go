// Package bootstrap 配置装配（对齐 CRM internal/bootstrap/config.go）。
package bootstrap

import (
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr         string
	MySQLDSN           string
	PathPrefix         string
	PublicOrigin       string
	OIDCIssuer         string
	OIDCBackchannel    string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCRedirectURI    string
	OIDCTenantID       string
	OIDCRoleConfigHash string
	OIDCMaxRoles       int
	CodecKey           []byte
	SecureCookie       bool

	PlatformBaseURL      string
	CatalogSyncEnabled   bool
	CatalogApplicationID string
	CatalogClientID      string
	CatalogClientSecret  string
	AuditClientID        string
	AuditClientSecret    string
	AuditApplicationCode string
	AuditEnvironmentCode string
	AuditWorkerID        string

	AuthorizationContextURL string
	MetabaseInternalURL     string
	MetabaseBasePath        string
	MetabaseEmbeddingSecret string
	DashboardIDs            map[string]string
}

func LoadConfig() (Config, error) {
	codecKeyHex := os.Getenv("OIDC_CODEC_KEY")
	if len(codecKeyHex) != 64 {
		return Config{}, errors.New("OIDC_CODEC_KEY must be 32 bytes hex")
	}
	codecKey, err := hex.DecodeString(codecKeyHex)
	if err != nil {
		return Config{}, err
	}
	maxRoles, _ := strconv.Atoi(os.Getenv("OIDC_MAX_ROLES"))
	if maxRoles <= 0 {
		maxRoles = 8
	}
	pathPrefix := envOr("APP_PATH_PREFIX", "/data_analysis")
	// 浏览器 origin（同源/CSRF 校验）与平台 API 地址分离：PLATFORM_BASE_URL 在容器内指向 platform-api。
	publicOrigin := envOr("APP_PUBLIC_ORIGIN", "http://localhost:8081")
	secure, _ := strconv.ParseBool(envOr("COOKIE_SECURE", "false"))
	return Config{
		ListenAddr:         envOr("LISTEN_ADDR", ":8080"),
		MySQLDSN:           os.Getenv("DASHBOARD_MYSQL_DSN"),
		PathPrefix:         pathPrefix,
		PublicOrigin:       strings.TrimRight(publicOrigin, "/"),
		OIDCIssuer:         envOr("OIDC_ISSUER", "http://localhost:18090/realms/basic-platform"),
		OIDCBackchannel:    os.Getenv("OIDC_BACKCHANNEL_BASE_URL"),
		OIDCClientID:       os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:   os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURI:    os.Getenv("OIDC_REDIRECT_URI"),
		OIDCTenantID:       os.Getenv("OIDC_TENANT_ID"),
		OIDCRoleConfigHash: os.Getenv("OIDC_ROLE_CONFIG_HASH"),
		OIDCMaxRoles:       maxRoles,
		CodecKey:           codecKey,
		SecureCookie:       secure,

		PlatformBaseURL:      strings.TrimRight(envOr("PLATFORM_BASE_URL", publicOrigin), "/"),
		CatalogSyncEnabled:   envBool("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED", false),
		CatalogApplicationID: os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"),
		CatalogClientID:      os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"),
		CatalogClientSecret:  os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"),
		AuditClientID:        os.Getenv("AUDIT_INGEST_CLIENT_ID"),
		AuditClientSecret:    os.Getenv("AUDIT_INGEST_CLIENT_SECRET"),
		AuditApplicationCode: envOr("PLATFORM_APPLICATION_CODE", "data_analysis"),
		AuditEnvironmentCode: envOr("PLATFORM_ENVIRONMENT_CODE", "dev"),
		AuditWorkerID:        envOr("AUDIT_WORKER_ID", "dashboard-api"),

		AuthorizationContextURL: envOr("PLATFORM_AUTHORIZATION_CONTEXT_URL", "http://platform-api:8080/oauth2/authorization-context"),
		MetabaseInternalURL:     envOr("METABASE_INTERNAL_URL", "http://metabase:3000"),
		MetabaseBasePath:        strings.TrimRight(envOr("METABASE_BASE_PATH", "/"), "/"),
		MetabaseEmbeddingSecret: os.Getenv("METABASE_EMBEDDING_SECRET"),
		DashboardIDs: map[string]string{
			"overview": envOr("MB_DASHBOARD_ID_OVERVIEW", ""),
			"contract": envOr("MB_DASHBOARD_ID_CONTRACT", ""),
			"project":  envOr("MB_DASHBOARD_ID_PROJECT", ""),
			"report":   envOr("MB_DASHBOARD_ID_REPORT", ""),
			"finance":  envOr("MB_DASHBOARD_ID_FINANCE", ""),
		},
	}, nil
}

func envOr(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

var _ = time.Now
