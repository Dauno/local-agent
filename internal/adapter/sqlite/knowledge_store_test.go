package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func newKnowledgeTestStore(t *testing.T) (*KnowledgeStore, *Store) {
	t.Helper()
	store, err := Initialize(t.Context(), t.TempDir()+"/knowledge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewKnowledgeStore(store), store
}

func testKnowledgeStoreClaim() domain.KnowledgeClaim {
	return domain.KnowledgeClaim{
		Subject:     "api",
		Predicate:   domain.KnowledgePredicateRunsOn,
		Value:       domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 17"},
		ScopeKind:   domain.KnowledgeScopeProject,
		ScopeID:     "local-agent",
		SourceClass: domain.KnowledgeSourceHuman,
		SourceRef:   "slack-human:evt-1",
		AuthorID:    "U00000001",
		Status:      domain.KnowledgeClaimAsserted,
	}
}

func knowledgeTestScopes() []domain.KnowledgeScopeRef {
	return []domain.KnowledgeScopeRef{{Kind: domain.KnowledgeScopeProject, ID: "local-agent"}}
}

func knowledgeTestGlobalScopes() []domain.KnowledgeScopeRef {
	return []domain.KnowledgeScopeRef{{Kind: domain.KnowledgeScopeGlobal}}
}

func TestKnowledgeStoreCreateAndReplayClaim(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	created, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Revision != 1 {
		t.Fatalf("created claim = %#v, want ID and revision 1", created)
	}
	got, err := store.GetClaim(t.Context(), created.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "api" || got.Value.Text != "PostgreSQL 17" || got.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("stored claim = %#v", got)
	}

	replay, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
	if replay.ID != created.ID {
		t.Fatalf("replay created a second claim %q; want %q", replay.ID, created.ID)
	}

	second := testKnowledgeStoreClaim()
	second.SourceRef = "slack-human:evt-2"
	second.ID = ""
	secondCreated, err := store.CreateClaim(t.Context(), second, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if secondCreated.ID == created.ID {
		t.Fatal("a different source identity must create a new claim")
	}
}

func TestKnowledgeStoreCreateClaimRejectsInvalidAndSupersedingInput(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim := testKnowledgeStoreClaim()
	claim.SupersedesID = "kclaim_other"
	if _, err := store.CreateClaim(t.Context(), claim, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("supersedes on create error = %v, want ErrKnowledgeValidation", err)
	}
	claim = testKnowledgeStoreClaim()
	claim.Status = domain.KnowledgeClaimExpired
	if _, err := store.CreateClaim(t.Context(), claim, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("expired status error = %v, want ErrKnowledgeValidation", err)
	}
	claim = testKnowledgeStoreClaim()
	claim.ScopeKind = domain.KnowledgeScopeGlobal
	claim.ScopeID = ""
	if _, err := store.CreateClaim(t.Context(), claim, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("global claim error = %v, want ErrKnowledgeValidation", err)
	}
}

func TestKnowledgeStoreCreateClaimBlockedByTombstone(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	forgot, err := store.ForgetSubject(t.Context(), "api", domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-9")
	if err != nil || !forgot {
		t.Fatalf("forget = %v, %v; want true, nil", forgot, err)
	}
	if _, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits()); !errors.Is(err, domain.ErrKnowledgeTombstoneBlocked) {
		t.Fatalf("create after forget error = %v, want ErrKnowledgeTombstoneBlocked", err)
	}
	replay, err := store.ForgetSubject(t.Context(), "api", domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-9")
	if err != nil || replay {
		t.Fatalf("forget replay = %v, %v; want false, nil", replay, err)
	}
}

func TestKnowledgeStoreCorrectClaimSupersedesPrior(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	prior, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.KnowledgeClaim{
		Subject:      prior.Subject,
		Predicate:    prior.Predicate,
		Value:        domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind:    prior.ScopeKind,
		ScopeID:      prior.ScopeID,
		SourceClass:  domain.KnowledgeSourceHuman,
		SourceRef:    "slack-human:evt-2",
		AuthorID:     "U00000001",
		Status:       domain.KnowledgeClaimVerified,
		SupersedesID: prior.ID,
	}
	committed, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if committed.ID == prior.ID || committed.SupersedesID != prior.ID || committed.Revision != 1 {
		t.Fatalf("committed replacement = %#v", committed)
	}
	superseded, err := store.GetClaim(t.Context(), prior.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if superseded.Status != domain.KnowledgeClaimSuperseded || superseded.Value.Text != "PostgreSQL 17" {
		t.Fatalf("prior claim = %#v; provenance must be preserved", superseded)
	}
	if superseded.Revision != prior.Revision+1 {
		t.Fatalf("superseded prior revision = %d, want %d", superseded.Revision, prior.Revision+1)
	}
	var revisionCount int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claim_revisions WHERE claim_id = ?`, string(prior.ID)).Scan(&revisionCount); err != nil || revisionCount != 2 {
		t.Fatalf("prior revision rows = %d, %v", revisionCount, err)
	}
	var supersessionStatus, supersessionClass, supersessionRef string
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT status, source_class, source_ref FROM knowledge_claim_revisions
		WHERE claim_id = ? AND revision_number = 2`, string(prior.ID)).
		Scan(&supersessionStatus, &supersessionClass, &supersessionRef); err != nil {
		t.Fatal(err)
	}
	if supersessionStatus != "superseded" || supersessionClass != "human" || supersessionRef != "slack-human:evt-2" {
		t.Fatalf("supersession revision provenance = %s/%s/%s", supersessionStatus, supersessionClass, supersessionRef)
	}

	replayed, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("correction replay rejected: %v", err)
	}
	if replayed.ID != committed.ID || replayed.SupersedesID != prior.ID || replayed.Value.Text != "PostgreSQL 18" {
		t.Fatalf("correction replay = %#v; want the originally committed replacement", replayed)
	}

	conflicting := replacement
	conflicting.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 19"}
	if _, err := store.CorrectClaim(t.Context(), conflicting, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("conflicting correction replay error = %v, want ErrKnowledgeCASConflict", err)
	}
	otherSource := replacement
	otherSource.SourceRef = "slack-human:evt-3"
	if _, err := store.CorrectClaim(t.Context(), otherSource, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("second correction of superseded prior error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStoreCorrectionRejectsMismatchedSourceAndScope(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	prior, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.KnowledgeClaim{
		Subject: prior.Subject, Predicate: prior.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: prior.ScopeKind, ScopeID: "other-project",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: prior.ID,
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("cross-scope correction error = %v, want ErrKnowledgeValidation", err)
	}
	replacement.ScopeID = prior.ScopeID
	replacement.SourceClass = domain.KnowledgeSourceDecision
	replacement.AuthorID = ""
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("mismatched source error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceClass("curator"), domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("removed curator source error = %v, want ErrKnowledgeValidation", err)
	}
	superseded, err := store.GetClaim(t.Context(), prior.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if superseded.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by rejected corrections", superseded.Status)
	}
}

func TestKnowledgeStoreTransitionClaimStatus(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	verified, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-verify")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != domain.KnowledgeClaimVerified {
		t.Fatalf("transitioned status = %s", verified.Status)
	}
	if verified.Revision != claim.Revision+1 {
		t.Fatalf("transitioned revision = %d, want %d", verified.Revision, claim.Revision+1)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, verified.Revision, domain.KnowledgeClaimDisputed, domain.KnowledgeSourceClass("curator"), "exchange-1"); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("removed curator source dispute error = %v, want ErrKnowledgeValidation", err)
	}
	disputed, err := store.TransitionClaimStatus(t.Context(), claim.ID, verified.Revision, domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman, "slack-human:evt-dispute")
	if err != nil {
		t.Fatalf("human dispute with advanced revision rejected: %v", err)
	}
	if disputed.Status != domain.KnowledgeClaimDisputed || disputed.Revision != verified.Revision+1 {
		t.Fatalf("second transition = %#v", disputed)
	}
	replayedTransition, err := store.TransitionClaimStatus(t.Context(), claim.ID, verified.Revision, domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman, "slack-human:evt-dispute")
	if err != nil {
		t.Fatalf("committed transition replay rejected: %v", err)
	}
	if replayedTransition.Status != domain.KnowledgeClaimDisputed || replayedTransition.Revision != disputed.Revision {
		t.Fatalf("transition replay = %#v; want idempotent return of the current claim", replayedTransition)
	}
	retried, err := store.TransitionClaimStatus(t.Context(), claim.ID, disputed.Revision, domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman, "slack-human:evt-dispute")
	if err != nil {
		t.Fatalf("transition retry with current revision rejected: %v", err)
	}
	if retried.Revision != disputed.Revision {
		t.Fatalf("transition retry advanced revision to %d; receipts must not bump revisions", retried.Revision)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, disputed.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-dispute"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same source different transition error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, disputed.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-1"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("creation identity reuse error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, disputed.Revision, domain.KnowledgeClaimSuperseded, domain.KnowledgeSourceHuman, "slack-human:evt-supersede"); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("direct supersede error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), "kclaim_missing", 1, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-x"); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("missing claim error = %v, want ErrKnowledgeNotFound", err)
	}
	var revisionRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claim_revisions WHERE claim_id = ?`, string(claim.ID)).Scan(&revisionRows); err != nil || revisionRows != 3 {
		t.Fatalf("revision rows after two transitions and replays = %d, %v; want 3", revisionRows, err)
	}
}

func TestKnowledgeStoreTransitionsAndEvidenceEnqueueProjection(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-verify"); err != nil {
		t.Fatal(err)
	}
	evidence := domain.KnowledgeEvidence{
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		ExchangeTS:      "1723543200.123456",
		AuthorID:        "U00000001",
		Kind:            domain.KnowledgeEvidenceSource,
	}
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, evidence); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(t.Context())
	if err != nil || len(items) != 3 {
		t.Fatalf("projection batch after transition/evidence = %d items, %v; want 3", len(items), err)
	}
	ids := make([]int, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if err := store.CompleteProjectionBatch(t.Context(), ids, items[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if next, err := store.ClaimProjectionBatch(t.Context()); err != nil || len(next) != 0 {
		t.Fatalf("exhausted projection items = %v, %v", next, err)
	}
}

func TestKnowledgeStoreForgetRemovesContentAndBlocksReplay(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "local-agent", time.Now().UTC())
	document, err := store.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "api", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}

	forgot, err := store.ForgetSubject(t.Context(), "api", domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-9")
	if err != nil || !forgot {
		t.Fatalf("forget = %v, %v", forgot, err)
	}
	if _, err := store.GetClaim(t.Context(), claim.ID, knowledgeTestScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("claim after forget error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, err := store.GetDocument(t.Context(), document.ID, knowledgeTestScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("document after forget error = %v, want ErrKnowledgeNotFound", err)
	}
	var revisionRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claim_revisions`).Scan(&revisionRows); err != nil || revisionRows != 0 {
		t.Fatalf("revision rows after forget = %d, %v", revisionRows, err)
	}

	if _, err := store.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "api", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "mem_topic_2",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits()); !errors.Is(err, domain.ErrKnowledgeTombstoneBlocked) {
		t.Fatalf("document after forget error = %v, want ErrKnowledgeTombstoneBlocked", err)
	}

	now := time.Now().UTC().UnixNano()
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, provenance, status, created_at, updated_at)
		VALUES ('kdoc_bypass', 'api', 'project', 'local-agent', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'h', 'curated', 'active', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ForgetSubject(t.Context(), "api", domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-9")
	if err != nil || replayed {
		t.Fatalf("forget replay = %v, %v; want false, nil", replayed, err)
	}
	if _, err := store.GetDocument(t.Context(), "kdoc_bypass", knowledgeTestScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("bypass document survived replay forget: %v", err)
	}

	other, err := store.CreateClaim(t.Context(), func() domain.KnowledgeClaim {
		claim := testKnowledgeStoreClaim()
		claim.Subject = "api"
		claim.ScopeID = "other-project"
		claim.SourceRef = "slack-human:evt-10"
		return claim
	}(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("claim in another scope rejected after forget: %v", err)
	}
	if other.ID == "" {
		t.Fatal("cross-scope claim must not be blocked by this tombstone")
	}
}

func TestKnowledgeStoreEvidenceRequiresLedgerIdentities(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	evidence := domain.KnowledgeEvidence{
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		ExchangeTS:      "1723543200.123456",
		AuthorID:        "U00000001",
		Kind:            domain.KnowledgeEvidenceSource,
	}
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, evidence); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, evidence); err != nil {
		t.Fatalf("evidence replay rejected: %v", err)
	}
	var evidenceRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_evidence`).Scan(&evidenceRows); err != nil || evidenceRows != 1 {
		t.Fatalf("evidence rows after replay = %d, %v; want 1", evidenceRows, err)
	}
	invalid := evidence
	invalid.ConversationKey = "hello"
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, invalid); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("invalid conversation key error = %v, want ErrKnowledgeValidation", err)
	}
	if err := store.AddEvidence(t.Context(), claim.ID, 99, evidence); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("unknown revision error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestKnowledgeStoreReplayIsIdempotentAtLimits(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	limits := domain.DefaultKnowledgeLimits()
	limits.MaxClaimsPerSubject = 1
	limits.MaxPreferences = 1
	created, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), limits)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), limits)
	if err != nil {
		t.Fatalf("claim replay at limit rejected: %v", err)
	}
	if replay.ID != created.ID {
		t.Fatalf("claim replay at limit created %q; want %q", replay.ID, created.ID)
	}
	second := testKnowledgeStoreClaim()
	second.SourceRef = "slack-human:evt-2"
	if _, err := store.CreateClaim(t.Context(), second, limits); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("second claim at limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}

	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	createdPreference, err := store.CreatePreference(t.Context(), preference, limits)
	if err != nil {
		t.Fatal(err)
	}
	replayedPreference, err := store.CreatePreference(t.Context(), preference, limits)
	if err != nil {
		t.Fatalf("preference replay at limit rejected: %v", err)
	}
	if replayedPreference.ID != createdPreference.ID {
		t.Fatalf("preference replay at limit created %d; want %d", replayedPreference.ID, createdPreference.ID)
	}
	otherPreference := preference
	otherPreference.Key = "timezone"
	if _, err := store.CreatePreference(t.Context(), otherPreference, limits); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("second preference at limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}
}

func TestKnowledgeStoreReplayRejectsDifferentPayload(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	if _, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	conflicting := testKnowledgeStoreClaim()
	conflicting.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 16"}
	if _, err := store.CreateClaim(t.Context(), conflicting, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("conflicting claim replay error = %v, want ErrKnowledgeCASConflict", err)
	}
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	if _, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	conflictingPreference := preference
	conflictingPreference.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	if _, err := store.CreatePreference(t.Context(), conflictingPreference, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("conflicting preference replay error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStoreUpdatePreferenceAdvancesRevision(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	corrected := preference
	corrected.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	corrected.SourceRef = "slack-human:evt-5"
	updated, err := store.UpdatePreference(t.Context(), corrected, created.Revision, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != created.Revision+1 || updated.Value.Text != "English" || updated.SourceRef != "slack-human:evt-5" {
		t.Fatalf("updated preference = %#v", updated)
	}
	stale := preference
	stale.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "French"}
	if _, err := store.UpdatePreference(t.Context(), stale, created.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("stale update error = %v, want ErrKnowledgeCASConflict", err)
	}
	replay := preference
	replay.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	replay.SourceRef = "slack-human:evt-5"
	replayed, err := store.UpdatePreference(t.Context(), replay, updated.Revision, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("update replay rejected: %v", err)
	}
	if replayed.Revision != updated.Revision || replayed.Value.Text != "English" {
		t.Fatalf("update replay = %#v; want idempotent return of the committed revision", replayed)
	}
	foreign := corrected
	foreign.OwnerKey = "slack:T00000001:user:U99999999"
	if _, err := store.UpdatePreference(t.Context(), foreign, updated.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("foreign owner update error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestKnowledgeStorePreferencesOwnerIsolation(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Revision != 1 {
		t.Fatalf("created preference = %#v", created)
	}
	replay, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != created.ID {
		t.Fatalf("preference replay created %d; want %d", replay.ID, created.ID)
	}
	listed, err := store.ListPreferencesForOwner(t.Context(), preference.OwnerKey, domain.DefaultKnowledgeLimits())
	if err != nil || len(listed) != 1 {
		t.Fatalf("owner list = %v, %v", listed, err)
	}
	foreign, err := store.ListPreferencesForOwner(t.Context(), "slack:T00000001:user:U99999999", domain.DefaultKnowledgeLimits())
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign owner list = %v, %v; want empty", foreign, err)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision+1, "slack-human:evt-archive"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("stale archive error = %v, want ErrKnowledgeCASConflict", err)
	}
	archived, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision, "slack-human:evt-archive")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != domain.KnowledgePreferenceArchived {
		t.Fatalf("archived status = %s", archived.Status)
	}
	listed, err = store.ListPreferencesForOwner(t.Context(), preference.OwnerKey, domain.DefaultKnowledgeLimits())
	if err != nil || len(listed) != 0 {
		t.Fatalf("active list after archive = %v, %v; want empty", listed, err)
	}
}

func TestKnowledgeStoreDocumentsAndArchive(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	documentScopes := []domain.KnowledgeScopeRef{{Kind: domain.KnowledgeScopeTeam, ID: "T12345678"}}
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())
	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	created, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	replayedDocument, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("document replay rejected: %v", err)
	}
	if replayedDocument.ID != created.ID {
		t.Fatalf("document replay created %q; want %q", replayedDocument.ID, created.ID)
	}
	conflicting := document
	conflicting.ContentDigest = strings.Repeat("b", 64)
	if _, err := store.CreateDocument(t.Context(), conflicting, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("conflicting document error = %v, want ErrKnowledgeCASConflict", err)
	}
	invalid := document
	invalid.SourceID = "source-1"
	if _, err := store.CreateDocument(t.Context(), invalid, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("curated with source identity error = %v, want ErrKnowledgeValidation", err)
	}
	got, err := store.GetDocument(t.Context(), created.ID, documentScopes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != domain.KnowledgeProvenanceCurated || got.SourceRev != 0 {
		t.Fatalf("stored document = %#v", got)
	}
	listed, err := store.ListDocumentsInScopes(t.Context(), documentScopes, domain.DefaultKnowledgeLimits())
	if err != nil || len(listed) != 1 {
		t.Fatalf("team documents = %v, %v", listed, err)
	}
	if _, err := store.ArchiveDocument(t.Context(), "kdoc_missing", 1, "slack-human:evt-missing"); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("archive missing error = %v, want ErrKnowledgeNotFound", err)
	}
	archived, err := store.ArchiveDocument(t.Context(), created.ID, 1, "slack-human:evt-archive")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != domain.KnowledgeDocumentArchived || archived.Revision != 2 {
		t.Fatalf("archived document = %#v, want archived revision 2", archived)
	}
	replayed, err := store.ArchiveDocument(t.Context(), created.ID, 1, "slack-human:evt-archive")
	if err != nil {
		t.Fatalf("archive replay rejected: %v", err)
	}
	if replayed.Revision != 2 {
		t.Fatalf("archive replay advanced revision to %d", replayed.Revision)
	}
	var attributed string
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT source_ref FROM knowledge_document_revisions
		WHERE document_id = ? AND revision_number = 2`, string(created.ID)).Scan(&attributed); err != nil || attributed != "slack-human:evt-archive" {
		t.Fatalf("document revision attribution = %q, %v", attributed, err)
	}
	if _, err := store.ArchiveDocument(t.Context(), created.ID, 2, "slack-human:evt-other"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archive by another source error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStoreProjectionOutboxLifecycle(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	if item.Status != domain.KnowledgeProjectionProcessing || item.Attempts != 1 {
		t.Fatalf("claimed item = %#v", item)
	}
	duplicate, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(duplicate) != 0 {
		t.Fatalf("second claim = %v, %v; want none while leased", duplicate, err)
	}
	if err := store.CompleteProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("double complete error = %v, want ErrKnowledgeCASConflict", err)
	}
	if err := store.CleanupProjection(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, err := store.ClaimProjectionBatch(ctx); err != nil || len(next) != 0 {
		t.Fatalf("claim after cleanup = %v, %v; want none", next, err)
	}
}

func TestKnowledgeStoreUnavailableWithoutConfig(t *testing.T) {
	store := &KnowledgeStore{}
	if _, err := store.CreateClaim(context.Background(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("unconfigured store error = %v, want ErrKnowledgeUnavailable", err)
	}
}

func TestKnowledgeStoreReplayRejectsDifferentStatus(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	verifiedReplay := testKnowledgeStoreClaim()
	verifiedReplay.Status = domain.KnowledgeClaimVerified
	if _, err := store.CreateClaim(t.Context(), verifiedReplay, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("verified replay of asserted claim error = %v, want ErrKnowledgeCASConflict", err)
	}

	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	if _, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	archivedReplay := preference
	archivedReplay.Status = domain.KnowledgePreferenceArchived
	if _, err := store.CreatePreference(t.Context(), archivedReplay, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archived replay of active preference error = %v, want ErrKnowledgeCASConflict", err)
	}

	documentResultID, documentDigest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "local-agent", time.Now().UTC())
	document := domain.KnowledgeDocument{
		Subject: "api", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		ContentDigest: documentDigest, ContentHandle: "result:" + documentResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if _, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	archivedDocument := document
	archivedDocument.Status = domain.KnowledgeDocumentArchived
	if _, err := store.CreateDocument(t.Context(), archivedDocument, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archived replay of active document error = %v, want ErrKnowledgeCASConflict", err)
	}

	correction := domain.KnowledgeClaim{
		Subject: claim.Subject, Predicate: claim.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: claim.ScopeKind, ScopeID: claim.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: claim.ID,
	}
	committed, err := store.CorrectClaim(t.Context(), correction, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	_ = committed
	assertedReplay := correction
	assertedReplay.Status = domain.KnowledgeClaimAsserted
	if _, err := store.CorrectClaim(t.Context(), assertedReplay, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("asserted replay of verified correction error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStorePreferenceUpdateReplaySurvivesLaterUpdates(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	english := preference
	english.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	english.SourceRef = "slack-human:evt-5"
	if _, err := store.UpdatePreference(t.Context(), english, created.Revision, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	french := preference
	french.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "French"}
	french.SourceRef = "slack-human:evt-6"
	updated, err := store.UpdatePreference(t.Context(), french, created.Revision+1, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.UpdatePreference(t.Context(), english, created.Revision, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("replay of earlier update rejected: %v", err)
	}
	if replayed.Revision != updated.Revision || replayed.Value.Text != "French" {
		t.Fatalf("earlier update replay = %#v; want idempotent return of current state", replayed)
	}
	conflicting := english
	conflicting.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "German"}
	if _, err := store.UpdatePreference(t.Context(), conflicting, created.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("reused source with different content error = %v, want ErrKnowledgeCASConflict", err)
	}
	archivedInput := french
	archivedInput.Status = domain.KnowledgePreferenceArchived
	if _, err := store.UpdatePreference(t.Context(), archivedInput, updated.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("archived update input error = %v, want ErrKnowledgeValidation", err)
	}
}

func TestKnowledgeStoreCorrectionsRespectClaimsLimit(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	limits := domain.DefaultKnowledgeLimits()
	limits.MaxClaimsPerSubject = 1
	prior, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), limits)
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.KnowledgeClaim{
		Subject: prior.Subject, Predicate: prior.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: prior.ScopeKind, ScopeID: prior.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: prior.ID,
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, limits); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("correction at limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}
	still, err := store.GetClaim(t.Context(), prior.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("rejected correction mutated prior to %s", still.Status)
	}
}

func TestKnowledgeStoreEvidenceRejectsAuthorMismatch(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	evidence := domain.KnowledgeEvidence{
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		ExchangeTS:      "1723543200.123456",
		AuthorID:        "U00000001",
		Kind:            domain.KnowledgeEvidenceSource,
	}
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, evidence); err != nil {
		t.Fatal(err)
	}
	foreign := evidence
	foreign.AuthorID = "U99999999"
	if err := store.AddEvidence(t.Context(), claim.ID, claim.Revision, foreign); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("author mismatch error = %v, want ErrKnowledgeValidation", err)
	}
	var rows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_evidence`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("evidence rows after mismatch = %d, %v", rows, err)
	}
}

func TestKnowledgeStoreTransitionRecordsStructuredProvenance(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-verify"); err != nil {
		t.Fatal(err)
	}
	var status, sourceClass, sourceRef string
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT status, source_class, source_ref FROM knowledge_claim_revisions
		WHERE claim_id = ? AND revision_number = 2`, string(claim.ID)).Scan(&status, &sourceClass, &sourceRef); err != nil {
		t.Fatal(err)
	}
	if status != "verified" || sourceClass != "human" || sourceRef != "slack-human:evt-verify" {
		t.Fatalf("transition revision provenance = %s/%s/%s", status, sourceClass, sourceRef)
	}
}

func TestKnowledgeStoreArchivePreferenceAdvancesRevision(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision, "slack-human:evt-archive")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != domain.KnowledgePreferenceArchived || archived.Revision != created.Revision+1 {
		t.Fatalf("archived preference = %#v; want archived with advanced revision", archived)
	}
	var revisionRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_preference_revisions WHERE preference_id = ?`, created.ID).Scan(&revisionRows); err != nil || revisionRows != 2 {
		t.Fatalf("preference revision rows after archive = %d, %v; want 2", revisionRows, err)
	}
}

func TestKnowledgeStoreCreateReplaySurvivesLaterMutations(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-verify"); err != nil {
		t.Fatal(err)
	}
	replayedClaim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("creation receipt rejected after transition: %v", err)
	}
	if replayedClaim.ID != claim.ID || replayedClaim.Status != domain.KnowledgeClaimVerified || replayedClaim.Revision != claim.Revision+1 {
		t.Fatalf("claim replay after transition = %#v; want current state against the immutable receipt", replayedClaim)
	}

	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	createdPreference, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	updated := preference
	updated.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	updated.SourceRef = "slack-human:evt-5"
	if _, err := store.UpdatePreference(t.Context(), updated, createdPreference.Revision, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, createdPreference.Revision+1, "slack-human:evt-archive"); err != nil {
		t.Fatal(err)
	}
	replayedPreference, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("creation receipt rejected after update and archive: %v", err)
	}
	if replayedPreference.ID != createdPreference.ID || replayedPreference.Status != domain.KnowledgePreferenceArchived {
		t.Fatalf("preference replay after archive = %#v; want current archived state", replayedPreference)
	}

	documentResultID, documentDigest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())
	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: documentDigest, ContentHandle: "result:" + documentResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	createdDocument, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveDocument(t.Context(), createdDocument.ID, 1, "slack-human:evt-archive"); err != nil {
		t.Fatal(err)
	}
	replayedDocument, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("creation receipt rejected after document archive: %v", err)
	}
	if replayedDocument.ID != createdDocument.ID || replayedDocument.Status != domain.KnowledgeDocumentArchived {
		t.Fatalf("document replay after archive = %#v; want current archived state", replayedDocument)
	}
	var receiptRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_document_receipts`).Scan(&receiptRows); err != nil || receiptRows != 1 {
		t.Fatalf("document receipt rows = %d, %v; want 1", receiptRows, err)
	}
}

