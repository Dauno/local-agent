package sqlite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// newAnalysisStoreFixture opens a fresh v40 database, seeds one
// result_records row (AnalysisStore.Create looks up its bytes to populate
// result_analyses.source_bytes), and returns the store plus that source
// result's id and digest.
func newAnalysisStoreFixture(t *testing.T) (*AnalysisStore, string, string) {
	t.Helper()
	store, err := Initialize(t.Context(), t.TempDir()+"/analysis-store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sourceID := strings.Repeat("a", 64)
	sourceSHA := strings.Repeat("b", 64)
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'op-1', 0, 'artifact', 'key-1', ?, 1024, 'text/plain', 'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'context', 1, 'available')`,
		sourceID, sourceSHA); err != nil {
		t.Fatal(err)
	}
	return NewAnalysisStore(store), sourceID, sourceSHA
}

func testAnalysisLimits() domain.AnalysisLimits {
	return domain.AnalysisLimits{
		MaxSegmentBytes: 24576, OverlapBasisPoints: 1000, OverlapMaxBytes: 4096,
		MaxLeaves: 64, MaxReductionFanIn: 8, MaxReductionDepth: 4, MaxConcurrentLeaves: 2,
		MaxAttemptsPerStep: 2, CallTimeoutSeconds: 120, WallTimeSeconds: 900,
		EvidenceExcerptBytes: 2048, EvidenceSelectorsPerLeaf: 8, EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
	}
}

func testAnalysisIdentity(sourceID, sourceSHA string) domain.AnalysisIdentity {
	return domain.AnalysisIdentity{
		SourceResultID:      sourceID,
		SourceSHA256:        sourceSHA,
		ObjectiveClass:      domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     strings.Repeat("c", 64),
		SegmentationVersion: "text_v1",
		PromptVersion:       "leaf_v1",
		ModelFingerprint:    "model-v1",
		LimitsDigest:        testAnalysisLimits().Digest(),
	}
}

func testAnalysisScope() domain.ResultScope {
	return domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
}

func TestAnalysisStoreCreateIdempotentBySemanticIdentity(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	first, err := store.Create(t.Context(), identity, testAnalysisLimits(), "what changed?", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !domain.ValidAnalysisID(first.AnalysisID) {
		t.Fatalf("first create returned a malformed analysis id %q", first.AnalysisID)
	}

	second, err := store.Create(t.Context(), identity, testAnalysisLimits(), "what changed?", scope, "ws-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.AnalysisID != first.AnalysisID {
		t.Fatalf("second create returned a different analysis id: %q vs %q", second.AnalysisID, first.AnalysisID)
	}

	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_analyses`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one durable row, got %d", count)
	}
}

func TestAnalysisStoreCreateDistinctOnComponentChange(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	base := testAnalysisIdentity(sourceID, sourceSHA)
	first, err := store.Create(t.Context(), base, testAnalysisLimits(), "objective one", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	changed := base
	changed.ObjectiveDigest = strings.Repeat("e", 64)
	second, err := store.Create(t.Context(), changed, testAnalysisLimits(), "objective two", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.AnalysisID == first.AnalysisID {
		t.Fatal("expected a distinct analysis id for a distinct objective digest")
	}

	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_analyses`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected two durable rows, got %d", count)
	}
}

