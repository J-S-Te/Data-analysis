package aggregation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

// APISyncOptions 看板系统经子系统 internal 接口同步的配置（设计方案：看板数据经接口获取）。
type APISyncOptions struct {
	// 机器令牌由基础平台 OAuth client_credentials 端点签发。
	MachineTokenURL      string
	MachineTokenIssuer   string
	MachineTokenAudience string
	ContractCredential   MachineCredential
	ProjectCredential    MachineCredential
	// 子系统 internal 接口
	ContractInternalURL string
	ProjectInternalURL  string
	TenantID            string
	HTTPTimeout         time.Duration
}

// MachineCredential 是单个上游读取用途的最小权限凭据。
// 合同和项目必须分别配置，禁止复用同一个 Client Secret。
type MachineCredential struct {
	ClientID     string
	ClientSecret string
	Scope        string
}

type apiContractDashboard struct {
	TenantID          string `json:"tenant_id"`
	TotalAmountMinor  int64  `json:"total_amount_minor"`
	TotalContracts    int64  `json:"total_contracts"`
	ApprovalContracts int64  `json:"approval_contracts"`
	ActiveContracts   int64  `json:"active_contracts"`
	ExpiredContracts  int64  `json:"expired_contracts"`
}

// contractDashboardSnapshot 表示经机器接口租户校验后待持久化的合同看板快照。
// HTTP 响应映射与数据库写入分离，避免测试依赖真实 MySQL。
type contractDashboardSnapshot struct {
	TenantID          string
	SnapshotAt        time.Time
	TotalAmountMinor  int64
	TotalContracts    int64
	ApprovalContracts int64
	ActiveContracts   int64
	ExpiredContracts  int64
}

type contractSnapshotSink func(context.Context, contractDashboardSnapshot) error

// APISyncRunner 接口同步执行器（写入聚合库快照表，供 Metabase 渲染）。
type APISyncRunner struct {
	db      *gorm.DB
	options APISyncOptions
	tokens  map[string]cachedMachineToken
}

type cachedMachineToken struct {
	value     string
	expiresAt time.Time
}

// NewAPISyncRunner 基于聚合库连接与 APISyncOptions 构建 API 同步执行器。
// db 为聚合库连接（未连接时会在持久化阶段触发错误），options 提供机器凭证与子系统仪表盘 URL；
// 方法返回 *APISyncRunner，仅在参数入参错误时做最小修复（如 HTTPTimeout 补齐默认值）不做连接检测。
func NewAPISyncRunner(db *gorm.DB, options APISyncOptions) *APISyncRunner {
	if options.HTTPTimeout <= 0 {
		options.HTTPTimeout = 10 * time.Second
	}
	return &APISyncRunner{db: db, options: options, tokens: make(map[string]cachedMachineToken)}
}

// machineToken 获取并缓存 Keycloak client_credentials 访问令牌。
// 触发条件：
// 1）当前 token 为空或剩余有效期不足 30 秒时发起刷新；
// 2）HTTP 请求失败、状态码非 200 或返回体解析失败都会返回错误；
// 3）成功时返回可复用的 token 并更新本地过期时间。
func (r *APISyncRunner) machineToken(ctx context.Context, credential MachineCredential) (string, error) {
	credential.ClientID = strings.TrimSpace(credential.ClientID)
	credential.Scope = strings.TrimSpace(credential.Scope)
	if cached := r.tokens[credential.ClientID]; cached.value != "" && time.Now().Before(cached.expiresAt.Add(-30*time.Second)) {
		return cached.value, nil
	}
	if strings.TrimSpace(r.options.MachineTokenURL) == "" || credential.ClientID == "" || credential.ClientSecret == "" || credential.Scope == "" {
		return "", errors.New("platform machine credential is incomplete")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {credential.Scope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.options.MachineTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(credential.ClientID, credential.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: r.options.HTTPTimeout}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("machine token returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if err := validateMachineTokenContract(payload.AccessToken, r.options, credential); err != nil {
		return "", err
	}
	if payload.ExpiresIn <= 30 {
		return "", errors.New("platform machine token lifetime is too short")
	}
	r.tokens[credential.ClientID] = cachedMachineToken{value: payload.AccessToken, expiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)}
	return payload.AccessToken, nil
}

type machineAccessTokenClaims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	AuthorizedParty string          `json:"azp"`
	ClientID        string          `json:"client_id"`
	Type            string          `json:"typ"`
	TokenUse        string          `json:"token_use"`
	Scopes          []string        `json:"scope"`
	Audience        json.RawMessage `json:"aud"`
}