func TestKnowledgeStoreArchivePreferenceReceipt(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision, "slack-human:evt-archive")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, 1, "slack-human:evt-archive")
	if err != nil {
		t.Fatalf("archive receipt rejected: %v", err)
	}
	if replayed.ID != archived.ID || replayed.Revision != archived.Revision {
		t.Fatalf("archive replay = %#v; want idempotent return of the archived state", replayed)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, archived.Revision, "slack-human:evt-other"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archive with different source after archive error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, archived.Revision, "slack-human:evt-4"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archive with creation source error = %v, want ErrKnowledgeCASConflict", err)
	}
	updateWithArchiveSource := preference
	updateWithArchiveSource.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	updateWithArchiveSource.SourceRef = "slack-human:evt-archive"
	if _, err := store.UpdatePreference(t.Context(), updateWithArchiveSource, archived.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("update with archive source error = %v, want ErrKnowledgeCASConflict", err)
	}
	var archivedStatus string
	if err := store.db.QueryRowContext(t.Context(), `SELECT status FROM knowledge_preference_revisions WHERE preference_id = ? AND revision_number = 2`, created.ID).Scan(&archivedStatus); err != nil {
		t.Fatal(err)
	}
	if archivedStatus != "archived" {
		t.Fatalf("archive revision status = %q; history must reconstruct the archive", archivedStatus)
	}
}

