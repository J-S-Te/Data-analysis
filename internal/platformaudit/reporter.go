// Package platformaudit reports concise API write outcomes to the platform audit service.
package platformaudit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ActorID, ActorName, Action, ResourceType, ResourceID string
	RequestID, CorrelationID, Result, ReasonCode         string
	UserLoginIP                                          string
}

type Reporter interface {
	Report(context.Context, Event) error
}

type client struct {
	baseURL, clientID, clientSecret, applicationCode, environmentCode string
	httpClient                                                        *http.Client
	mu                                                                sync.Mutex
	token                                                             string
	expiresAt                                                         time.Time
}

func NewReporter(baseURL, clientID, clientSecret, applicationCode, environmentCode string) Reporter {
	if strings.TrimSpace(baseURL) == "" || clientID == "" || clientSecret == "" || applicationCode == "" || environmentCode == "" {
		return nil
	}
	return &client{baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, clientSecret: clientSecret, applicationCode: applicationCode, environmentCode: environmentCode, httpClient: &http.Client{Timeout: 5 * time.Second}}
}

func (c *client) Report(ctx context.Context, event Event) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	requestID := event.RequestID
	if requestID == "" {
		requestID = randomHex(16)
	}
	correlationID := event.CorrelationID
	if correlationID == "" {
		correlationID = requestID
	}
	traceID, parentID, err := traceIDs()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"event_id": randomHex(16), "occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"application_code": c.applicationCode, "environment_code": c.environmentCode,
		"actor_type": actorType(event.ActorID), "action": event.Action, "resource_type": event.ResourceType,
		"request_id": requestID, "trace_id": traceID, "correlation_id": correlationID,
		"result": event.Result, "risk_level": "LOW", "metadata": map[string]string{"http_status": event.ReasonCode},
	}
	if event.ActorID != "" {
		payload["actor_id"] = event.ActorID
	}
	if event.ActorName != "" {
		payload["actor_name"] = event.ActorName
	}
	if event.ResourceID != "" {
		payload["resource_id"] = event.ResourceID
	}
	if event.ReasonCode != "" {
		payload["reason_code"] = event.ReasonCode
	}
	if event.UserLoginIP != "" {
		payload["user_login_ip"] = event.UserLoginIP
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/audit/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentID+"-01")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("platform audit returned %d", resp.StatusCode)
	}
	return nil
}

func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > 30*time.Second {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"audit.ingest"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("platform token returned %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.AccessToken == "" || !strings.EqualFold(result.TokenType, "bearer") || !hasScope(result.Scope, "audit.ingest") || result.ExpiresIn <= 0 {
		return "", fmt.Errorf("platform token missing audit.ingest scope")
	}
	c.token, c.expiresAt = result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn)*time.Second)
	return c.token, nil
}

func hasScope(scopes, wanted string) bool {
	for _, scope := range strings.Fields(scopes) {
		if scope == wanted {
			return true
		}
	}
	return false
}
func actorType(actorID string) string {
	if actorID == "" {
		return "SYSTEM"
	}
	return "USER"
}
func randomHex(size int) string {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}
func traceIDs() (string, string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(raw[:16]), hex.EncodeToString(raw[16:]), nil
}
