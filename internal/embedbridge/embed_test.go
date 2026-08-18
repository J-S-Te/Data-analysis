package embedbridge

import "testing"

func TestRewriteEmbedDocumentUsesTokenResourcePrefix(t *testing.T) {
	input := []byte(`<html><head><base href="/mb/"><meta name="base-href" content="/mb/"><meta name="uri" content="/mb/embed/dashboard/signed"></head></html>`)
	got := string(rewriteEmbedDocument(input, "/data_analysis/api/v1/embed-proxy/raw-token/", "/mb"))
	wantParts := []string{
		`<base href="/data_analysis/api/v1/embed-proxy/raw-token/">`,
		`<meta name="base-href" content="/data_analysis/api/v1/embed-proxy/raw-token/">`,
		`<meta name="uri" content="/embed/dashboard/signed">`,
	}
	for _, want := range wantParts {
		if !contains(got, want) {
			t.Fatalf("rewritten document missing %q: %s", want, got)
		}
	}
}

func TestResourcePrefixIncludesConfiguredPathPrefix(t *testing.T) {
	bridge := &Bridge{options: Options{PathPrefix: "/data_analysis/"}}
	if got, want := bridge.resourcePrefix("token"), "/data_analysis/api/v1/embed-proxy/token/"; got != want {
		t.Fatalf("resource prefix = %q, want %q", got, want)
	}
}

func TestWithoutFrameAncestorsPreservesOtherCSPDirectives(t *testing.T) {
	input := "default-src 'none'; frame-ancestors 'none'; script-src 'self';"
	if got, want := withoutFrameAncestors(input), "default-src 'none'; script-src 'self';"; got != want {
		t.Fatalf("CSP = %q, want %q", got, want)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