func TestKnowledgeStorePreferenceUpdateReplaySurvivesArchive(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	english := preference
	english.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "English"}
	english.SourceRef = "slack-human:evt-5"
	if _, err := store.UpdatePreference(t.Context(), english, created.Revision, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	archived, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision+1, "slack-human:evt-archive")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.UpdatePreference(t.Context(), english, created.Revision, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("update receipt rejected after archive: %v", err)
	}
	if replayed.ID != archived.ID || replayed.Status != domain.KnowledgePreferenceArchived || replayed.Revision != archived.Revision {
		t.Fatalf("update replay after archive = %#v; want current archived state", replayed)
	}
	conflicting := english
	conflicting.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "German"}
	if _, err := store.UpdatePreference(t.Context(), conflicting, created.Revision, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("reused source with different content after archive error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStoreCorrectionRejectsSourceUsedByPrior(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	prior, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), prior.ID, prior.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-2"); err != nil {
		t.Fatal(err)
	}
	replacement := domain.KnowledgeClaim{
		Subject: prior.Subject, Predicate: prior.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: prior.ScopeKind, ScopeID: prior.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: prior.ID,
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("correction reusing prior transition source error = %v, want ErrKnowledgeCASConflict", err)
	}
	still, err := store.GetClaim(t.Context(), prior.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != domain.KnowledgeClaimVerified {
		t.Fatalf("prior mutated to %s by rejected correction", still.Status)
	}
}

func TestKnowledgeStoreTransitionReceiptAuthenticatesCommand(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "slack-human:evt-verify"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision+1, domain.KnowledgeClaimVerified, domain.KnowledgeSourceClass("curator"), "slack-human:evt-verify"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("authority-class impostor replay error = %v, want ErrKnowledgeCASConflict", err)
	}
	replacement := domain.KnowledgeClaim{
		Subject: claim.Subject, Predicate: claim.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: claim.ScopeKind, ScopeID: claim.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: claim.ID,
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	superseded, err := store.GetClaim(t.Context(), claim.ID, knowledgeTestScopes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, superseded.Revision, domain.KnowledgeClaimSuperseded, domain.KnowledgeSourceHuman, "slack-human:evt-2"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("supersession row impostor replay error = %v, want ErrKnowledgeCASConflict", err)
	}
	var revisionRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claim_revisions WHERE claim_id = ?`, string(claim.ID)).Scan(&revisionRows); err != nil || revisionRows != 3 {
		t.Fatalf("revision rows after impostor attempts = %d, %v; want 3", revisionRows, err)
	}
	var operation string
	if err := store.db.QueryRowContext(t.Context(), `SELECT operation FROM knowledge_claim_revisions WHERE claim_id = ? AND source_ref = ?`, string(claim.ID), "slack-human:evt-2").Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if operation != "supersede" {
		t.Fatalf("supersession revision operation = %q, want supersede", operation)
	}
}

func TestKnowledgeStoreArchivedCreationCannotImpersonateArchive(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceArchived, SourceRef: "slack-human:evt-4",
	}
	created, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.KnowledgePreferenceArchived {
		t.Fatalf("created preference status = %s", created.Status)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision, "slack-human:evt-4"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("creation receipt impostor archive error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, created.Revision, "slack-human:evt-other"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("archive of already-archived preference error = %v, want ErrKnowledgeCASConflict", err)
	}
	replayed, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("archived creation receipt rejected: %v", err)
	}
	if replayed.ID != created.ID || replayed.Status != domain.KnowledgePreferenceArchived {
		t.Fatalf("archived creation replay = %#v", replayed)
	}
	listed, err := store.ListPreferencesForOwner(t.Context(), preference.OwnerKey, domain.DefaultKnowledgeLimits())
	if err != nil || len(listed) != 0 {
		t.Fatalf("active list for archived creation = %v, %v; want empty", listed, err)
	}
}

func TestKnowledgeStoreMutationSourceReferencesAreValidated(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("x", domain.DefaultMaxKnowledgeSourceRefRunes+1)
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, oversized); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("oversized transition source error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), claim.ID, claim.Revision, domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman, "password: hunter2secret"); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("credential transition source error = %v, want ErrKnowledgeValidation", err)
	}
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4",
	}
	if _, err := store.CreatePreference(t.Context(), preference, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchivePreference(t.Context(), preference.OwnerKey, preference.Key, 1, oversized); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("oversized archive source error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.ForgetSubject(t.Context(), "api", domain.KnowledgeScopeProject, "local-agent", oversized); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("oversized forget source error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.ForgetSubject(t.Context(), "", domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-9"); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("empty forget subject error = %v, want ErrKnowledgeValidation", err)
	}
	var revisionRows int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claim_revisions WHERE claim_id = ?`, string(claim.ID)).Scan(&revisionRows); err != nil || revisionRows != 1 {
		t.Fatalf("revision rows after rejected mutations = %d, %v; want 1", revisionRows, err)
	}
}

