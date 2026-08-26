package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

// insertKnowledgeCommandTestResult writes a minimal result_records row
// matching knowledgeTestBinding's actor/team/conversation, so a curated
// document created directly through the store can bind to it with
// "result:<id>". Every call gets a distinct result identity via t.Name()
// plus project, so tests in this file never collide.
func insertKnowledgeCommandTestResult(t *testing.T, store *adaptersqlite.Store, project string) (resultID, digest string) {
	t.Helper()
	content := "curated content for " + t.Name() + ":" + project
	sum := sha256.Sum256([]byte(content))
	idSeed := sha256.Sum256([]byte("result-id:" + t.Name() + ":" + project))
	resultID = hex.EncodeToString(idSeed[:])
	digest = hex.EncodeToString(sum[:])
	if _, err := store.DB().ExecContext(context.Background(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', ?, 1, 'artifact', 'result-key', ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', ?, 'conversation', 1, 'available')`,
		resultID, resultID, digest, len(content), project); err != nil {
		t.Fatal(err)
	}
	return resultID, digest
}

type knowledgeTestCoordinator struct {
	mu      sync.Mutex
	held    map[string]bool
	inUse   atomic.Int64
	maxSeen int64
}

func newKnowledgeTestCoordinator() *knowledgeTestCoordinator {
	return &knowledgeTestCoordinator{held: map[string]bool{}}
}

func (c *knowledgeTestCoordinator) TryAcquire(key string) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key] {
		return nil, false
	}
	c.held[key] = true
	now := c.inUse.Add(1)
	if now > c.maxSeen {
		c.maxSeen = now
	}
	return func() {
		c.inUse.Add(-1)
		c.mu.Lock()
		delete(c.held, key)
		c.mu.Unlock()
	}, true
}

func newKnowledgeTestService(t *testing.T, database string) (*knowledge.Service, *adaptersqlite.Store) {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func knowledgeTestBinding(actor string, project string) domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team: "T12345678", Actor: actor,
		Conversation: domain.ConversationKey("slack:T12345678:dm:D12345678"),
		Project:      project,
	}
}

func claimIDFromMessage(t *testing.T, message string) string {
	t.Helper()
	start := strings.Index(message, "`")
	end := strings.Index(message[start+1:], "`")
	if start < 0 || end < 0 {
		t.Fatalf("no claim identity in message %q", message)
	}
	return message[start+1 : start+1+end]
}

func TestKnowledgeCommandReplayIsIdempotentAcrossRestart(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	binding := knowledgeTestBinding("U12345678", "workspace")

	_, created, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatal(err)
	}
	claimID := claimIDFromMessage(t, created)
	_, replayed, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
	if claimIDFromMessage(t, replayed) != claimID {
		t.Fatalf("replay created a second claim: %q vs %q", replayed, created)
	}
	if _, _, err := service.Execute(
		ctx,
		binding,
		"evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-02","scope_kind":"project","scope_id":"workspace"}`,
	); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same event with different content error = %v", err)
	}

	_, corrected, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"correct","claim_id":"%s","value_kind":"string","value_text":"pg-02"}`, claimID))
	if err != nil {
		t.Fatal(err)
	}
	replacementID := strings.TrimSuffix(strings.TrimPrefix(corrected, "Claim `"+claimID+"` corrected by replacement claim `"), "`.")
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"correct","claim_id":"%s","value_kind":"string","value_text":"pg-02"}`, claimID)); err != nil {
		t.Fatalf("correction replay rejected: %v", err)
	}
	_, disputed, err := service.Execute(ctx, binding, "evt-3",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, replacementID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disputed, "revision `2`") {
		t.Fatalf("dispute message = %q", disputed)
	}
	_, disputeReplay, err := service.Execute(ctx, binding, "evt-3",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, replacementID))
	if err != nil {
		t.Fatalf("dispute replay rejected: %v", err)
	}
	if !strings.Contains(disputeReplay, "revision `2`") {
		t.Fatalf("dispute replay advanced the revision: %q", disputeReplay)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(reopened), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, persisted, err := restarted.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatalf("post-restart replay rejected: %v", err)
	}
	if claimIDFromMessage(t, persisted) != claimID {
		t.Fatal("post-restart replay lost claim identity")
	}
}

func TestKnowledgeCommandsIsolateByScope(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-isolation.db"))
	defer func() { _ = store.Close() }()
	projectBinding := knowledgeTestBinding("U12345678", "workspace")

	_, created, err := service.Execute(ctx, projectBinding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01"}`)
	if err != nil {
		t.Fatal(err)
	}
	claimID := claimIDFromMessage(t, created)

	foreignProject := knowledgeTestBinding("U12345678", "elsewhere")
	foreignUser := domain.KnowledgeWriteBinding{
		Team: "T12345678", Actor: "U99999999",
		Conversation: domain.ConversationKey("slack:T12345678:dm:D99999999"),
	}
	for _, binding := range []domain.KnowledgeWriteBinding{foreignProject, foreignUser} {
		for _, action := range []string{
			fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID),
			fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, claimID),
			fmt.Sprintf(`{"action":"archive","claim_id":"%s","expected_revision":1}`, claimID),
			fmt.Sprintf(`{"action":"correct","claim_id":"%s","value_kind":"string","value_text":"x"}`, claimID),
		} {
			if _, _, err := service.Execute(ctx, binding, "evt-9", knowledge.HumanCommandPrefix+action); !errors.Is(err, port.ErrKnowledgeNotFound) {
				t.Fatalf("foreign binding action %q error = %v, want ErrKnowledgeNotFound (indistinguishable from missing)", action, err)
			}
		}
		if _, _, err := service.Execute(ctx, binding, "evt-9",
			knowledge.HumanCommandPrefix+`{"action":"forget","subject":"database","scope_kind":"project","scope_id":"workspace"}`); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
			t.Fatalf("foreign forget error = %v", err)
		}
	}
	_, card, err := service.Execute(ctx, projectBinding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, "database runs_on pg-01") || !strings.Contains(card, "verified") {
		t.Fatalf("card rendering = %q", card)
	}

	// The trusted actor's user scope is readable by its owner even though the
	// claim above lives in project scope: user-scope writes must not leak into
	// project scope and vice versa.
	if _, _, err := service.Execute(ctx, knowledgeTestBinding("U12345678", ""), "evt-3",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID)); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("user-scope binding read of project claim error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestKnowledgeCommandsSerializeConcurrentTransitions(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-concurrency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	coordinator := newKnowledgeTestCoordinator()
	service, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: coordinator,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := knowledgeTestBinding("U12345678", "workspace")
	_, created, err := service.Execute(ctx, binding, "evt-seed",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"cache","predicate":"runs_on","value_kind":"string","value_text":"redis"}`)
	if err != nil {
		t.Fatal(err)
	}
	claimID := claimIDFromMessage(t, created)

	var wg sync.WaitGroup
	var winners, conflicts int64
	for i := range 4 {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			_, _, execErr := service.Execute(ctx, binding, fmt.Sprintf("evt-dispute-%d", seq),
				knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, claimID))
			if execErr == nil {
				atomic.AddInt64(&winners, 1)
				return
			}
			if errors.Is(execErr, port.ErrKnowledgeCASConflict) || errors.Is(execErr, port.ErrKnowledgeBusy) {
				atomic.AddInt64(&conflicts, 1)
				return
			}
			t.Error(execErr)
		}(i)
	}
	wg.Wait()
	if coordinator.maxSeen != 1 {
		t.Fatalf("coordinator observed %d concurrent commands, want 1", coordinator.maxSeen)
	}
	if winners != 1 || conflicts != 3 {
		t.Fatalf("concurrent disputes produced %d winners and %d conflicts, want 1 and 3", winners, conflicts)
	}
	_, card, err := service.Execute(ctx, binding, "evt-inspect",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, "disputed") || !strings.Contains(card, "reason: human inspect") {
		t.Fatalf("card after concurrent disputes = %q", card)
	}
}

func TestKnowledgeForgetBlocksRewriteAndSurvivesRestart(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge-forget.db")
	service, store := newKnowledgeTestService(t, database)
	binding := knowledgeTestBinding("U12345678", "")

	_, created, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"secret-plan","predicate":"is","value_kind":"string","value_text":"sensitive"}`)
	if err != nil {
		t.Fatal(err)
	}
	claimID := claimIDFromMessage(t, created)
	_, forgotten, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+`{"action":"forget","subject":"secret-plan"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(forgotten, "tombstone recorded") {
		t.Fatalf("forget message = %q", forgotten)
	}
	_, replayed, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+`{"action":"forget","subject":"secret-plan"}`)
	if err != nil {
		t.Fatalf("forget replay rejected: %v", err)
	}
	if !strings.Contains(replayed, "already recorded") {
		t.Fatalf("forget replay message = %q", replayed)
	}
	if _, _, err := service.Execute(
		ctx,
		binding,
		"evt-3",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"secret-plan","predicate":"is","value_kind":"string","value_text":"sensitive"}`,
	); !errors.Is(err, domain.ErrKnowledgeTombstoneBlocked) {
		t.Fatalf("rewrite of forgotten subject error = %v", err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-4",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID)); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("inspect of forgotten claim error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(reopened), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Execute(ctx, binding, "evt-5",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"secret-plan","predicate":"is","value_kind":"string","value_text":"back"}`); !errors.Is(err, domain.ErrKnowledgeTombstoneBlocked) {
		t.Fatalf("post-restart rewrite error = %v", err)
	}
}

func TestKnowledgePreferenceCommandsBindToActor(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-preferences.db"))
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "")

	_, created, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"Spanish"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created, "revision `1`") {
		t.Fatalf("preference message = %q", created)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"Spanish"}`); err != nil {
		t.Fatalf("preference replay rejected: %v", err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"French"}`); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("preference replay with different value error = %v", err)
	}
	otherActor := knowledgeTestBinding("U99999999", "")
	otherActor.Conversation = domain.ConversationKey("slack:T12345678:dm:D99999999")
	if _, _, err := service.Execute(ctx, otherActor, "evt-2",
		knowledge.HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"French"}`); err != nil {
		t.Fatalf("other actor preference namespace rejected: %v", err)
	}
	if _, own, err := service.Execute(ctx, otherActor, "evt-2a",
		knowledge.HumanCommandPrefix+`{"action":"inspect","preference_key":"language"}`); err != nil || !strings.Contains(own, "French") {
		t.Fatalf("other actor preference = %q, %v", own, err)
	}
	_, updated, err := service.Execute(ctx, binding, "evt-3",
		knowledge.HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"English","expected_revision":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "revision `2`") {
		t.Fatalf("preference update message = %q", updated)
	}
	_, listed, err := service.Execute(ctx, binding, "evt-4", knowledge.HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, "language = English") {
		t.Fatalf("listing = %q", listed)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-5",
		knowledge.HumanCommandPrefix+`{"action":"archive","preference_key":"language","expected_revision":2}`); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeCommandIdentityIsGlobalAcrossTargets(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-identity.db"))
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "workspace")

	remember := func(subject string) string {
		return knowledge.HumanCommandPrefix + fmt.Sprintf(
			`{"action":"remember","subject":"%s","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`,
			subject,
		)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1", remember("database")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1", remember("database")); err != nil {
		t.Fatalf("identical replay rejected: %v", err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1", remember("cache")); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same event different target error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"forget","subject":"database","scope_kind":"project","scope_id":"workspace"}`); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same event different action error = %v, want ErrKnowledgeCASConflict", err)
	}
	var claims int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_claims WHERE subject = 'database'`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("database claims = %d, %v; want 1", claims, err)
	}
	if _, _, err := service.Execute(ctx, binding, "", remember("api")); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("empty event identity error = %v, want ErrKnowledgeValidation", err)
	}
}

func TestKnowledgeDocumentArchiveCarriesReceiptAcrossRestart(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge-document-archive.db")
	service, store := newKnowledgeTestService(t, database)
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "workspace")

	resultID, digest := insertKnowledgeCommandTestResult(t, store, "workspace")
	document, err := adaptersqlite.NewKnowledgeStore(store).CreateDocument(ctx, domain.KnowledgeDocument{
		Subject: "runbook", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "workspace",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if document.Revision != 1 {
		t.Fatalf("created document revision = %d", document.Revision)
	}
	_, archived, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"archive","document_id":"%s","expected_revision":1}`, document.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archived, "revision `2`") {
		t.Fatalf("archive message = %q", archived)
	}
	_, replay, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"archive","document_id":"%s","expected_revision":1}`, document.ID))
	if err != nil {
		t.Fatalf("archive replay rejected: %v", err)
	}
	if !strings.Contains(replay, "revision `2`") {
		t.Fatalf("archive replay advanced revision: %q", replay)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"archive","document_id":"%s","expected_revision":2}`, document.ID)); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archive by another event error = %v, want ErrKnowledgeCASConflict", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var attributed string
	var revision int
	if err := reopened.DB().QueryRowContext(ctx, `
		SELECT source_ref, revision_number FROM knowledge_document_revisions
		WHERE document_id = ?`, string(document.ID)).Scan(&attributed, &revision); err != nil || attributed != "slack-human:evt-1" || revision != 2 {
		t.Fatalf("document revision attribution = %q rev %d, %v", attributed, revision, err)
	}
}

