package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestClaimDedupeIsAtomicAndExpiredKeysCanBeReclaimed(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	claimed, err := store.ClaimDedupe(ctx, []string{"event:E1", "message:M1"}, now, now.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("first ClaimDedupe = (%v, %v), want (true, nil)", claimed, err)
	}
	claimed, err = store.ClaimDedupe(ctx, []string{"event:E1", "message:M1"}, now.Add(time.Minute), now.Add(2*time.Hour))
	if err != nil || claimed {
		t.Fatalf("duplicate ClaimDedupe = (%v, %v), want (false, nil)", claimed, err)
	}

	claimed, err = store.ClaimDedupe(ctx, []string{"event:fresh", "event:E1"}, now.Add(time.Minute), now.Add(2*time.Hour))
	if err != nil || claimed {
		t.Fatalf("partially conflicting ClaimDedupe = (%v, %v), want (false, nil)", claimed, err)
	}
	claimed, err = store.ClaimDedupe(ctx, []string{"event:fresh"}, now.Add(time.Minute), now.Add(2*time.Hour))
	if err != nil || !claimed {
		t.Fatalf("rolled-back key ClaimDedupe = (%v, %v), want (true, nil)", claimed, err)
	}

	claimed, err = store.ClaimDedupe(ctx, []string{"event:E1", "message:M1"}, now.Add(2*time.Hour), now.Add(3*time.Hour))
	if err != nil || !claimed {
		t.Fatalf("expired ClaimDedupe = (%v, %v), want (true, nil)", claimed, err)
	}

	if err := store.CleanupDedupe(ctx, now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM dedupe_records").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dedupe count after cleanup = %d, want 0", count)
	}
}

func TestClaimDedupeConcurrentAcrossConnectionsHasSingleWinner(t *testing.T) {
	store, path := newTestStore(t)
	second, err := OpenExisting(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})

	const contenders = 24
	start := make(chan struct{})
	errorsByContender := make(chan error, contenders)
	var winners atomic.Int32
	var wait sync.WaitGroup
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	for index := range contenders {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			candidate := store
			if index%2 == 1 {
				candidate = second
			}
			claimed, err := candidate.ClaimDedupe(
				context.Background(),
				[]string{"event:concurrent", "message:concurrent"},
				now,
				now.Add(time.Hour),
			)
			if err != nil {
				errorsByContender <- fmt.Errorf("contender %d: %w", index, err)
				return
			}
			if claimed {
				winners.Add(1)
			}
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsByContender)
	for err := range errorsByContender {
		t.Error(err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent claim winners = %d, want 1", got)
	}
}

