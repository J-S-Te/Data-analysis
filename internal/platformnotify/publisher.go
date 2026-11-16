// Package platformnotify 投递平台统一站内信事件。
package platformnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultScope       = "notification.ingest"
	defaultHTTPTimeout = 10 * time.Second
	maxResponseBytes   = 64 << 10
)

// Config 是平台站内信发布配置；凭据缺失时 Enabled 为 false。
type Config struct {
	Endpoint     string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
	HTTPTimeout  time.Duration
}

// ConfigFromEnv 从环境变量读取通知发布配置；不完整的配置返回零值配置和 nil，
// 让尚未开通通知的环境继续运行。URL 非法或 scope 不是 notification.ingest 时返回错误。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("notification config requires env getter")
	}
	config := Config{
		Endpoint:     strings.TrimSpace(getenv("PLATFORM_NOTIFICATION_URL")),
		TokenURL:     strings.TrimSpace(getenv("PLATFORM_NOTIFICATION_TOKEN_URL")),
		ClientID:     strings.TrimSpace(getenv("PLATFORM_NOTIFICATION_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(getenv("PLATFORM_NOTIFICATION_CLIENT_SECRET")),
		Scope:        strings.TrimSpace(getenv("PLATFORM_NOTIFICATION_SCOPE")),
		HTTPTimeout:  defaultHTTPTimeout,
	}
	if config.Endpoint == "" || config.TokenURL == "" || config.ClientID == "" || config.ClientSecret == "" {
		return Config{}, nil
	}
	for name, raw := range map[string]string{"PLATFORM_NOTIFICATION_URL": config.Endpoint, "PLATFORM_NOTIFICATION_TOKEN_URL": config.TokenURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
			return Config{}, fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
	}
	if config.Scope == "" {
		config.Scope = defaultScope
	}
	if config.Scope != defaultScope {
		return Config{}, fmt.Errorf("PLATFORM_NOTIFICATION_SCOPE must be %s", defaultScope)
	}
	return config, nil
}

// Enabled 表示通知发布凭据是否完整。
func (config Config) Enabled() bool {
	return config.Endpoint != "" && config.TokenURL != "" && config.ClientID != "" && config.ClientSecret != ""
}

// Event 是一条平台站内信事件；Tenant 由服务凭据决定，不接受调用方指定。
type Event struct {
	EventID        string
	EventType      string
	Scope          string
	Priority       string
	Title          string
	Content        string
	ReferenceType  string
	ReferenceID    string
	Recipients     []string
	OccurredAt     time.Time
	IdempotencyKey string
}

// Publisher 是通知发布接口，便于 Worker 注入和测试。
type Publisher interface {
	Publish(context.Context, Event) error
}

// New 构造发布器；配置不完整时返回 nil。
func New(config Config) Publisher {
	if !config.Enabled() {
		return nil
	}
	if config.Scope == "" {
		config.Scope = defaultScope
	}
	if config.HTTPTimeout <= 0 {
		config.HTTPTimeout = defaultHTTPTimeout
	}
	return &publisher{config: config, client: &http.Client{Timeout: config.HTTPTimeout}}
}

type publisher struct {
	config Config
	client *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (p *publisher) Publish(ctx context.Context, event Event) error {
	recipients := uniqueRecipients(event.Recipients)
	if len(recipients) == 0 {
		return nil
	}
	if strings.TrimSpace(event.Title) == "" {
		return errors.New("notification title is required")
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	idempotencyKey := strings.TrimSpace(event.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = event.EventID
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return errors.New("notification idempotency key is required")
	}
	payload := struct {
		EventID           string    `json:"event_id"`
		EventType         string    `json:"event_type"`
		NotificationScope string    `json:"notification_scope"`
		Priority          string    `json:"priority"`
		Title             string    `json:"title"`
		Content           string    `json:"content"`
		ReferenceType     string    `json:"reference_type"`
		ReferenceID       string    `json:"reference_id"`
		IdempotencyKey    string    `json:"idempotency_key"`
		Recipients        []string  `json:"recipient_user_ids"`
		OccurredAt        time.Time `json:"occurred_at"`
	}{
		EventID: strings.TrimSpace(event.EventID), EventType: strings.TrimSpace(event.EventType),
		NotificationScope: firstNonBlank(event.Scope, "CROSS_SYSTEM"),
		Priority:          firstNonBlank(strings.ToUpper(event.Priority), "NORMAL"),
		Title:             strings.TrimSpace(event.Title), Content: strings.TrimSpace(event.Content),
		ReferenceType: strings.TrimSpace(event.ReferenceType), ReferenceID: strings.TrimSpace(event.ReferenceID),
		IdempotencyKey: idempotencyKey, Recipients: recipients, OccurredAt: occurredAt.UTC(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notification event: %w", err)
	}
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish notification: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("platform notification ingest returned %d", response.StatusCode)
	}
	return nil
}

func (p *publisher) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.token != "" && time.Now().Before(p.expiry) {
		token := p.token
		p.mu.Unlock()
		return token, nil
	}
	p.mu.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {p.config.Scope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(p.config.ClientID, p.config.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request notification token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("notification token returned %d", response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("notification token response is invalid")
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Minute
	}
	p.mu.Lock()
	p.token = payload.AccessToken
	// 提前 30 秒过期，避免临界点重复投递。
	p.expiry = time.Now().Add(lifetime - 30*time.Second)
	if lifetime < time.Minute {
		p.expiry = time.Now().Add(lifetime / 2)
	}
	p.mu.Unlock()
	return payload.AccessToken, nil
}

func uniqueRecipients(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || len([]rune(value)) > 26 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