// validateMachineTokenContract performs an early configuration-contract check
// on the token obtained directly from Keycloak. This is diagnostic only; the
// contract/project APIs remain responsible for cryptographic verification via
// OIDC discovery/JWKS before accepting the request.
func validateMachineTokenContract(rawToken string, options APISyncOptions, credential MachineCredential) error {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return errors.New("machine token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode machine token claims: %w", err)
	}
	claims := machineAccessTokenClaims{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return fmt.Errorf("decode machine token claims: %w", err)
	}
	if strings.TrimSpace(claims.Type) != "" && !strings.EqualFold(strings.TrimSpace(claims.Type), "jwt") {
		return errors.New("platform machine token typ is not JWT")
	}
	if claims.TokenUse != "application" {
		return errors.New("platform machine token_use is not application")
	}
	if issuer := strings.TrimRight(strings.TrimSpace(options.MachineTokenIssuer), "/"); issuer != "" && strings.TrimRight(strings.TrimSpace(claims.Issuer), "/") != issuer {
		return fmt.Errorf("machine token issuer mismatch: got %q", claims.Issuer)
	}
	clientID := strings.TrimSpace(credential.ClientID)
	// 基础平台签发的 application JWT 用 sub 承载外部 client_id（如
	// data_analysis-prod-dashboard-reader）；老版本 Keycloak 机器令牌用 azp。
	// 优先校验 sub，缺失时回退到 azp，client_id 若存在也必须一致。
	subject := strings.TrimSpace(claims.Subject)
	authorizedParty := strings.TrimSpace(claims.AuthorizedParty)
	subjectMatch := (subject != "" && subject == clientID) || (subject == "" && authorizedParty == clientID)
	if !subjectMatch || (strings.TrimSpace(claims.ClientID) != "" && strings.TrimSpace(claims.ClientID) != clientID) {
		return errors.New("machine token subject/authorized party/client_id mismatch")
	}
	if len(claims.Scopes) != 1 || claims.Scopes[0] != strings.TrimSpace(credential.Scope) {
		return errors.New("platform machine token scope does not match the requested single scope")
	}
	if audience := strings.TrimSpace(options.MachineTokenAudience); audience != "" {
		audiences, err := decodeTokenAudience(claims.Audience)
		if err != nil || !containsTokenAudience(audiences, audience) {
			return fmt.Errorf("machine token audience does not contain %q", audience)
		}
	}
	return nil
}

func decodeTokenAudience(raw json.RawMessage) ([]string, error) {
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []string{one}, nil
}