func TestMessagesAreIsolatedOrderedAndRetained(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	channel := domain.ConversationMetadata{
		Key:         "slack:T12345678:channel:C12345678:thread:1700000000.000001",
		TeamID:      "T12345678",
		ChannelID:   "C12345678",
		ChannelKind: domain.ChannelPublic,
		RootTS:      "1700000000.000001",
		LastTS:      "1700000003.000004",
	}
	dm := domain.ConversationMetadata{
		Key:         "slack:T12345678:dm:D12345678",
		TeamID:      "T12345678",
		ChannelID:   "D12345678",
		ChannelKind: domain.ChannelDM,
		LastTS:      "1700000001.000002",
	}

	// Insert out of timestamp order to prove reads use logical chronology, not row ID.
	messages := []domain.Message{
		{Role: domain.RoleUser, Content: "old", UserID: "U12345678", ExternalTS: "1", CreatedAt: base.Add(time.Minute)},
		{Role: domain.RoleUser, Content: "new", UserID: "U12345678", ExternalTS: "3", CreatedAt: base.Add(3 * time.Minute)},
		{Role: domain.RoleAssistant, Content: "middle", ExternalTS: "2", CreatedAt: base.Add(2 * time.Minute)},
	}
	for _, message := range messages {
		if err := store.AppendMessage(ctx, channel, message, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendMessage(ctx, dm, domain.Message{
		Role: domain.RoleUser, Content: "isolated", UserID: "U99999999", CreatedAt: base,
	}, 10); err != nil {
		t.Fatal(err)
	}

	got, err := store.RecentMessages(ctx, channel.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, got, "old", "middle", "new")
	got, err = store.RecentMessages(ctx, channel.Key, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, got, "middle", "new")
	got, err = store.RecentMessages(ctx, dm.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, got, "isolated")

	participated, err := store.HasAssistantMessage(ctx, channel.Key)
	if err != nil || !participated {
		t.Fatalf("channel HasAssistantMessage = (%v, %v), want (true, nil)", participated, err)
	}
	participated, err = store.HasAssistantMessage(ctx, dm.Key)
	if err != nil || participated {
		t.Fatalf("DM HasAssistantMessage = (%v, %v), want (false, nil)", participated, err)
	}

	if err := store.AppendMessage(ctx, channel, domain.Message{
		Role: domain.RoleAssistant, Content: "newest", CreatedAt: base.Add(4 * time.Minute),
	}, 2); err != nil {
		t.Fatal(err)
	}
	got, err = store.RecentMessages(ctx, channel.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, got, "new", "newest")

	if got[0].Role != domain.RoleUser || got[0].UserID != "U12345678" || got[0].ExternalTS != "3" {
		t.Fatalf("message fields were not preserved: %#v", got[0])
	}
	if !got[0].CreatedAt.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("CreatedAt = %v", got[0].CreatedAt)
	}

	var lastTS string
	var updatedNanos int64
	if err := store.db.QueryRowContext(ctx, `
		SELECT last_ts, updated_at FROM conversations WHERE conversation_key = ?`,
		string(channel.Key),
	).Scan(&lastTS, &updatedNanos); err != nil {
		t.Fatal(err)
	}
	if lastTS != channel.LastTS || updatedNanos != base.Add(4*time.Minute).UnixNano() {
		t.Fatalf("conversation recency = (%q, %d)", lastTS, updatedNanos)
	}
}

func TestAppendMessageMetadataConflictRollsBack(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	metadata := domain.ConversationMetadata{
		Key:         "slack:T12345678:channel:C12345678:thread:1700000000.000001",
		TeamID:      "T12345678",
		ChannelID:   "C12345678",
		ChannelKind: domain.ChannelPublic,
		RootTS:      "1700000000.000001",
		LastTS:      "1700000000.000001",
	}
	if err := store.AppendMessage(ctx, metadata, domain.Message{
		Role: domain.RoleUser, Content: "first", UserID: "U12345678", CreatedAt: now,
	}, 10); err != nil {
		t.Fatal(err)
	}

	conflict := metadata
	conflict.ChannelID = "C99999999"
	if err := store.AppendMessage(ctx, conflict, domain.Message{
		Role: domain.RoleAssistant, Content: "must roll back", CreatedAt: now.Add(time.Second),
	}, 10); !errors.Is(err, ErrMetadataConflict) {
		t.Fatalf("AppendMessage conflict error = %v", err)
	}
	got, err := store.RecentMessages(ctx, metadata.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, got, "first")
}

func TestProbeReadWriteLeavesNoRecord(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if err := store.ProbeReadWrite(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `
		SELECT count(*) FROM dedupe_records
		WHERE dedupe_key = '__local_agent_read_write_probe__'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("probe left %d records", count)
	}
}

func assertContents(t *testing.T, messages []domain.Message, want ...string) {
	t.Helper()
	if len(messages) != len(want) {
		t.Fatalf("message count = %d, want %d: %#v", len(messages), len(want), messages)
	}
	for index, content := range want {
		if messages[index].Content != content {
			t.Fatalf("message %d content = %q, want %q", index, messages[index].Content, content)
		}
	}
}
