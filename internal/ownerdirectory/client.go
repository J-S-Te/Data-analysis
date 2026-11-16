// Package ownerdirectory 读取平台负责人目录，用于把用户 ID 解析为姓名。
package ownerdirectory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultScope = "owner_directory.read"

// Client 使用独立机器凭据读取平台负责人目录；配置不完整时为 nil。
type Client struct {
	endpoint, tokenURL, clientID, clientSecret, scope string
	http                                              *http.Client
	mu                                                sync.Mutex
	token                                             string
	expiry                                            time.Time
}

// New 从独立环境变量创建目录读取客户端。
// 参数：getenv 为配置读取器，仅使用最小权限专用机器凭据。
// 返回值：完整配置的客户端；未配置时返回 nil，不影响既有ID显示。
func New(getenv func(string) string) *Client {
	if getenv == nil {
		return nil
	}
	endpoint := strings.TrimSpace(getenv("PLATFORM_OWNER_DIRECTORY_URL"))
	tokenURL := strings.TrimSpace(getenv("MACHINE_TOKEN_URL"))
	clientID := strings.TrimSpace(getenv("PLATFORM_OWNER_DIRECTORY_CLIENT_ID"))
	clientSecret := strings.TrimSpace(getenv("PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"))
	scope := strings.TrimSpace(getenv("PLATFORM_OWNER_DIRECTORY_SCOPE"))
	if endpoint == "" || tokenURL == "" || clientID == "" || clientSecret == "" {
		return nil
	}
	if scope == "" {
		scope = defaultScope
	}
	return &Client{endpoint: endpoint, tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret, scope: scope, http: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Lookup 按稳定用户ID精确查询姓名。
// 参数：ctx 为可取消请求，userID 为当前租户的用户ID。
// 返回值：匹配姓名或空字符串；来源失败返回脱敏错误。
func (c *Client) Lookup(ctx context.Context, userID string) (string, error) {
	if c == nil || strings.TrimSpace(userID) == "" {
		return "", nil
	}
	token, err := c.tokenValue(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("user_id", strings.TrimSpace(userID))
	q.Set("page_size", "1")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if readErr != nil || len(body) > 256<<10 {
		return "", fmt.Errorf("owner directory response invalid")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("owner directory returned %d", resp.StatusCode)
	}
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			Items []struct {
				DisplayName string `json:"display_name"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != "OK" {
		return "", fmt.Errorf("owner directory response invalid")
	}
	if len(envelope.Data.Items) == 0 {
		return "", nil
	}
	return strings.TrimSpace(envelope.Data.Items[0].DisplayName), nil
}

// tokenValue 使用专用单scope身份获取或复用短期机器令牌。
// 参数：ctx 为令牌请求的取消和超时边界。
// 返回值：有效令牌；依赖失败返回不含凭据的错误。
func (c *Client) tokenValue(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expiry) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {c.scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if readErr != nil || len(body) > 64<<10 {
		return "", fmt.Errorf("owner directory token invalid")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("owner directory token returned %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.AccessToken == "" {
		return "", fmt.Errorf("owner directory token invalid")
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Minute
	}
	c.mu.Lock()
	c.token = payload.AccessToken
	c.expiry = time.Now().Add(lifetime - 30*time.Second)
	c.mu.Unlock()
	return payload.AccessToken, nil
}
