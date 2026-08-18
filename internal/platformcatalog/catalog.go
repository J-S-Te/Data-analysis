// Package platformcatalog 使用专用的 OAuth 客户端凭据，把应用自有授权目录发布到基础平台。
package platformcatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const syncScope = "authorization.catalog.sync"

// Manifest 是应用自有的角色与权限目录。机器集成 scope 有意排除在外：它们授予 OAuth 客户端，
// 不能混入浏览器用户角色。
type Manifest struct {
	Version     string
	Permissions []Permission
	Roles       []Role
	Policy      Policy
}

type Permission struct {
	Code, Name, Action, ResourceCode, ResourceName, RiskLevel string
}

type Role struct {
	Code, Name, Description string
	Permissions             []string
}

type Policy struct {
	MaxEffectiveRoles int
}

// Options 标识平台端点及子系统接入时创建、与应用绑定的目录发布凭据。
type Options struct {
	Enabled                                        bool
	BaseURL, ApplicationID, ClientID, ClientSecret string
	HTTPClient                                     *http.Client
}

type catalogPayload struct {
	CatalogVersion       string              `json:"catalog_version"`
	Checksum             string              `json:"checksum"`
	ClaimsRoleConfigHash string              `json:"claims_role_config_hash"`
	Permissions          []catalogPermission `json:"permissions"`
	Roles                []catalogRole       `json:"roles"`
	Policy               catalogPolicy       `json:"policy"`
}

type catalogPermission struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	ResourceCode string `json:"resource_code"`
	ResourceName string `json:"resource_name,omitempty"`
	RiskLevel    string `json:"risk_level"`
}

type catalogRole struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
}