func TestAnalysisStoreScopeIsolation(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	created, err := store.Create(t.Context(), identity, testAnalysisLimits(), "objective", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	variants := map[string]domain.ResultScope{
		"actor":        {Actor: "someone-else", TeamID: scope.TeamID, ConversationKey: scope.ConversationKey, Project: scope.Project},
		"team":         {Actor: scope.Actor, TeamID: "T2", ConversationKey: scope.ConversationKey, Project: scope.Project},
		"conversation": {Actor: scope.Actor, TeamID: scope.TeamID, ConversationKey: "slack:T1:dm:U2", Project: scope.Project},
		"project":      {Actor: scope.Actor, TeamID: scope.TeamID, ConversationKey: scope.ConversationKey, Project: "other-project"},
	}
	for name, wrongScope := range variants {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get(t.Context(), created.AnalysisID, wrongScope)
			if !errors.Is(err, domain.ErrAnalysisUnavailable) {
				t.Fatalf("cross-scope get on %s = %v, want ErrAnalysisUnavailable", name, err)
			}
		})
	}

	missingErr := func() error { _, err := store.Get(t.Context(), strings.Repeat("f", 64), scope); return err }()
	for name, wrongScope := range variants {
		crossErr := func() error { _, err := store.Get(t.Context(), created.AnalysisID, wrongScope); return err }()
		if crossErr.Error() != missingErr.Error() {
			t.Fatalf("cross-scope error for %s (%v) is distinguishable from a missing row (%v)", name, crossErr, missingErr)
		}
	}
}

