package aggregation

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func TestValidateMachineTokenContractAcceptsRunningKeycloakClaims(t *testing.T) {
	raw := compactTestJWT(`{"iss":"http://localhost:18090/realms/basic-platform","aud":["basic-platform-application","account"],"typ":"Bearer","azp":"data_analysis-dev-machine","client_id":"data_analysis-dev-machine"}`)
	err := validateMachineTokenContract(raw, APISyncOptions{
		MachineTokenIssuer:   "http://localhost:18090/realms/basic-platform",
		MachineTokenAudience: "basic-platform-application",
		MachineClientID:      "data_analysis-dev-machine",
	})
	if err != nil {
		t.Fatalf("running Keycloak token contract rejected: %v", err)
	}
}

func TestValidateMachineTokenContractRejectsIssuerAudienceAndClientMismatch(t *testing.T) {
	raw := compactTestJWT(`{"iss":"http://localhost:18090/realms/basic-platform","aud":["basic-platform-application","account"],"typ":"Bearer","azp":"data_analysis-dev-machine","client_id":"data_analysis-dev-machine"}`)
	cases := []APISyncOptions{
		{MachineTokenIssuer: "http://wrong/realms/basic-platform", MachineTokenAudience: "basic-platform-application", MachineClientID: "data_analysis-dev-machine"},
		{MachineTokenIssuer: "http://localhost:18090/realms/basic-platform", MachineTokenAudience: "wrong-audience", MachineClientID: "data_analysis-dev-machine"},
		{MachineTokenIssuer: "http://localhost:18090/realms/basic-platform", MachineTokenAudience: "basic-platform-application", MachineClientID: "another-client"},
	}
	for index, options := range cases {
		if err := validateMachineTokenContract(raw, options); err == nil {
			t.Fatalf("case %d: expected mismatch error", index)
		}
	}
}

func compactTestJWT(payload string) string {
	return fmt.Sprintf("%s.%s.signature", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)), base64.RawURLEncoding.EncodeToString([]byte(payload)))
}