func TestKnowledgeStoreCommandReceiptsAreGlobalAndImmutable(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	receipt := domain.KnowledgeCommandReceipt{
		SourceRef: "slack-human:evt-1", Action: domain.KnowledgeActionRemember,
		PayloadDigest: strings.Repeat("a", 64), Target: "claim:api",
	}
	if err := store.CommitCommandReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitCommandReceipt(t.Context(), receipt); err != nil {
		t.Fatalf("identical receipt replay rejected: %v", err)
	}
	otherTarget := receipt
	otherTarget.Target = "claim:db"
	if err := store.CommitCommandReceipt(t.Context(), otherTarget); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same source different target error = %v, want ErrKnowledgeCASConflict", err)
	}
	otherAction := receipt
	otherAction.Action = domain.KnowledgeActionForget
	if err := store.CommitCommandReceipt(t.Context(), otherAction); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same source different action error = %v, want ErrKnowledgeCASConflict", err)
	}
	otherDigest := receipt
	otherDigest.PayloadDigest = strings.Repeat("b", 64)
	if err := store.CommitCommandReceipt(t.Context(), otherDigest); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same source different digest error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE knowledge_command_receipts SET target = 'mutated'`); err == nil {
		t.Error("command receipt mutation unexpectedly succeeded")
	}
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM knowledge_command_receipts`); err == nil {
		t.Error("command receipt deletion unexpectedly succeeded")
	}
	invalid := domain.KnowledgeCommandReceipt{
		SourceRef: "slack-human:evt-2", Action: domain.KnowledgeActionInspect,
		PayloadDigest: strings.Repeat("c", 64), Target: "claim:api",
	}
	if err := store.CommitCommandReceipt(t.Context(), invalid); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("read-only action receipt error = %v, want ErrKnowledgeValidation", err)
	}
}

