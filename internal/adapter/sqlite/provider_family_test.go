package sqlite

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestRootSessionProviderFamilies(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	service := NewAdkSessionService(store)

	families, err := service.RootSessionProviderFamilies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 0 {
		t.Fatalf("empty store should report no sessions, got %v", families)
	}

	if _, err := service.Create(ctx, &session.CreateRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:legacy",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, &session.CreateRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:cli",
		State: map[string]any{domain.ProviderFamilyStateKey: domain.ProviderFamilyAgentCLI},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, &session.CreateRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:openai",
		State: map[string]any{domain.ProviderFamilyStateKey: domain.ProviderFamilyOpenAICompatible},
	}); err != nil {
		t.Fatal(err)
	}

	families, err = service.RootSessionProviderFamilies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"adk:legacy": domain.ProviderFamilyOpenAICompatible,
		"adk:cli":    domain.ProviderFamilyAgentCLI,
		"adk:openai": domain.ProviderFamilyOpenAICompatible,
	}
	if len(families) != len(want) {
		t.Fatalf("families = %v, want %v", families, want)
	}
	for id, family := range want {
		if families[id] != family {
			t.Errorf("session %q family = %q, want %q", id, families[id], family)
		}
	}
}
