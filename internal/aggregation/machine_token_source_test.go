package aggregation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMachineTokenRequestsOnePlatformScopeWithBasicAuthentication(t *testing.T) {
	const clientID = "data_analysis-prod-contract-dashboard"
	token := compactTestJWT(`{"iss":"basic-platform","aud":"basic-platform-application","token_use":"application","scope":["dashboard.contract.read"],"azp":"data_analysis-prod-contract-dashboard","client_id":"data_analysis-prod-contract-dashboard"}`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		gotClientID, gotSecret, ok := request.BasicAuth()
		if !ok || gotClientID != clientID || gotSecret != "secret" {
			t.Errorf("unexpected Basic authentication")
		}
		if request.Form.Get("grant_type") != "client_credentials" || request.Form.Get("scope") != "dashboard.contract.read" || request.Form.Get("audience") != "" {
			t.Errorf("unexpected token request: %v", request.Form)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": token, "expires_in": 900})
	}))
	defer server.Close()
	runner := NewAPISyncRunner(nil, APISyncOptions{MachineTokenURL: server.URL, MachineTokenIssuer: "basic-platform", MachineTokenAudience: "basic-platform-application", HTTPTimeout: time.Second})
	got, err := runner.machineToken(context.Background(), MachineCredential{ClientID: clientID, ClientSecret: "secret", Scope: "dashboard.contract.read"})
	if err != nil {
		t.Fatal(err)
	}
	if got != token {
		t.Fatalf("token = %q", got)
	}
}
