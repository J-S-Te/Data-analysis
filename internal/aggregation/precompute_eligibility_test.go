package aggregation

import "testing"

func TestPrecomputeEligibilityIsolatesUnrelatedSourceFailures(t *testing.T) {
	sources := map[string]string{"contract_management": "c", "customer_and_opportunity": "r", "project_management": "p"}
	signing, conversion := precomputeEligibility(sources, map[string]bool{"contract_management": true})
	if !signing || conversion {
		t.Fatalf("contract-only success signing=%v conversion=%v, want signing only", signing, conversion)
	}
	signing, conversion = precomputeEligibility(sources, map[string]bool{"contract_management": true, "customer_and_opportunity": true})
	if !signing || !conversion {
		t.Fatalf("both sources signing=%v conversion=%v, want both", signing, conversion)
	}
	signing, conversion = precomputeEligibility(sources, map[string]bool{"customer_and_opportunity": true})
	if signing || conversion {
		t.Fatalf("contract failure signing=%v conversion=%v, want none", signing, conversion)
	}
}
