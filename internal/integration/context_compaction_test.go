package integration

import (
	"context"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestContextCompactionLargeSessionPreservesActiveProtocolAndReloadsSummary(t *testing.T) {
	dbPath := t.TempDir() + "/agent.db"
	store, err := sqlite.Initialize(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	projector, err := adkagent.NewProjector(adkagent.CompactionConfig{Enabled: true, MaxHistoryChars: 900, RecentTurns: 3, SummaryEnabled: true, SummaryMaxChars: 200})
	if err != nil {
		t.Fatal(err)
	}
	projector.SetSummaryStore(store)
	contents := make([]domain.Content, 0, 663)
	for range 331 {
		contents = append(contents,
			domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "user turn"}}},
			domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "assistant turn"}}})
	}
	contents = append(contents, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "active"}}})
	result, err := projector.Project(
		context.Background(),
		domain.CompactionRequest{Contents: contents, ConversationKey: "conversation-1", SessionRevision: 663, SystemInstructionChars: 12, ToolChars: 34},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.HistoryCharsAfter > 900 || result.Diagnostics.RecentTurnsRetained != 3 || result.Diagnostics.ConversationKey != "conversation-1" ||
		result.Diagnostics.SessionRevision != 663 ||
		result.Diagnostics.SystemInstructionChars != 12 ||
		result.Diagnostics.ToolChars != 34 {
		t.Fatalf("bounded projection diagnostics = %#v", result.Diagnostics)
	}

	text := "The user stated that a durable summary exists."
	if _, err := store.CommitSummary(
		context.Background(),
		port.SummaryCommit{SessionIdentity: "adk:conversation-1", Summary: port.ConversationSummary{Text: text, CoveredThroughOrdinal: 1, SourceDigest: "digest", PromptVersion: "v1"}},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Initialize(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projector.SetSummaryStore(store)
	reloaded, err := store.LatestSummary(context.Background(), "adk:conversation-1")
	if err != nil || reloaded.SanitizedText != text {
		t.Fatalf("summary reload = %#v, %v", reloaded, err)
	}
}
