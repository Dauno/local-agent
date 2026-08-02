package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestStandardProgressLifecycleAndWaitingLookup(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	operation := domain.ProgressOperation{
		ID: "progress-1", ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001",
		ChannelID: "D12345678", ThreadTS: "1700000000.000001", State: domain.ProgressWorking,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateProgress(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProgressPublished(ctx, operation.ID, "1700000001.000001"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProgressState(ctx, operation.ID, domain.ProgressWaitingConfirmation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.FindWaitingProgress(ctx, operation.ConversationKey)
	if err != nil || waiting == nil || waiting.MessageTS != "1700000001.000001" {
		t.Fatalf("waiting=%#v err=%v", waiting, err)
	}
	recoverable, err := store.ListRecoverableProgress(ctx)
	if err != nil || len(recoverable) != 1 {
		t.Fatalf("recoverable=%#v err=%v", recoverable, err)
	}
	if err := store.SetProgressState(ctx, operation.ID, domain.ProgressCleared, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	recoverable, err = store.ListRecoverableProgress(ctx)
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("terminal recoverable=%#v err=%v", recoverable, err)
	}
}

func TestSuggestedPromptsAreClaimedOncePerWorkspaceUser(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	id, claimed, err := store.ClaimSuggestedPrompts(ctx, "T12345678", "U12345678", key, now)
	if err != nil || !claimed || id == "" {
		t.Fatalf("id=%q claimed=%v err=%v", id, claimed, err)
	}
	if _, claimed, err := store.ClaimSuggestedPrompts(ctx, "T12345678", "U12345678", key, now); err != nil || claimed {
		t.Fatalf("second claim=%v err=%v", claimed, err)
	}
	if err := store.MarkSuggestedPromptsPublished(ctx, id, "1700000001.000001", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingClaimSurvivesRestartAndRecoversAfterLease(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	first := time.Unix(1700000000, 0).UTC()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")

	claim, state, err := store.ClaimOnboarding(ctx, "T12345678", "U12345678", key, first)
	if err != nil || state != port.OnboardingClaimed || claim.ClaimToken == "" {
		t.Fatalf("first claim=%#v state=%q err=%v", claim, state, err)
	}
	if _, state, err := store.ClaimOnboarding(ctx, "T12345678", "U12345678", key, first.Add(time.Second)); err != nil || state != port.OnboardingInFlight {
		t.Fatalf("active claim state=%q err=%v", state, err)
	}

	newKey := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000002.000001")
	retry, state, err := store.ClaimOnboarding(ctx, "T12345678", "U12345678", newKey, first.Add(onboardingClaimLease+time.Second))
	if err != nil || state != port.OnboardingClaimed || retry.ClaimToken == claim.ClaimToken {
		t.Fatalf("recovered claim=%#v state=%q err=%v", retry, state, err)
	}
	if retry.ConversationKey != key {
		t.Fatalf("recovered conversation=%q, want original %q", retry.ConversationKey, key)
	}
	if err := store.MarkOnboardingPublished(ctx, claim, "1700000001.000001", first.Add(onboardingClaimLease+2*time.Second)); err == nil {
		t.Fatal("stale onboarding claim was accepted")
	}
	if err := store.MarkOnboardingPublished(ctx, retry, "1700000001.000001", first.Add(onboardingClaimLease+2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, state, err := store.ClaimOnboarding(ctx, "T12345678", "U12345678", key, first.Add(onboardingClaimLease+3*time.Second)); err != nil || state != port.OnboardingAlreadyPublished {
		t.Fatalf("published state=%q err=%v", state, err)
	}
}

func TestOnboardingDoesNotCompeteWithAnExistingSuggestedPromptClaim(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	if _, claimed, err := store.ClaimSuggestedPrompts(ctx, "T12345678", "U12345678", key, now); err != nil || !claimed {
		t.Fatalf("suggested prompt claim=%v err=%v", claimed, err)
	}
	_, state, err := store.ClaimOnboarding(ctx, "T12345678", "U12345678", key, now)
	if err != nil || state != port.OnboardingUnavailable {
		t.Fatalf("onboarding state=%q err=%v", state, err)
	}
}

func TestBuilderLauncherClaimRecoversAndUsesCompareAndSwapPublication(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")

	claim, state, err := store.ClaimBuilderLauncher(ctx, "launcher-1", key, now)
	if err != nil || state != port.BuilderLauncherClaimed || claim.ClaimToken == "" {
		t.Fatalf("first claim=%#v state=%q err=%v", claim, state, err)
	}
	if _, state, err := store.ClaimBuilderLauncher(ctx, "launcher-1", key, now.Add(time.Second)); err != nil || state != port.BuilderLauncherInFlight {
		t.Fatalf("active claim state=%q err=%v", state, err)
	}
	retry, state, err := store.ClaimBuilderLauncher(ctx, "launcher-1", key, now.Add(onboardingClaimLease+time.Second))
	if err != nil || state != port.BuilderLauncherClaimed || retry.ClaimToken == claim.ClaimToken {
		t.Fatalf("retry claim=%#v state=%q err=%v", retry, state, err)
	}
	if err := store.MarkBuilderLauncherPublished(ctx, claim, "1700000001.000001", now); err == nil {
		t.Fatal("stale builder launcher claim was accepted")
	}
	if err := store.MarkBuilderLauncherPublished(ctx, retry, "1700000001.000001", now); err != nil {
		t.Fatal(err)
	}
	if _, state, err := store.ClaimBuilderLauncher(ctx, "launcher-1", key, now.Add(time.Hour)); err != nil || state != port.BuilderLauncherAlreadyPublished {
		t.Fatalf("published state=%q err=%v", state, err)
	}
}

func TestIncrementalOperationPersistsOnlyIdentitySequenceAndDigest(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	operation := domain.IncrementalOperation{
		ID: "incremental-1", ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001",
		ChannelID: "D12345678", ThreadTS: "1700000000.000001", RendererVersion: "standard_incremental_v1",
		Status: domain.IncrementalPrepared, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PrepareIncremental(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkIncrementalCreated(ctx, operation.ID, "1700000001.000001", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceIncremental(ctx, operation.ID, domain.IncrementalUpdating, 2, "digest-only", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	unfinished, err := store.ListUnfinishedIncremental(ctx)
	if err != nil || len(unfinished) != 1 || unfinished[0].MessageTS != "1700000001.000001" || unfinished[0].Sequence != 2 || unfinished[0].PrefixDigest != "digest-only" {
		t.Fatalf("unfinished=%#v err=%v", unfinished, err)
	}
	if err := store.AdvanceIncremental(ctx, operation.ID, domain.IncrementalFinalized, 3, "final-digest", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	unfinished, err = store.ListUnfinishedIncremental(ctx)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("finalized unfinished=%#v err=%v", unfinished, err)
	}
}