func TestKnowledgeInspectRediscoveryAndReadableScopes(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge-inspect.db")
	service, store := newKnowledgeTestService(t, database)
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "workspace")

	knowledgeStore := adaptersqlite.NewKnowledgeStore(store)
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01"}`); err != nil {
		t.Fatal(err)
	}
	teamResultID, teamDigest := insertKnowledgeCommandTestResult(t, store, "workspace")
	if _, err := knowledgeStore.CreateDocument(ctx, domain.KnowledgeDocument{
		Subject: "team-runbook", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: teamDigest, ContentHandle: "result:" + teamResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	otherResultID, otherDigest := insertKnowledgeCommandTestResult(t, store, "elsewhere")
	if _, err := knowledgeStore.CreateDocument(ctx, domain.KnowledgeDocument{
		Subject: "other-runbook", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "elsewhere",
		ContentDigest: otherDigest, ContentHandle: "result:" + otherResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(reopened), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, listing, err := restarted.Execute(ctx, binding, "evt-2", knowledge.HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "claim kclaim_") || !strings.Contains(listing, "database runs_on pg-01") {
		t.Fatalf("listing must rediscover claim identities after restart: %q", listing)
	}
	if !strings.Contains(listing, "team-runbook") {
		t.Fatalf("listing must include readable team documents: %q", listing)
	}
	if strings.Contains(listing, "other-runbook") {
		t.Fatalf("listing leaked a foreign-project document: %q", listing)
	}
	if _, _, err := restarted.Execute(ctx, binding, "evt-3",
		knowledge.HumanCommandPrefix+`{"action":"inspect","document_id":"missing"}`); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("missing document inspect error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestKnowledgeForgetLeavesNoPlaintextSubjectAnywhere(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-plaintext.db"))
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "")
	subject := "top-secret-project-name"

	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"x"}`, subject)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatal(err)
	}
	tables := []string{
		"knowledge_claims", "knowledge_claim_revisions", "knowledge_evidence",
		"knowledge_preferences", "knowledge_preference_revisions",
		"knowledge_documents", "knowledge_document_receipts", "knowledge_document_revisions",
		"knowledge_tombstones", "knowledge_command_receipts", "knowledge_projection_outbox",
	}
	for _, table := range tables {
		rows, err := store.DB().QueryContext(ctx, `SELECT * FROM `+table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for _, value := range values {
				if value != nil && strings.Contains(fmt.Sprint(value), subject) {
					t.Fatalf("table %s leaked plaintext subject %q in value %v", table, subject, value)
				}
			}
		}
		_ = rows.Close()
	}
	var tombstoneCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstoneCount); err != nil || tombstoneCount != 1 {
		t.Fatalf("tombstones = %d, %v; want exactly one", tombstoneCount, err)
	}
}

func TestKnowledgeForgetAndInspectHonorConfiguredLimits(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := knowledge.New(knowledge.Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxSubjectRunes: 512}}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := knowledgeTestBinding("U12345678", "workspace")
	subject := strings.Repeat("s", 300)
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"x"}`, subject)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatalf("amplified forget rejected end-to-end: %v", err)
	}
	var tombstoneCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstoneCount); err != nil || tombstoneCount != 1 {
		t.Fatalf("tombstones = %d, %v", tombstoneCount, err)
	}
	if _, _, err := service.Execute(
		ctx,
		binding,
		"evt-3",
		knowledge.HumanCommandPrefix+fmt.Sprintf(
			`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"y"}`,
			subject,
		),
	); !errors.Is(err, domain.ErrKnowledgeTombstoneBlocked) {
		t.Fatalf("rewrite after amplified forget error = %v", err)
	}
}