func TestAnalysisStoreCompleteAndFailRejectSecondTerminal(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	created, err := store.Create(t.Context(), identity, testAnalysisLimits(), "objective", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	completed, err := store.Complete(t.Context(), created.AnalysisID, scope, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.State != domain.AnalysisCompleted {
		t.Fatalf("state after complete = %q, want completed", completed.State)
	}

	if _, err := store.Complete(t.Context(), created.AnalysisID, scope, now.Add(2*time.Minute)); !errors.Is(err, domain.ErrAnalysisUnavailable) {
		t.Fatalf("second complete = %v, want ErrAnalysisUnavailable", err)
	}
	if _, err := store.Fail(t.Context(), created.AnalysisID, scope, domain.AnalysisFailureWallTimeExceeded, now.Add(2*time.Minute)); !errors.Is(err, domain.ErrAnalysisUnavailable) {
		t.Fatalf("fail after complete = %v, want ErrAnalysisUnavailable", err)
	}
}

// FIND-103 regression: limits_json must be a reconstructible snapshot of
// the actual limits values, not only their digest, so a resumed analysis
// can recover the exact bounds it ran under.
func TestAnalysisStoreLimitsJSONReconstructsStoredDigest(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	limits := testAnalysisLimits()
	scope := testAnalysisScope()
	now := time.Now().UTC()

	created, err := store.Create(t.Context(), identity, limits, "objective", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var limitsJSON, storedDigest string
	if err := store.db.QueryRowContext(t.Context(), `SELECT limits_json, limits_digest FROM result_analyses WHERE analysis_id = ?`,
		created.AnalysisID).Scan(&limitsJSON, &storedDigest); err != nil {
		t.Fatal(err)
	}
	var reconstructed domain.AnalysisLimits
	if err := json.Unmarshal([]byte(limitsJSON), &reconstructed); err != nil {
		t.Fatalf("limits_json did not round-trip as JSON: %v", err)
	}
	if reconstructed != limits {
		t.Fatalf("reconstructed limits = %+v, want %+v", reconstructed, limits)
	}
	if reconstructed.Digest() != storedDigest {
		t.Fatalf("reconstructed limits digest = %q, want stored limits_digest %q", reconstructed.Digest(), storedDigest)
	}
}

func TestAnalysisStoreCreateRejectsMismatchedLimitsDigest(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	scope := testAnalysisScope()
	limits := testAnalysisLimits()
	limits.MaxLeaves = limits.MaxLeaves + 1 // no longer matches identity.LimitsDigest

	if _, err := store.Create(t.Context(), identity, limits, "objective", scope, "ws-1", time.Now().UTC()); !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("create with mismatched limits digest = %v, want ErrAnalysisValidation", err)
	}
}

func TestAnalysisStoreFailRecordsClosedFailureCode(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	created, err := store.Create(t.Context(), identity, testAnalysisLimits(), "objective", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	failed, err := store.Fail(t.Context(), created.AnalysisID, scope, domain.AnalysisFailureSourceTooLarge, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if failed.State != domain.AnalysisFailed || failed.FailureCode != domain.AnalysisFailureSourceTooLarge {
		t.Fatalf("failed record = state %q code %q, want failed/%q", failed.State, failed.FailureCode, domain.AnalysisFailureSourceTooLarge)
	}
}

// TestAnalysisStoreListByWorkstreamFiltersByWorkstreamAndScope is part of
// checkpoint 5 item 3 (dependent dispatch blocking): it proves
// ListByWorkstream returns only the rows for the requested workstream and
// exact actor/conversation/project scope, so workstream.Service's dispatch
// gate can never see an analysis belonging to a different workstream or a
// different owner.
func TestAnalysisStoreListByWorkstreamFiltersByWorkstreamAndScope(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	identityA := testAnalysisIdentity(sourceID, sourceSHA)
	createdA, err := store.Create(t.Context(), identityA, testAnalysisLimits(), "objective A", scope, "ws-1", now)
	if err != nil {
		t.Fatalf("create ws-1 analysis: %v", err)
	}
	identityB := identityA
	identityB.ObjectiveDigest = strings.Repeat("d", 64)
	if _, err := store.Create(t.Context(), identityB, testAnalysisLimits(), "objective B", scope, "ws-2", now); err != nil {
		t.Fatalf("create ws-2 analysis: %v", err)
	}
	otherScope := domain.ResultScope{Actor: "someone-else", TeamID: scope.TeamID, ConversationKey: "slack:T1:dm:U2", Project: scope.Project}
	identityC := identityA
	identityC.ObjectiveDigest = strings.Repeat("e", 64)
	if _, err := store.Create(t.Context(), identityC, testAnalysisLimits(), "objective C", otherScope, "ws-1", now); err != nil {
		t.Fatalf("create cross-scope ws-1 analysis: %v", err)
	}

	records, err := store.ListByWorkstream(t.Context(), "ws-1", scope.Actor, domain.ConversationKey(scope.ConversationKey), scope.Project)
	if err != nil {
		t.Fatalf("list by workstream: %v", err)
	}
	if len(records) != 1 || records[0].AnalysisID != createdA.AnalysisID {
		t.Fatalf("list by workstream = %+v, want exactly [%s]", records, createdA.AnalysisID)
	}
}

// TestAnalysisStoreListByWorkstreamRejectsTeamMismatch is FIND-106's hostile
// test: it proves the filter enforces team_id as an exact predicate rather
// than trusting the convention that a canonical conversation key already
// encodes the team. The host cannot produce this row through Create today
// (a conversation key always carries the team it was minted for), so the
// test forges it directly with SQL, exactly the adversarial condition
// FIND-106 says is not exploitable yet but is also not an enforced
// invariant.
func TestAnalysisStoreListByWorkstreamRejectsTeamMismatch(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	scope := testAnalysisScope()
	now := time.Now().UTC()

	// team_id is part of the analysis identity and protected by the v40
	// immutability trigger, so a mismatch cannot be forged with an UPDATE
	// after Create. It is forged with a direct INSERT instead, which is
	// exactly the point: the host itself never produces this row (Create
	// always writes scope.TeamID verbatim), so this row can only exist as
	// an adversarial or corrupted one, and the filter must still exclude it.
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'preparing', ?, ?)`,
		strings.Repeat("f", 64), sourceID, sourceSHA, int64(1024), "bounded_question_v1", strings.Repeat("9", 64),
		"objective A", "text_v1", "v1", "fp-1", strings.Repeat("1", 64), `{"limits_digest":"`+strings.Repeat("1", 64)+`"}`,
		scope.Actor, "T-OTHER", scope.ConversationKey, scope.Project, "ws-1", now.UTC().Unix(), now.UTC().Unix()); err != nil {
		t.Fatalf("forge team mismatch row: %v", err)
	}

	records, err := store.ListByWorkstream(t.Context(), "ws-1", scope.Actor, domain.ConversationKey(scope.ConversationKey), scope.Project)
	if err != nil {
		t.Fatalf("list by workstream: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("list by workstream = %+v, want none: conversation key encodes team %q but the row's team_id is T-OTHER", records, scope.TeamID)
	}
}