type catalogPolicy struct {
	MaxEffectiveRoles int `json:"max_effective_roles"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// 启用同步后，目录校验、取令牌或发布任一步失败都会返回启动方；服务应失败退出，不能在平台
// 仍使用旧目录时继续解释新版本 Claims。
func Publish(ctx context.Context, manifest Manifest, options Options) error {
	if !options.Enabled {
		return nil
	}
	payload, err := normalizeManifest(manifest)
	if err != nil {
		return err
	}
	baseURL, err := validateOptions(options)
	if err != nil {
		return err
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// 发布凭据和 Bearer Token 不得跨越 HTTP 重定向边界，即使调用方注入了自定义客户端也一样。
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	token, err := requestAccessToken(ctx, client, baseURL, options)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode authorization catalog: %w", err)
	}
	endpoint := strings.TrimRight(baseURL.String(), "/") + "/api/v1/applications/" + url.PathEscape(options.ApplicationID) + "/authorization-catalog"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create authorization catalog request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("publish authorization catalog: platform transport failed: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("platform authorization catalog returned HTTP %d", response.StatusCode)
	}
	return nil
}

// 返回发布给平台并要求出现在 OIDC Claims 中的不透明兼容哈希，用于绑定角色到权限的解释版本。
func ClaimsRoleConfigHash(manifest Manifest) (string, error) {
	payload, err := normalizeManifest(manifest)
	if err != nil {
		return "", err
	}
	return payload.ClaimsRoleConfigHash, nil
}

// 完整目录校验和覆盖展示元数据和策略；它与 Claims 兼容哈希分离，后者只跟踪会改变授权解释的
// 角色—权限映射。
func CatalogChecksum(manifest Manifest) (string, error) {
	payload, err := normalizeManifest(manifest)
	if err != nil {
		return "", err
	}
	return payload.Checksum, nil
}

// 认证校验与发布流程复用同一目录，避免本地允许列表和平台目录各自维护后发生漂移。
func HasPermission(manifest Manifest, expected string) bool {
	for _, item := range manifest.Permissions {
		if item.Code == expected {
			return true
		}
	}
	return false
}

func HasRole(manifest Manifest, expected string) bool {
	for _, item := range manifest.Roles {
		if item.Code == expected {
			return true
		}
	}
	return false
}

// 校验运行时 OIDC 配置期望的映射与当前二进制内置目录完全一致，滚动发布中的不兼容副本会拒绝启动。
func ValidateClaimsRoleConfigHash(manifest Manifest, configured string) error {
	expected, err := ClaimsRoleConfigHash(manifest)
	if err != nil {
		return err
	}
	if strings.TrimSpace(configured) != expected {
		return fmt.Errorf("OIDC role configuration hash does not match embedded authorization catalog; expected %s", expected)
	}
	return nil
}

func normalizeManifest(manifest Manifest) (catalogPayload, error) {
	manifest.Version = strings.TrimSpace(manifest.Version)
	if manifest.Version == "" || len(manifest.Permissions) == 0 || len(manifest.Roles) == 0 {
		return catalogPayload{}, errors.New("authorization catalog manifest is incomplete")
	}
	if manifest.Policy.MaxEffectiveRoles < 0 || manifest.Policy.MaxEffectiveRoles > 10 {
		return catalogPayload{}, errors.New("authorization catalog max_effective_roles must be between 0 and 10")
	}
	permissionCodes := make(map[string]struct{}, len(manifest.Permissions))
	resourceActions := make(map[string]struct{}, len(manifest.Permissions))
	payload := catalogPayload{CatalogVersion: manifest.Version, Policy: catalogPolicy{MaxEffectiveRoles: manifest.Policy.MaxEffectiveRoles}}
	for _, item := range manifest.Permissions {
		item.Code, item.Name = strings.TrimSpace(item.Code), strings.TrimSpace(item.Name)
		item.Action, item.ResourceCode = strings.TrimSpace(item.Action), strings.TrimSpace(item.ResourceCode)
		item.ResourceName, item.RiskLevel = strings.TrimSpace(item.ResourceName), strings.ToUpper(strings.TrimSpace(item.RiskLevel))
		if item.Code == "" || item.Name == "" || item.Action == "" || item.ResourceCode == "" {
			return catalogPayload{}, errors.New("authorization catalog permission is incomplete")
		}
		if item.RiskLevel != "LOW" && item.RiskLevel != "MEDIUM" && item.RiskLevel != "HIGH" && item.RiskLevel != "CRITICAL" {
			return catalogPayload{}, fmt.Errorf("authorization catalog permission %s has invalid risk level", item.Code)
		}
		if _, duplicate := permissionCodes[item.Code]; duplicate {
			return catalogPayload{}, fmt.Errorf("authorization catalog contains duplicate permission %s", item.Code)
		}
		resourceAction := item.ResourceCode + "\x00" + strings.ToLower(item.Action)
		if _, duplicate := resourceActions[resourceAction]; duplicate {
			return catalogPayload{}, fmt.Errorf("authorization catalog contains duplicate resource/action %s/%s", item.ResourceCode, item.Action)
		}
		permissionCodes[item.Code], resourceActions[resourceAction] = struct{}{}, struct{}{}
		payload.Permissions = append(payload.Permissions, catalogPermission{Code: item.Code, Name: item.Name, Action: item.Action, ResourceCode: item.ResourceCode, ResourceName: item.ResourceName, RiskLevel: item.RiskLevel})
	}
	sort.Slice(payload.Permissions, func(i, j int) bool { return payload.Permissions[i].Code < payload.Permissions[j].Code })
	roleCodes := make(map[string]struct{}, len(manifest.Roles))
	for _, item := range manifest.Roles {
		item.Code, item.Name, item.Description = strings.TrimSpace(item.Code), strings.TrimSpace(item.Name), strings.TrimSpace(item.Description)
		if item.Code == "" || item.Name == "" {
			return catalogPayload{}, errors.New("authorization catalog role is incomplete")
		}
		if _, duplicate := roleCodes[item.Code]; duplicate {
			return catalogPayload{}, fmt.Errorf("authorization catalog contains duplicate role %s", item.Code)
		}
		roleCodes[item.Code] = struct{}{}
		permissions := sortedUnique(item.Permissions)
		for _, permission := range permissions {
			if _, exists := permissionCodes[permission]; !exists {
				return catalogPayload{}, fmt.Errorf("authorization catalog role %s references unknown permission %s", item.Code, permission)
			}
		}
		payload.Roles = append(payload.Roles, catalogRole{Code: item.Code, Name: item.Name, Description: item.Description, Permissions: permissions})
	}
	sort.Slice(payload.Roles, func(i, j int) bool { return payload.Roles[i].Code < payload.Roles[j].Code })
	catalogCanonical := struct {
		Version     string              `json:"catalog_version"`
		Permissions []catalogPermission `json:"permissions"`
		Roles       []catalogRole       `json:"roles"`
		Policy      catalogPolicy       `json:"policy"`
	}{manifest.Version, payload.Permissions, payload.Roles, payload.Policy}
	catalogEncoded, err := json.Marshal(catalogCanonical)
	if err != nil {
		return catalogPayload{}, fmt.Errorf("encode authorization catalog checksum: %w", err)
	}
	catalogSum := sha256.Sum256(catalogEncoded)
	payload.Checksum = "sha256:" + hex.EncodeToString(catalogSum[:])
	// Claims 兼容性只绑定稳定角色码及权限映射；中文名称和说明属于展示元数据，文案调整不应使
	// 所有活动会话失效。
	type claimsRole struct {
		Code        string   `json:"code"`
		Permissions []string `json:"permissions"`
	}
	canonical := struct {
		Roles []claimsRole `json:"roles"`
	}{Roles: make([]claimsRole, 0, len(payload.Roles))}
	for _, item := range payload.Roles {
		canonical.Roles = append(canonical.Roles, claimsRole{Code: item.Code, Permissions: item.Permissions})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return catalogPayload{}, fmt.Errorf("encode authorization Claims compatibility mapping: %w", err)
	}
	sum := sha256.Sum256(encoded)
	payload.ClaimsRoleConfigHash = "sha256:" + hex.EncodeToString(sum[:])
	return payload, nil
}

func validateOptions(options Options) (*url.URL, error) {
	if strings.TrimSpace(options.BaseURL) == "" || strings.TrimSpace(options.ApplicationID) == "" || strings.TrimSpace(options.ClientID) == "" || options.ClientSecret == "" {
		return nil, errors.New("authorization catalog synchronization configuration is incomplete")
	}
	parsed, err := url.ParseRequestURI(options.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("authorization catalog platform base URL must be an HTTP(S) origin")
	}
	return parsed, nil
}

func requestAccessToken(ctx context.Context, client *http.Client, baseURL *url.URL, options Options) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {syncScope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL.String(), "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create authorization catalog token request: %w", err)
	}
	request.SetBasicAuth(options.ClientID, options.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request authorization catalog token: platform transport failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", fmt.Errorf("platform authorization catalog token returned HTTP %d", response.StatusCode)
	}
	var token tokenResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&token); err != nil {
		return "", errors.New("decode authorization catalog token: invalid platform response")
	}
	if strings.TrimSpace(token.AccessToken) == "" || !strings.EqualFold(token.TokenType, "bearer") || !containsScope(token.Scope, syncScope) {
		return "", errors.New("platform token is missing authorization.catalog.sync scope")
	}
	return token.AccessToken, nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsScope(value, expected string) bool {
	for _, scope := range strings.Fields(value) {
		if scope == expected {
			return true
		}
	}
	return false
}
