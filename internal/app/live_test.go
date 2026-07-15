package app

import (
	"context"
	"strings"
	"testing"
)

func TestCheckAuthenticationRejectsUnknownShimIdentity(t *testing.T) {
	checker := cliProviderChecker{}
	for _, name := range []string{"", "unknown-cli", "opencode;rm -rf /"} {
		_, err := checker.CheckAuthentication(context.Background(), nil, name)
		if err == nil {
			t.Fatalf("shim identity %q must be rejected", name)
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unexpected error for %q: %v", name, err)
		}
	}
}
