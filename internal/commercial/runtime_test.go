package commercial

import (
	"context"
	"testing"
)

func TestControlledStartupDoesNotFallbackWhenConfigurationMissing(t *testing.T) {
	t.Setenv("COMMERCIAL_LICENSE_ENABLED", "true")
	t.Setenv("COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if check, err := Start(ctx); err == nil || check != nil {
		t.Fatal("controlled startup failed open")
	}
}

func TestDisabledStartupPreservesUnenrolledDeployment(t *testing.T) {
	t.Setenv("COMMERCIAL_LICENSE_ENABLED", "false")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	check, err := Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := check.Allow(ctx, MutateBusiness); err != nil {
		t.Fatal(err)
	}
}
