package aggregation

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func TestValidateMachineTokenContractAcceptsPlatformApplicationClaims(t *testing.T) {
	raw := compactTestJWT(`{"iss":"basic-platform","aud":"basic-platform-application","token_use":"application","scope":["dashboard.contract.read"],"azp":"data_analysis-dev-contract-dashboard","client_id":"data_analysis-dev-contract-dashboard"}`)
	credential := MachineCredential{ClientID: "data_analysis-dev-contract-dashboard", Scope: "dashboard.contract.read"}
	err := validateMachineTokenContract(raw, APISyncOptions{
		MachineTokenIssuer:   "basic-platform",
		MachineTokenAudience: "basic-platform-application",
	}, credential)
	if err != nil {
		t.Fatalf("platform application token contract rejected: %v", err)
	}
}

func TestValidateMachineTokenContractRejectsIssuerAudienceAndClientMismatch(t *testing.T) {
	raw := compactTestJWT(`{"iss":"basic-platform","aud":"basic-platform-application","token_use":"application","scope":["dashboard.contract.read"],"azp":"data_analysis-dev-contract-dashboard","client_id":"data_analysis-dev-contract-dashboard"}`)
	cases := []APISyncOptions{
		{MachineTokenIssuer: "wrong-platform", MachineTokenAudience: "basic-platform-application"},
		{MachineTokenIssuer: "basic-platform", MachineTokenAudience: "wrong-audience"},
		{MachineTokenIssuer: "basic-platform", MachineTokenAudience: "basic-platform-application"},
	}
	for index, options := range cases {
		credential := MachineCredential{ClientID: "data_analysis-dev-contract-dashboard", Scope: "dashboard.contract.read"}
		if index == 2 {
			credential.ClientID = "another-client"
		}
		if err := validateMachineTokenContract(raw, options, credential); err == nil {
			t.Fatalf("case %d: expected mismatch error", index)
		}
	}
}

func TestValidateMachineTokenContractRejectsScopeReuse(t *testing.T) {
	raw := compactTestJWT(`{"iss":"basic-platform","aud":"basic-platform-application","token_use":"application","scope":["dashboard.contract.read","dashboard.project.read"],"azp":"data_analysis-dev-contract-dashboard","client_id":"data_analysis-dev-contract-dashboard"}`)
	err := validateMachineTokenContract(raw, APISyncOptions{MachineTokenIssuer: "basic-platform", MachineTokenAudience: "basic-platform-application"}, MachineCredential{ClientID: "data_analysis-dev-contract-dashboard", Scope: "dashboard.contract.read"})
	if err == nil {
		t.Fatal("multi-scope machine token must be rejected")
	}
}

func compactTestJWT(payload string) string {
	return fmt.Sprintf("%s.%s.signature", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)), base64.RawURLEncoding.EncodeToString([]byte(payload)))
}