func containsTokenAudience(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// SyncContractDashboard 调合同系统 internal/dashboard，写 api_contract_dashboard 快照。
func (r *APISyncRunner) SyncContractDashboard(ctx context.Context) error {
	return r.syncContractDashboard(ctx, r.persistContractDashboard)
}

// syncContractDashboard 拉取合同系统 /internal/dashboard 并通过 sink 落库。
// 预期返回：
// - 只在合同接口、租户校验、快照写入都通过时返回 nil；
// - 任一环节失败均返回错误，调用方可根据错误原因决定是否重试。
func (r *APISyncRunner) syncContractDashboard(ctx context.Context, sink contractSnapshotSink) error {
	if strings.TrimSpace(r.options.ContractInternalURL) == "" {
		return errors.New("contract internal URL is not configured")
	}
	tenantID := strings.TrimSpace(r.options.TenantID)
	if tenantID == "" {
		return errors.New("data-analysis tenant ID is required for contract dashboard sync")
	}
	token, err := r.machineToken(ctx, r.options.ContractCredential)
	if err != nil {
		return fmt.Errorf("machine token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.options.ContractInternalURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-DA-Tenant-ID", tenantID)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: r.options.HTTPTimeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("contract internal/dashboard returned HTTP %d: %s", response.StatusCode, errorResponseBody(response.Body))
	}
	var envelope struct {
		Code string               `json:"code"`
		Data apiContractDashboard `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Code != "OK" {
		return fmt.Errorf("contract dashboard envelope code %s", envelope.Code)
	}
	responseTenantID := strings.TrimSpace(envelope.Data.TenantID)
	if responseTenantID != tenantID {
		return fmt.Errorf(
			"contract dashboard tenant mismatch: requested %q, received %q",
			tenantID,
			responseTenantID,
		)
	}
	if sink == nil {
		return errors.New("contract dashboard snapshot sink is not configured")
	}
	return sink(ctx, contractDashboardSnapshot{
		TenantID:          tenantID,
		SnapshotAt:        time.Now().UTC(),
		TotalAmountMinor:  envelope.Data.TotalAmountMinor,
		TotalContracts:    envelope.Data.TotalContracts,
		ApprovalContracts: envelope.Data.ApprovalContracts,
		ActiveContracts:   envelope.Data.ActiveContracts,
		ExpiredContracts:  envelope.Data.ExpiredContracts,
	})
}

func (r *APISyncRunner) persistContractDashboard(ctx context.Context, snapshot contractDashboardSnapshot) error {
	if r.db == nil {
		return errors.New("aggregation database is not configured")
	}
	// 数据库写入失败时返回错误给上层，由调用方统一进行同步重试与告警。
	if err := r.db.Exec(`INSERT INTO api_contract_dashboard
(tenant_id, snapshot_at, total_amount_minor, total_contracts, approval_contracts, active_contracts, expired_contracts, source)
VALUES (?, ?, ?, ?, ?, ?, ?, 'contract-api')`,
		snapshot.TenantID, snapshot.SnapshotAt, snapshot.TotalAmountMinor, snapshot.TotalContracts,
		snapshot.ApprovalContracts, snapshot.ActiveContracts, snapshot.ExpiredContracts).Error; err != nil {
		return err
	}
	return nil
}

// SyncProjectDashboard 调项目系统 internal/dashboard，写 api_project_dashboard 快照。
// 返回错误的典型触发条件包括：项目内置接口未配置、租户 ID 未配置、机器令牌获取失败、
// 内部接口非 200、JSON 反序列化失败，或数据库写入失败。
func (r *APISyncRunner) SyncProjectDashboard(ctx context.Context) error {
	if strings.TrimSpace(r.options.ProjectInternalURL) == "" {
		return errors.New("project internal URL is not configured")
	}
	if strings.TrimSpace(r.options.TenantID) == "" {
		return errors.New("data-analysis tenant ID is required for project dashboard sync")
	}
	token, err := r.machineToken(ctx, r.options.ProjectCredential)
	if err != nil {
		return fmt.Errorf("machine token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.options.ProjectInternalURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-DA-Tenant-ID", r.options.TenantID)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: r.options.HTTPTimeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("project internal/dashboard returned HTTP %d: %s", response.StatusCode, errorResponseBody(response.Body))
	}
	var payload struct {
		Data struct {
			TenantID         string         `json:"tenant_id"`
			ProjectCount     int            `json:"project_count"`
			InFlightProjects int            `json:"in_flight_projects"`
			RiskProjects     int            `json:"risk_projects"`
			ServiceItems     int            `json:"service_items"`
			StatusCounts     map[string]int `json:"status_counts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return err
	}
	if responseTenantID := strings.TrimSpace(payload.Data.TenantID); responseTenantID != strings.TrimSpace(r.options.TenantID) {
		return fmt.Errorf("project dashboard tenant mismatch: requested %q, received %q", r.options.TenantID, responseTenantID)
	}
	statusJSON, _ := json.Marshal(payload.Data.StatusCounts)
	now := time.Now().UTC()
	if err := r.db.Exec(`INSERT INTO api_project_dashboard
(tenant_id, snapshot_at, project_count, in_flight_projects, risk_projects, service_items, status_counts_json, source)
VALUES (?, ?, ?, ?, ?, ?, ?, 'project-api')`,
		r.options.TenantID, now, payload.Data.ProjectCount, payload.Data.InFlightProjects,
		payload.Data.RiskProjects, payload.Data.ServiceItems, string(statusJSON)).Error; err != nil {
		return err
	}
	return nil
}

// errorResponseBody returns a short, control-character-free excerpt of an
// upstream error body for diagnostics. It deliberately does not echo the full
// response into logs, avoiding accidental exposure of sensitive upstream content.
func errorResponseBody(reader io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(reader, 256))
	return strings.TrimSpace(strings.Map(func(ch rune) rune {
		if ch < 0x20 && ch != '\t' {
			return -1
		}
		return ch
	}, string(raw)))
}

var _ = bytes.MinRead
