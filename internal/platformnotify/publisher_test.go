package platformnotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigFromEnvDisabledWhenIncomplete(t *testing.T) {
	t.Setenv("PLATFORM_NOTIFICATION_URL", "http://example/events")
	t.Setenv("PLATFORM_NOTIFICATION_CLIENT_ID", "client")
	config, err := ConfigFromEnv(func(key string) string {
		return map[string]string{
			"PLATFORM_NOTIFICATION_URL":       "http://example/events",
			"PLATFORM_NOTIFICATION_CLIENT_ID": "client",
		}[key]
	})
	if err != nil || config.Enabled() {
		t.Fatalf("config = %+v, err = %v; want disabled", config, err)
	}
	if _, err := ConfigFromEnv(func(key string) string {
		return map[string]string{
			"PLATFORM_NOTIFICATION_URL":           "http://example/events",
			"PLATFORM_NOTIFICATION_TOKEN_URL":     "http://example/token",
			"PLATFORM_NOTIFICATION_CLIENT_ID":     "client",
			"PLATFORM_NOTIFICATION_CLIENT_SECRET": "secret",
			"PLATFORM_NOTIFICATION_SCOPE":         "audit.ingest",
		}[key]
	}); err == nil {
		t.Fatal("non notification scope accepted")
	}
}

func TestPublisherRequestsTokenAndPostsRecipients(t *testing.T) {
	var tokenCalls, eventCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenCalls, 1)
			if user, password, ok := request.BasicAuth(); !ok || user != "client" || password != "secret" {
				t.Fatalf("token auth = %q/%q ok=%v", user, password, ok)
			}
			if err := request.ParseForm(); err != nil || request.Form.Get("scope") != "notification.ingest" {
				t.Fatalf("token form = %v err=%v", request.Form, err)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "token", "expires_in": 300})
		case "/events":
			atomic.AddInt32(&eventCalls, 1)
			if request.Header.Get("Authorization") != "Bearer token" {
				t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
			}
			var body struct {
				EventID    string   `json:"event_id"`
				Recipients []string `json:"recipient_user_ids"`
				Scope      string   `json:"notification_scope"`
				Priority   string   `json:"priority"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.EventID != "evt-1" || len(body.Recipients) != 2 || body.Scope != "CROSS_SYSTEM" || body.Priority != "HIGH" {
				t.Fatalf("event body = %+v", body)
			}
			writer.WriteHeader(http.StatusCreated)
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	publisher := New(Config{Endpoint: server.URL + "/events", TokenURL: server.URL + "/token", ClientID: "client", ClientSecret: "secret", HTTPTimeout: time.Second})
	if publisher == nil {
		t.Fatal("publisher is nil")
	}
	err := publisher.Publish(context.Background(), Event{
		EventID: "evt-1", EventType: "ALERT", Priority: "high", Title: "到期提醒",
		Recipients: []string{"user-1", " user-2 ", "user-1", ""}, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || eventCalls != 1 {
		t.Fatalf("calls token=%d event=%d", tokenCalls, eventCalls)
	}
}

func TestPublisherSkipsEmptyRecipients(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&calls, 1) }))
	defer server.Close()
	publisher := New(Config{Endpoint: server.URL, TokenURL: server.URL, ClientID: "c", ClientSecret: "s", HTTPTimeout: time.Second})
	if err := publisher.Publish(context.Background(), Event{EventID: "evt", Title: "x", Recipients: nil}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestPublisherRejectsNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			_, _ = io.WriteString(writer, `{"access_token":"token","expires_in":300}`)
			return
		}
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	publisher := New(Config{Endpoint: server.URL + "/events", TokenURL: server.URL + "/token", ClientID: "c", ClientSecret: "s", HTTPTimeout: time.Second})
	if err := publisher.Publish(context.Background(), Event{EventID: "evt", Title: "x", Recipients: []string{"user-1"}}); err == nil {
		t.Fatal("expected ingest error")
	}
}
