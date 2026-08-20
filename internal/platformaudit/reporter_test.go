package platformaudit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReporterObtainsAuditScopeAndPostsEvent(t *testing.T) {
	var tokenCalls, auditCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			tokenCalls++
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "audit.ingest" {
				t.Fatalf("unexpected token request: %v", r.Form)
			}
			user, secret, ok := r.BasicAuth()
			if !ok || user != "audit-client" || secret != "audit-secret" {
				t.Fatal("missing client credentials")
			}
			_, _ = w.Write([]byte(`{"access_token":"token-1","token_type":"Bearer","scope":"audit.ingest","expires_in":3600}`))
		case "/api/v1/audit/events":
			auditCalls++
			if r.Header.Get("Authorization") != "Bearer token-1" {
				t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["application_code"] != "data_analysis" || payload["actor_id"] != "user-1" || payload["result"] != "SUCCESS" {
				t.Fatalf("unexpected payload: %#v", payload)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	reporter := NewReporter(server.URL, "audit-client", "audit-secret", "data_analysis", "dev")
	if err := reporter.Report(t.Context(), Event{ActorID: "user-1", Action: "DATA_ANALYSIS:POST:alerts", ResourceType: "ALERT", Result: "SUCCESS", ReasonCode: "200"}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(t.Context(), Event{ActorID: "user-1", Action: "DATA_ANALYSIS:POST:alerts", ResourceType: "ALERT", Result: "SUCCESS", ReasonCode: "200"}); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || auditCalls != 2 {
		t.Fatalf("token calls %d, audit calls %d", tokenCalls, auditCalls)
	}
}

func TestNewReporterRequiresCompleteConfiguration(t *testing.T) {
	if NewReporter("", "id", "secret", "data_analysis", "dev") != nil {
		t.Fatal("expected nil reporter for incomplete config")
	}
}