func TestKnowledgeInspectSubjectSelectorRediscovery(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-subject.db"))
	defer func() { _ = store.Close() }()
	binding := knowledgeTestBinding("U12345678", "workspace")
	if _, _, err := service.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"api","predicate":"runs_on","value_kind":"string","value_text":"pg-01"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"db","predicate":"runs_on","value_kind":"string","value_text":"pg-02"}`); err != nil {
		t.Fatal(err)
	}
	_, listing, err := service.Execute(ctx, binding, "evt-3",
		knowledge.HumanCommandPrefix+`{"action":"inspect","subject":"db"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "db runs_on pg-02") || strings.Contains(listing, "api runs_on pg-01") {
		t.Fatalf("subject-scoped listing = %q", listing)
	}
}

func TestKnowledgeForgetSurvivesRestartWithDefaultLimits(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge-forget-restart.db")
	store, err := adaptersqlite.Initialize(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	amplified, err := knowledge.New(knowledge.Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxSubjectRunes: 512}}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := knowledgeTestBinding("U12345678", "workspace")
	subject := strings.Repeat("s", 300)
	if _, _, err := amplified.Execute(ctx, binding, "evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"x"}`, subject)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(reopened), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatalf("forget after restart under default limits rejected: %v", err)
	}
	var tombstoneCount int
	if err := reopened.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstoneCount); err != nil || tombstoneCount != 1 {
		t.Fatalf("tombstones = %d, %v", tombstoneCount, err)
	}
	if _, _, err := restarted.Execute(
		ctx,
		binding,
		"evt-3",
		knowledge.HumanCommandPrefix+fmt.Sprintf(
			`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"y"}`,
			subject,
		),
	); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("rewrite of forgotten amplified subject error = %v, want rejection under default limits", err)
	}
}

