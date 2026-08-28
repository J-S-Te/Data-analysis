package oidc

import "testing"

func TestValidateCatalogWindowAcceptsOnlyNOrNMinusOne(t *testing.T) {
	value := AuthorizationContext{CatalogVersion: "3", CompatibleCatalogVersions: []string{"3", "2"}, RoleConfigHash: "hash-3", CompatibleRoleConfigHashes: []string{"hash-3", "hash-2"}}
	if err := validateCatalogWindow(value, "2", "hash-2"); err != nil {
		t.Fatalf("N-1 catalog rejected: %v", err)
	}
	if err := validateCatalogWindow(value, "1", "hash-1"); err == nil {
		t.Fatal("catalog older than N-1 accepted")
	}
	value.CompatibleRoleConfigHashes = nil
	if err := validateCatalogWindow(value, "2", "hash-2"); err == nil {
		t.Fatal("partial compatibility response accepted")
	}
}