func TestKnowledgeStoreReadsFilterByReadableScopes(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	documentScopes := []domain.KnowledgeScopeRef{{Kind: domain.KnowledgeScopeTeam, ID: "T12345678"}}
	documentResultID, documentDigest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())
	document, err := store.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "runbook", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: documentDigest, ContentHandle: "result:" + documentResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetClaim(t.Context(), claim.ID, knowledgeTestGlobalScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("project claim read under global-only scopes error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, err := store.GetClaim(t.Context(), claim.ID, nil); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("claim read under empty scopes error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, err := store.GetClaim(t.Context(), claim.ID, knowledgeTestScopes()); err != nil {
		t.Fatalf("claim read under its own scope rejected: %v", err)
	}
	if _, err := store.GetDocument(t.Context(), document.ID, knowledgeTestScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("team document read under project scopes error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, err := store.GetDocument(t.Context(), document.ID, documentScopes); err != nil {
		t.Fatalf("team document read under its own scope rejected: %v", err)
	}
	claims, err := store.ListClaimsInScopes(t.Context(), knowledgeTestScopes(), "", domain.DefaultKnowledgeLimits())
	if err != nil || len(claims) != 1 || claims[0].ID != claim.ID {
		t.Fatalf("claims in scopes = %v, %v", claims, err)
	}
	claims, err = store.ListClaimsInScopes(t.Context(), knowledgeTestGlobalScopes(), "", domain.DefaultKnowledgeLimits())
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims under global-only scopes = %v, %v", claims, err)
	}
	documents, err := store.ListDocumentsInScopes(t.Context(), documentScopes, domain.DefaultKnowledgeLimits())
	if err != nil || len(documents) != 1 || documents[0].ID != document.ID {
		t.Fatalf("documents in scopes = %v, %v", documents, err)
	}
}

func TestKnowledgeStoreArchiveDocumentStaleRevisionConflicts(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())
	created, err := store.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "runbook", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveDocument(t.Context(), created.ID, 1, "slack-human:evt-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveDocument(t.Context(), created.ID, 1, "slack-human:evt-2"); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("stale archive error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeStoreListingsApplyStatusFilterToAllScopes(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	claim, err := store.CreateClaim(t.Context(), testKnowledgeStoreClaim(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.KnowledgeClaim{
		Subject: claim.Subject, Predicate: claim.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: claim.ScopeKind, ScopeID: claim.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-2",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimVerified, SupersedesID: claim.ID,
	}
	if _, err := store.CorrectClaim(t.Context(), replacement, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}
	archivedClaim, err := store.CreateClaim(t.Context(), func() domain.KnowledgeClaim {
		claim := testKnowledgeStoreClaim()
		claim.Subject = "other-api"
		claim.SourceRef = "slack-human:evt-3"
		return claim
	}(), domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionClaimStatus(t.Context(), archivedClaim.ID, archivedClaim.Revision, domain.KnowledgeClaimArchived, domain.KnowledgeSourceHuman, "slack-human:evt-4"); err != nil {
		t.Fatal(err)
	}
	documentResultID, documentDigest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())
	document, err := store.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "runbook", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: documentDigest, ContentHandle: "result:" + documentResultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveDocument(t.Context(), document.ID, 1, "slack-human:evt-5"); err != nil {
		t.Fatal(err)
	}

	// Project scope is listed first; with incorrect SQL precedence the
	// status filter would apply only to the trailing alternative and
	// superseded, archived, and archived-document rows would leak.
	scopes := []domain.KnowledgeScopeRef{
		{Kind: domain.KnowledgeScopeProject, ID: "local-agent"},
		{Kind: domain.KnowledgeScopeTeam, ID: "T12345678"},
	}
	claims, err := store.ListClaimsInScopes(t.Context(), scopes, "", domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range claims {
		if listed.Status == domain.KnowledgeClaimSuperseded || listed.Status == domain.KnowledgeClaimArchived {
			t.Fatalf("listing leaked %s claim %s", listed.Status, listed.ID)
		}
	}
	documents, err := store.ListDocumentsInScopes(t.Context(), scopes, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range documents {
		if listed.Status != domain.KnowledgeDocumentActive {
			t.Fatalf("listing leaked %s document %s", listed.Status, listed.ID)
		}
	}
}

func TestKnowledgeStoreForgetHonorsConfiguredSubjectLimits(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	limits := domain.KnowledgeLimits{MaxSubjectRunes: 512}
	subject := strings.Repeat("s", 300)
	claim, err := store.CreateClaim(t.Context(), func() domain.KnowledgeClaim {
		claim := testKnowledgeStoreClaim()
		claim.Subject = subject
		return claim
	}(), limits)
	if err != nil {
		t.Fatalf("amplified create rejected: %v", err)
	}
	if _, err := store.ForgetSubject(t.Context(), subject, domain.KnowledgeScopeProject, "local-agent", "slack-human:evt-f"); err != nil {
		t.Fatalf("amplified forget rejected: %v", err)
	}
	if _, err := store.GetClaim(t.Context(), claim.ID, knowledgeTestScopes()); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("claim survived forget: %v", err)
	}
}

func TestKnowledgeStoreSubjectListingEscapesGlobalBound(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	limits := domain.DefaultKnowledgeLimits()
	for i := range 4 {
		claim := testKnowledgeStoreClaim()
		claim.Subject = "same-subject"
		claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: fmt.Sprintf("value-%d", i)}
		claim.SourceRef = fmt.Sprintf("slack-human:evt-s%d", i)
		if _, err := store.CreateClaim(t.Context(), claim, limits); err != nil {
			t.Fatal(err)
		}
	}
	// A tiny global listing bound must not truncate a subject-scoped listing:
	// the subject selector is bounded per scope by MaxClaimsPerSubject.
	strict := limits
	strict.MaxClaimsListing = 2
	claims, err := store.ListClaimsInScopes(t.Context(), knowledgeTestScopes(), "same-subject", strict)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 4 {
		t.Fatalf("subject-scoped listing returned %d claims of 4 under MaxClaimsListing=2", len(claims))
	}
	global, err := store.ListClaimsInScopes(t.Context(), knowledgeTestScopes(), "", strict)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 3 {
		t.Fatalf("global listing returned %d rows, want exactly 3 (MaxClaimsListing 2 + sentinel)", len(global))
	}
}