func TestKnowledgeForgetHardScopeValidationAcrossRestart(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge-scope-hard.db")
	store, err := adaptersqlite.Initialize(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	longScope := strings.Repeat("p", 300)
	amplified, err := knowledge.New(knowledge.Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxScopeIDRunes: 512}}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := knowledgeTestBinding("U12345678", longScope)
	if _, _, err := amplified.Execute(
		ctx,
		binding,
		"evt-1",
		knowledge.HumanCommandPrefix+fmt.Sprintf(
			`{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"%s"}`,
			longScope,
		),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(reopened), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Execute(ctx, binding, "evt-2",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"api","scope_kind":"project","scope_id":"%s"}`, longScope)); err != nil {
		t.Fatalf("forget with historically valid scope rejected under default limits: %v", err)
	}
	var tombstoneCount int
	if err := reopened.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstoneCount); err != nil || tombstoneCount != 1 {
		t.Fatalf("tombstones = %d, %v", tombstoneCount, err)
	}
	var claimRows int
	if err := reopened.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM knowledge_claims WHERE subject = 'api' AND scope_kind = 'project' AND scope_id = ?`, longScope).Scan(&claimRows); err != nil || claimRows != 0 {
		t.Fatalf("claim rows after forget = %d, %v; want 0", claimRows, err)
	}
}

func TestKnowledgeRejectedScopeForgetDoesNotConsumeIdentity(t *testing.T) {
	ctx := t.Context()
	service, store := newKnowledgeTestService(t, filepath.Join(t.TempDir(), "knowledge-scope-identity.db"))
	defer func() { _ = store.Close() }()
	oversized := strings.Repeat("p", 600)
	binding := knowledgeTestBinding("U12345678", oversized)
	if _, _, err := service.Execute(ctx, binding, "evt-reuse",
		knowledge.HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"api","scope_kind":"project","scope_id":"%s"}`, oversized)); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("hard-limit scope forget error = %v, want ErrKnowledgeValidation", err)
	}
	var receiptCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_command_receipts`).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatalf("command receipts after rejected forget = %d, %v; want 0", receiptCount, err)
	}
	if _, _, err := service.Execute(ctx, knowledgeTestBinding("U12345678", "workspace"), "evt-reuse",
		knowledge.HumanCommandPrefix+`{"action":"forget","subject":"api","scope_kind":"project","scope_id":"workspace"}`); err != nil {
		t.Fatalf("valid forget reusing the event identity rejected: %v", err)
	}
	var committedReceipts int
	if err := store.DB().QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM knowledge_command_receipts WHERE source_ref = 'slack-human:evt-reuse'`,
	).Scan(
		&committedReceipts,
	); err != nil ||
		committedReceipts != 1 {
		t.Fatalf("command receipts for the reused event = %d, %v; want exactly 1", committedReceipts, err)
	}
}
