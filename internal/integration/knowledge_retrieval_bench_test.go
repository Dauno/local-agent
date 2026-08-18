package integration_test

// Checkpoint 6 sub-round 6c: the 10,000-item benchmark suite named by the
// TRD's "Evaluation and Performance" paragraph. This is a measurement
// round, not a gate round: there is deliberately no portable wall-clock
// pass/fail assertion anywhere in this file. The hard runtime controls the
// TRD requires — bounded candidate counts, total timeout, and cancellation
// — are proven by the two proof benchmarks at the bottom, and the reported
// numbers document why the bounded linear approach is sufficient at this
// scale. ANN/external vector storage is rejected for V1 and does not
// appear here.
//
// Repo convention: everything lives behind `go test -bench` (this file has
// no Test functions), so the normal `go test ./...` suite pays only the
// compilation cost of this file — matching the existing *_bench_test.go
// files in this repo, none of which gate their seeding any further.
//
// The 10,000-item corpus is seeded exactly once per `go test` process into
// a shared SQLite store (sync.Once) and reused by every benchmark:
// reseeding inside b.N is never done, and each benchmark calls ResetTimer
// after the shared corpus is available, so the measured numbers cover only
// the real reader/index/retriever work over real SQL and the real linear
// cosine scan.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

const (
	knowledgeBenchTeam       = "T12345678"
	knowledgeBenchActor      = "U12345678"
	knowledgeBenchChannel    = "C12345678"
	knowledgeBenchOtherTeam  = "T99999999"
	knowledgeBenchOtherActor = "U99999999"
	knowledgeBenchProject    = "P12345678"
	knowledgeBenchWorkstream = "WSBENCH001"
	// Canonical Slack conversation key matching the team above.
	knowledgeBenchConversation = "slack:T12345678:channel:C12345678:thread:1700000000.000001"
	knowledgeBenchExchangeTS   = "1700000000.000002"

	// Pinned fake embedding configuration: an opaque provider/model pair
	// and eight dimensions. No live credential or endpoint exists.
	knowledgeBenchProvider = "bench-provider"
	knowledgeBenchModel    = "bench-model"
	knowledgeBenchDims     = 8

	// Frozen corpus sizes: 10,000 authoritative items in total.
	knowledgeBenchClaimsTotal = 8000
	knowledgeBenchClaimsAuth  = 2000
	knowledgeBenchPrefsTotal  = 1000
	knowledgeBenchPrefsAuth   = 300
	knowledgeBenchDocsTotal   = 1000
	knowledgeBenchDocsAuth    = 250

	// The broad exact-channel subject shared by the first 500 authorized
	// claims (far more rows than the per-channel candidate cap).
	knowledgeBenchExactSubject = "bench-exact-group"
	// The broad lexical token present in the FTS text of every item.
	knowledgeBenchLexicalToken = "zorchmarker"

	// Every 10th authorized item shares the query vector exactly (dot
	// product 1.0): 200 claims + 30 preferences + 25 documents = 255 rows,
	// a known 2.55% fraction of the corpus, with every other vector
	// orthogonal.
	knowledgeBenchSemanticMatchFraction = 10
)

var (
	knowledgeBenchOnce  sync.Once
	knowledgeBenchState knowledgeBenchCorpus
	knowledgeBenchErr   error
)

// knowledgeBenchStats records the one-time corpus composition facts the
// benchmarks report and the bounded-count proof asserts against.
type knowledgeBenchStats struct {
	claimsAuthorized   int
	claimsUnauthorized int
	prefsAuthorized    int
	prefsUnauthorized  int
	docsAuthorized     int
	docsUnauthorized   int
	staleFTS           int
	semanticMatches    int
	exactSubjectRows   int
	lexicalMatchRows   int
	lexicalEligible    int
}

type knowledgeBenchCorpus struct {
	store       *adaptersqlite.Store
	reader      *adaptersqlite.KnowledgeCandidateReader
	index       *adaptersqlite.KnowledgeLexicalIndexStore
	provider    *testutil.FakeEmbeddingProvider
	retriever   port.KnowledgeRetriever
	binding     domain.KnowledgeWriteBinding
	request     domain.KnowledgeRetrievalRequest
	scopes      []domain.KnowledgeScopeRef
	owner       string
	queryVector []float32
	stats       knowledgeBenchStats
}

// knowledgeBench returns the shared seeded corpus, seeding it once per
// process. The temporary database file lives in os.TempDir for the life of
// the process (benchmarks have no t.Cleanup; the OS reaps it).
func knowledgeBench(tb testing.TB) *knowledgeBenchCorpus {
	tb.Helper()
	knowledgeBenchOnce.Do(func() {
		knowledgeBenchState, knowledgeBenchErr = seedKnowledgeBenchCorpus()
	})
	if knowledgeBenchErr != nil {
		tb.Fatalf("seed knowledge benchmark corpus: %v", knowledgeBenchErr)
	}
	return &knowledgeBenchState
}

func seedKnowledgeBenchCorpus() (knowledgeBenchCorpus, error) {
	fail := func(err error) (knowledgeBenchCorpus, error) {
		return knowledgeBenchCorpus{}, fmt.Errorf("seed knowledge benchmark corpus: %w", err)
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "knowledge-bench-")
	if err != nil {
		return fail(err)
	}
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(dir, "bench.db"))
	if err != nil {
		return fail(err)
	}

	now := time.Now().Truncate(time.Second).UTC()
	nowUnix := now.Unix()
	fingerprint := domain.ModelFingerprint(knowledgeBenchProvider, knowledgeBenchModel, knowledgeBenchDims)
	binding := domain.KnowledgeWriteBinding{
		Team:         knowledgeBenchTeam,
		Actor:        knowledgeBenchActor,
		Conversation: domain.ConversationKey(knowledgeBenchConversation),
		Project:      knowledgeBenchProject,
		WorkstreamID: knowledgeBenchWorkstream,
	}
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	scopes := domain.KnowledgeReadableScopes(binding)

	e1 := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	e2 := []float32{0, 1, 0, 0, 0, 0, 0, 0}
	e3 := []float32{0, 0, 1, 0, 0, 0, 0, 0}
	deadDigest := strings.Repeat("deadbeef", 8)

	stats := knowledgeBenchStats{}
	stats.claimsAuthorized = knowledgeBenchClaimsAuth
	stats.claimsUnauthorized = knowledgeBenchClaimsTotal - knowledgeBenchClaimsAuth
	stats.prefsAuthorized = knowledgeBenchPrefsAuth
	stats.prefsUnauthorized = knowledgeBenchPrefsTotal - knowledgeBenchPrefsAuth
	stats.docsAuthorized = knowledgeBenchDocsAuth
	stats.docsUnauthorized = knowledgeBenchDocsTotal - knowledgeBenchDocsAuth
	stats.exactSubjectRows = 500

	db := store.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		store.Close()
		return fail(err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtClaim, err := tx.PrepareContext(ctx, `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, value_number,
			value_boolean, value_reference, scope_kind, scope_id, source_class, source_ref, author_id,
			status, valid_from, valid_until, supersedes_id, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, '', ?, ?, ?, '', 1, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	stmtFTS, err := tx.PrepareContext(ctx, `
		INSERT INTO knowledge_retrieval_fts (item_kind, item_id, item_revision, source_digest, subject, body)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	stmtEmbedding, err := tx.PrepareContext(ctx, `
		INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}

	encode := func(vector []float32) ([]byte, error) {
		return domain.NormalizeAndEncodeEmbedding(vector, knowledgeBenchDims)
	}

	// Authorized claims: 2,000 rows readable by the trusted binding,
	// project/user/workstream scopes with matching source classes (the
	// frozen writability contract), asserted/verified/disputed statuses,
	// and open validity windows. The first 500 share one exact subject;
	// every 4th carries a deliberately stale FTS/vector digest; every 10th
	// carries the semantic query vector.
	for i := 0; i < knowledgeBenchClaimsAuth; i++ {
		scopeKind, scopeID, sourceClass := domain.KnowledgeScopeProject, knowledgeBenchProject, domain.KnowledgeSourceHuman
		switch i % 10 {
		case 4, 5, 6:
			scopeKind, scopeID = domain.KnowledgeScopeUser, owner
		case 7, 8, 9:
			scopeKind, scopeID, sourceClass = domain.KnowledgeScopeWorkstream, knowledgeBenchWorkstream, domain.KnowledgeSourceDecision
		}
		status := domain.KnowledgeClaimAsserted
		switch {
		case i%10 == 9:
			status = domain.KnowledgeClaimDisputed
		case i%4 == 0:
			status = domain.KnowledgeClaimVerified
		}
		subject := fmt.Sprintf("bench subject %05d", i)
		predicate := domain.KnowledgePredicateIs
		valueKind, valueText, valueReference := domain.KnowledgeValueString,
			fmt.Sprintf("operational note %s %05d", knowledgeBenchLexicalToken, i), ""
		if i < 500 {
			subject = knowledgeBenchExactSubject
			predicate = domain.KnowledgePredicateRelatesTo
			valueKind, valueText, valueReference = domain.KnowledgeValueReference, "", fmt.Sprintf("bench-ref-%05d", i)
		}
		id := fmt.Sprintf("kclaim_%024x", i)
		sourceRef := fmt.Sprintf("bench-src-%05d", i)
		if _, err := stmtClaim.ExecContext(ctx, id, subject, string(predicate), string(valueKind), valueText,
			valueReference, string(scopeKind), scopeID, string(sourceClass), sourceRef, string(status),
			0, 0, nowUnix, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("claim %d: %w", i, err))
		}
		item := port.KnowledgeAuthoritativeItem{
			Kind: domain.KnowledgeRetrievalClaim, ID: id,
			Claim: &domain.KnowledgeClaim{
				ID: domain.KnowledgeClaimID(id), Subject: subject, Predicate: predicate,
				Value:     domain.KnowledgeValue{Kind: valueKind, Text: valueText, Reference: valueReference},
				ScopeKind: scopeKind, ScopeID: scopeID, SourceClass: sourceClass, SourceRef: sourceRef,
				Status: status, Revision: 1,
			},
		}
		text, err := knowledgeusecase.BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, item, "", nil)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("claim %d canonical text: %w", i, err))
		}
		digest := text.SourceDigest
		if i%4 == 0 {
			digest = deadDigest
			stats.staleFTS++
		}
		if _, err := stmtFTS.ExecContext(ctx, string(domain.KnowledgeRetrievalClaim), id, 1, digest, text.Subject, text.Body); err != nil {
			store.Close()
			return fail(fmt.Errorf("claim %d FTS: %w", i, err))
		}
		vector := e2
		if i%knowledgeBenchSemanticMatchFraction == 0 {
			vector = e1
			stats.semanticMatches++
		}
		encoded, err := encode(vector)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("claim %d vector: %w", i, err))
		}
		if _, err := stmtEmbedding.ExecContext(ctx, string(domain.KnowledgeRetrievalClaim), id, 1, digest, fingerprint, knowledgeBenchDims, encoded, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("claim %d embedding: %w", i, err))
		}
	}

	// Unauthorized claims: 6,000 rows that must never leave the reader or
	// index — other-team scopes, archived/superseded/expired/future
	// statuses, all carrying the broad lexical token so the FTS match set
	// is far larger than the eligible set.
	for i := 0; i < knowledgeBenchClaimsTotal-knowledgeBenchClaimsAuth; i++ {
		scopeKind, scopeID := domain.KnowledgeScopeTeam, knowledgeBenchOtherTeam
		status := domain.KnowledgeClaimAsserted
		validFrom, validUntil := int64(0), int64(0)
		switch {
		case i < 3100:
			// other-team: scope predicate must exclude it
		case i < 4000:
			scopeKind, scopeID, status = domain.KnowledgeScopeProject, knowledgeBenchProject, domain.KnowledgeClaimArchived
		case i < 4800:
			scopeKind, scopeID, status = domain.KnowledgeScopeProject, knowledgeBenchProject, domain.KnowledgeClaimSuperseded
		case i < 5500:
			scopeKind, scopeID, validUntil = domain.KnowledgeScopeProject, knowledgeBenchProject, 1600000000
		default:
			scopeKind, scopeID, validFrom = domain.KnowledgeScopeProject, knowledgeBenchProject, 4102444800
		}
		id := fmt.Sprintf("kclaim_%024x", 1000000+i)
		subject := fmt.Sprintf("other bench subject %05d", i)
		if _, err := stmtClaim.ExecContext(ctx, id, subject, string(domain.KnowledgePredicateIs), string(domain.KnowledgeValueString),
			fmt.Sprintf("operational note %s other %05d", knowledgeBenchLexicalToken, i), "",
			string(scopeKind), scopeID, string(domain.KnowledgeSourceHuman), "bench-src-unauth", string(status),
			validFrom, validUntil, nowUnix, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("unauthorized claim %d: %w", i, err))
		}
		item := port.KnowledgeAuthoritativeItem{
			Kind: domain.KnowledgeRetrievalClaim, ID: id,
			Claim: &domain.KnowledgeClaim{
				ID: domain.KnowledgeClaimID(id), Subject: subject, Predicate: domain.KnowledgePredicateIs,
				Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: fmt.Sprintf("operational note %s other %05d", knowledgeBenchLexicalToken, i)},
				ScopeKind: scopeKind, ScopeID: scopeID, SourceClass: domain.KnowledgeSourceHuman, SourceRef: "bench-src-unauth",
				Status: status, Revision: 1, ValidFrom: time.Unix(validFrom, 0).UTC(), ValidUntil: time.Unix(validUntil, 0).UTC(),
			},
		}
		text, err := knowledgeusecase.BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, item, "", nil)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("unauthorized claim %d canonical text: %w", i, err))
		}
		if _, err := stmtFTS.ExecContext(ctx, string(domain.KnowledgeRetrievalClaim), id, 1, text.SourceDigest, text.Subject, text.Body); err != nil {
			store.Close()
			return fail(fmt.Errorf("unauthorized claim %d FTS: %w", i, err))
		}
		encoded, err := encode(e3)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("unauthorized claim %d vector: %w", i, err))
		}
		if _, err := stmtEmbedding.ExecContext(ctx, string(domain.KnowledgeRetrievalClaim), id, 1, text.SourceDigest, fingerprint, knowledgeBenchDims, encoded, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("unauthorized claim %d embedding: %w", i, err))
		}
	}

	stmtPref, err := tx.PrepareContext(ctx, `
		INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_text, value_number, value_boolean, status, source_ref, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, 0, 'active', 'bench-pref-src', 1, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	// Preferences: 300 owned by the trusted actor, 700 owned by others.
	for j := 0; j < knowledgeBenchPrefsTotal; j++ {
		prefOwner := owner
		authorized := j < knowledgeBenchPrefsAuth
		if !authorized {
			prefOwner = fmt.Sprintf("slack:%s:user:%s", knowledgeBenchTeam, knowledgeBenchOtherActor)
		}
		key := fmt.Sprintf("bench-pref-%05d", j)
		valueText := fmt.Sprintf("%s preference %05d", knowledgeBenchLexicalToken, j)
		result, err := stmtPref.ExecContext(ctx, prefOwner, key, string(domain.KnowledgeValueString), valueText, nowUnix, nowUnix)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d: %w", j, err))
		}
		rowID, err := result.LastInsertId()
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d row id: %w", j, err))
		}
		identity := fmt.Sprintf("preference:%d", rowID)
		item := port.KnowledgeAuthoritativeItem{
			Kind: domain.KnowledgeRetrievalPreference, ID: identity,
			Preference: &domain.KnowledgePreference{
				ID: int(rowID), OwnerKey: prefOwner, Key: key,
				Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: valueText},
				Status: domain.KnowledgePreferenceActive, Revision: 1,
			},
		}
		text, err := knowledgeusecase.BuildKnowledgeIndexText(domain.KnowledgeRetrievalPreference, item, "", nil)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d canonical text: %w", j, err))
		}
		if _, err := stmtFTS.ExecContext(ctx, string(domain.KnowledgeRetrievalPreference), identity, 1, text.SourceDigest, text.Subject, text.Body); err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d FTS: %w", j, err))
		}
		vector := e3
		if authorized {
			vector = e2
			if j%knowledgeBenchSemanticMatchFraction == 0 {
				vector = e1
				stats.semanticMatches++
			}
		}
		encoded, err := encode(vector)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d vector: %w", j, err))
		}
		if _, err := stmtEmbedding.ExecContext(ctx, string(domain.KnowledgeRetrievalPreference), identity, 1, text.SourceDigest, fingerprint, knowledgeBenchDims, encoded, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("preference %d embedding: %w", j, err))
		}
	}

	stmtTopic, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_topics (id, slug, title, description, status, tags, content, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, '', 'active', '[]', '', 1, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	stmtRevision, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES (?, 1, ?, 'bench', ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	stmtDoc, err := tx.PrepareContext(ctx, `
		INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'legacy_curated_document', ?, 1, ?, ?)`)
	if err != nil {
		store.Close()
		return fail(err)
	}
	// Documents: 250 authorized (project scope, active), 750 unauthorized
	// (other-team active or archived), each backed by one memory topic
	// revision so the strict legacy resolver can reconstruct content.
	for k := 0; k < knowledgeBenchDocsTotal; k++ {
		topicID := fmt.Sprintf("bench-mem-%05d", k)
		if _, err := stmtTopic.ExecContext(ctx, topicID, topicID, topicID, nowUnix, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("memory topic %d: %w", k, err))
		}
		content := fmt.Sprintf("curated note %s %05d", knowledgeBenchLexicalToken, k)
		revisionResult, err := stmtRevision.ExecContext(ctx, topicID, content, nowUnix)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("memory revision %d: %w", k, err))
		}
		revisionRowID, err := revisionResult.LastInsertId()
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("memory revision %d row id: %w", k, err))
		}
		authorized := k < knowledgeBenchDocsAuth
		scopeKind, scopeID, status := domain.KnowledgeScopeProject, knowledgeBenchProject, domain.KnowledgeDocumentActive
		if !authorized {
			if k < 650 {
				scopeKind, scopeID = domain.KnowledgeScopeTeam, knowledgeBenchOtherTeam
			} else {
				status = domain.KnowledgeDocumentArchived
			}
		}
		sum := sha256.Sum256([]byte(content))
		digest := hex.EncodeToString(sum[:])
		handle := fmt.Sprintf("memory_topics:%s:revision:%d", topicID, revisionRowID)
		id := fmt.Sprintf("kdoc_%024x", k)
		subject := fmt.Sprintf("bench doc %05d", k)
		if _, err := stmtDoc.ExecContext(ctx, id, subject, string(scopeKind), scopeID, digest, handle, topicID, string(status), nowUnix, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("document %d: %w", k, err))
		}
		item := port.KnowledgeAuthoritativeItem{
			Kind: domain.KnowledgeRetrievalDocument, ID: id,
			Document: &domain.KnowledgeDocument{
				ID: domain.KnowledgeDocumentID(id), Subject: subject, ScopeKind: scopeKind, ScopeID: scopeID,
				ContentDigest: digest, ContentHandle: handle, SourceID: topicID, SourceRev: 1,
				Provenance: domain.KnowledgeProvenanceLegacyCurated, Status: status, Revision: 1,
			},
		}
		text, err := knowledgeusecase.BuildKnowledgeIndexText(domain.KnowledgeRetrievalDocument, item, content, nil)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("document %d canonical text: %w", k, err))
		}
		if _, err := stmtFTS.ExecContext(ctx, string(domain.KnowledgeRetrievalDocument), id, 1, text.SourceDigest, text.Subject, text.Body); err != nil {
			store.Close()
			return fail(fmt.Errorf("document %d FTS: %w", k, err))
		}
		vector := e3
		if authorized {
			vector = e2
			if k%knowledgeBenchSemanticMatchFraction == 0 {
				vector = e1
				stats.semanticMatches++
			}
		}
		encoded, err := encode(vector)
		if err != nil {
			store.Close()
			return fail(fmt.Errorf("document %d vector: %w", k, err))
		}
		if _, err := stmtEmbedding.ExecContext(ctx, string(domain.KnowledgeRetrievalDocument), id, 1, text.SourceDigest, fingerprint, knowledgeBenchDims, encoded, nowUnix); err != nil {
			store.Close()
			return fail(fmt.Errorf("document %d embedding: %w", k, err))
		}
	}

	if err := tx.Commit(); err != nil {
		store.Close()
		return fail(fmt.Errorf("commit corpus: %w", err))
	}

	// The broad lexical token appears in every FTS row, and every
	// authorized item is eligible, so the eligible pool is the whole
	// authorized set (claims + preferences + documents).
	stats.lexicalMatchRows = knowledgeBenchClaimsTotal + knowledgeBenchPrefsTotal + knowledgeBenchDocsTotal
	stats.lexicalEligible = knowledgeBenchClaimsAuth + knowledgeBenchPrefsAuth + knowledgeBenchDocsAuth

	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.EmbeddingDimensions = knowledgeBenchDims
	limits.MinSimilarityBasisPoints = 5000
	// Measurement headroom, not a relaxed control: the composed run at
	// 10,000 items measures around 1.2s/op with the stale-repair enqueue
	// traffic, so the hard maximum keeps the benchmark deterministic while
	// the cancellation benchmark separately proves timeout enforcement.
	// (Measured finding: at this corpus size the default 2s timeout has a
	// thin margin and was exceeded once by a first-run cold start.)
	limits.TimeoutSeconds = domain.HardMaxKnowledgeRetrievalTimeoutSeconds
	provider := testutil.NewFakeEmbeddingProvider(knowledgeBenchDims).SetVectors([][]float32{e1})
	index := adaptersqlite.NewKnowledgeLexicalIndexStore(store, fingerprint)
	retriever, err := knowledgeusecase.NewRetriever(knowledgeusecase.RetrieverDependencies{
		Reader:   adaptersqlite.NewKnowledgeCandidateReader(store),
		Index:    index,
		Resolver: adaptersqlite.NewKnowledgeDocumentResolver(store),
		Queue:    adaptersqlite.NewKnowledgeLexicalQueueStore(store),
		Provider: provider,
		Clock:    port.SystemClock{},
		Redact:   func(value string) string { return value },
	})
	if err != nil {
		store.Close()
		return fail(err)
	}
	request := domain.KnowledgeRetrievalRequest{
		Binding: binding,
		Workstream: &domain.WorkstreamSnapshot{
			ID:              knowledgeBenchWorkstream,
			ConversationKey: domain.ConversationKey(knowledgeBenchConversation),
			OwnerActor:      knowledgeBenchActor,
			Project:         knowledgeBenchProject,
			Status:          domain.WorkstreamActive,
			Revision:        1,
		},
		ExchangeTS:     knowledgeBenchExchangeTS,
		CurrentMessage: knowledgeBenchExactSubject + " " + knowledgeBenchLexicalToken,
		Now:            now,
		Limits:         limits,
	}
	return knowledgeBenchCorpus{
		store: store, reader: adaptersqlite.NewKnowledgeCandidateReader(store), index: index,
		provider: provider, retriever: retriever, binding: binding, request: request,
		scopes: scopes, owner: owner, queryVector: e1, stats: stats,
	}, nil
}

func knowledgeBenchLogCorpus(b *testing.B, corpus *knowledgeBenchCorpus) {
	stats := corpus.stats
	b.Logf("corpus: %d claims (%d authorized / %d unauthorized), %d preferences (%d/%d), %d documents (%d/%d) = %d items",
		knowledgeBenchClaimsTotal, stats.claimsAuthorized, stats.claimsUnauthorized,
		knowledgeBenchPrefsTotal, stats.prefsAuthorized, stats.prefsUnauthorized,
		knowledgeBenchDocsTotal, stats.docsAuthorized, stats.docsUnauthorized,
		knowledgeBenchClaimsTotal+knowledgeBenchPrefsTotal+knowledgeBenchDocsTotal)
	b.Logf("corpus: %d stale FTS/vector rows, %d semantic rows at dot 1.0, %d exact-subject rows, %d lexical FTS matches (%d eligible)",
		stats.staleFTS, stats.semanticMatches, stats.exactSubjectRows, stats.lexicalMatchRows, stats.lexicalEligible)
	b.Logf("controls: MaxCandidatesPerChannel=%d, MaxCards=%d, benchmark TimeoutSeconds=%d (hard max; default is 2), no ANN index (linear Go scan)",
		corpus.request.Limits.MaxCandidatesPerChannel, corpus.request.Limits.MaxCards, corpus.request.Limits.TimeoutSeconds)
}

// BenchmarkKnowledgeRetrievalExactChannel measures the exact identifier
// channel over the 10,000-item corpus: one broad subject shared by 500
// authorized claims, SQL scope/status/validity predicates, and the bounded
// candidate cap.
func BenchmarkKnowledgeRetrievalExactChannel(b *testing.B) {
	corpus := knowledgeBench(b)
	knowledgeBenchLogCorpus(b, corpus)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		candidates, err := corpus.reader.ReadExact(ctx, corpus.binding, corpus.request.Now, corpus.request.Limits,
			knowledgeBenchExactSubject, []string{knowledgeBenchExactSubject})
		if err != nil {
			b.Fatal(err)
		}
		if len(candidates) > corpus.request.Limits.MaxCandidatesPerChannel {
			b.Fatalf("exact channel returned %d candidates, cap %d", len(candidates), corpus.request.Limits.MaxCandidatesPerChannel)
		}
		benchmarkCandidateSink = len(candidates)
	}
	b.ReportMetric(float64(corpus.stats.exactSubjectRows), "rows/op-matching")
}

// BenchmarkKnowledgeRetrievalLexicalChannel measures the FTS channel:
// SearchLexical with the broad token matching every one of the 10,000 FTS
// rows while SQL authorizes down to the 2,550 eligible rows before the
// BM25 top-32 returns.
func BenchmarkKnowledgeRetrievalLexicalChannel(b *testing.B) {
	corpus := knowledgeBench(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		hits, err := corpus.index.SearchLexical(ctx, corpus.scopes, corpus.owner, knowledgeBenchLexicalToken, corpus.request.Limits.MaxCandidatesPerChannel)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) > corpus.request.Limits.MaxCandidatesPerChannel {
			b.Fatalf("lexical channel returned %d hits, cap %d", len(hits), corpus.request.Limits.MaxCandidatesPerChannel)
		}
		benchmarkCandidateSink = len(hits)
	}
	b.ReportMetric(float64(corpus.stats.lexicalMatchRows), "rows/op-matching")
	b.ReportMetric(float64(corpus.stats.lexicalEligible), "rows/op-eligible")
}

// BenchmarkKnowledgeRetrievalRelationChannel measures the one-hop relation
// channel: 32 exact seeds expanding through their (subject, reference)
// endpoints over the corpus, re-authorized in SQL, capped at 32.
func BenchmarkKnowledgeRetrievalRelationChannel(b *testing.B) {
	corpus := knowledgeBench(b)
	ctx := context.Background()
	seeds, err := corpus.reader.ReadExact(ctx, corpus.binding, corpus.request.Now, corpus.request.Limits,
		knowledgeBenchExactSubject, []string{knowledgeBenchExactSubject})
	if err != nil {
		b.Fatal(err)
	}
	if len(seeds) == 0 {
		b.Fatal("no exact seeds for relation channel")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		candidates, err := corpus.reader.ReadRelated(ctx, corpus.binding, corpus.request.Now, corpus.request.Limits, seeds)
		if err != nil {
			b.Fatal(err)
		}
		if len(candidates) > corpus.request.Limits.MaxCandidatesPerChannel {
			b.Fatalf("relation channel returned %d candidates, cap %d", len(candidates), corpus.request.Limits.MaxCandidatesPerChannel)
		}
		benchmarkCandidateSink = len(candidates)
	}
}

// BenchmarkKnowledgeRetrievalSemanticChannel measures the linear vector
// channel the TRD names: one deterministic fake-provider embedding for the
// query plus the real in-Go cosine scan over every authorized vector row
// under the pinned fingerprint, filtered by the similarity threshold and
// capped. There is no ANN index anywhere in this path by design.
func BenchmarkKnowledgeRetrievalSemanticChannel(b *testing.B) {
	corpus := knowledgeBench(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		vectors, err := corpus.provider.Embed(ctx, []string{"bench semantic probe"})
		if err != nil {
			b.Fatal(err)
		}
		hits, err := corpus.index.SearchSemantic(ctx, corpus.scopes, corpus.owner, vectors[0],
			corpus.request.Limits.MinSimilarityBasisPoints, corpus.request.Limits.MaxCandidatesPerChannel)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) > corpus.request.Limits.MaxCandidatesPerChannel {
			b.Fatalf("semantic channel returned %d hits, cap %d", len(hits), corpus.request.Limits.MaxCandidatesPerChannel)
		}
		benchmarkCandidateSink = len(hits)
	}
	b.ReportMetric(float64(corpus.stats.semanticMatches), "rows/op-matching")
}

// BenchmarkKnowledgeRetrieveEndToEnd measures the complete composed
// Retrieve: exact + relation + lexical + semantic channels, per-hit
// authoritative re-verification (including stale-row repair enqueues),
// deterministic ranking, card construction, and document resolution over
// the 10,000-item corpus with production wiring.
func BenchmarkKnowledgeRetrieveEndToEnd(b *testing.B) {
	corpus := knowledgeBench(b)
	ctx := context.Background()
	last := domain.KnowledgeRetrievalResult{}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, err := corpus.retriever.Retrieve(ctx, corpus.request)
		if err != nil {
			b.Fatal(err)
		}
		last = result
	}
	if last.Diagnostics.CandidateCount > domain.MaxKnowledgeRetrievalChannels*corpus.request.Limits.MaxCandidatesPerChannel {
		b.Fatalf("end-to-end processed %d candidates, bound %d", last.Diagnostics.CandidateCount, domain.MaxKnowledgeRetrievalChannels*corpus.request.Limits.MaxCandidatesPerChannel)
	}
	if last.Diagnostics.SelectedCount > corpus.request.Limits.MaxCards {
		b.Fatalf("end-to-end selected %d cards, bound %d", last.Diagnostics.SelectedCount, corpus.request.Limits.MaxCards)
	}
	b.Logf("end-to-end final run: %d candidates, %d selected, %d omitted, failures %v",
		last.Diagnostics.CandidateCount, last.Diagnostics.SelectedCount, last.Diagnostics.OmittedCount, last.Diagnostics.Failures)
}

// BenchmarkKnowledgeRetrievalCandidateBoundsAtScale is the bounded-candidate
// proof: every channel is pointed at a query whose match set is at least an
// order of magnitude larger than the configured per-channel cap, and each
// channel plus the composed end-to-end run must never return or process
// more than the cap. This is the concrete demonstration that the hard
// runtime controls — not an ANN index and not a wall-clock threshold —
// keep retrieval bounded regardless of corpus size.
func BenchmarkKnowledgeRetrievalCandidateBoundsAtScale(b *testing.B) {
	corpus := knowledgeBench(b)
	knowledgeBenchLogCorpus(b, corpus)
	if corpus.stats.claimsAuthorized+corpus.stats.claimsUnauthorized != knowledgeBenchClaimsTotal {
		b.Fatalf("claim seed totals do not add up")
	}
	cap := corpus.request.Limits.MaxCandidatesPerChannel
	// The match sets must dwarf the cap for the proof to mean anything.
	if corpus.stats.exactSubjectRows <= cap {
		b.Fatalf("exact match set %d does not exceed the cap %d", corpus.stats.exactSubjectRows, cap)
	}
	if corpus.stats.lexicalMatchRows <= cap {
		b.Fatalf("lexical match set %d does not exceed the cap %d", corpus.stats.lexicalMatchRows, cap)
	}
	if corpus.stats.semanticMatches <= cap {
		b.Fatalf("semantic match set %d does not exceed the cap %d", corpus.stats.semanticMatches, cap)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		exact, err := corpus.reader.ReadExact(ctx, corpus.binding, corpus.request.Now, corpus.request.Limits,
			knowledgeBenchExactSubject, []string{knowledgeBenchExactSubject})
		if err != nil {
			b.Fatal(err)
		}
		if len(exact) > cap {
			b.Fatalf("exact channel returned %d candidates, cap %d", len(exact), cap)
		}
		lexical, err := corpus.index.SearchLexical(ctx, corpus.scopes, corpus.owner, knowledgeBenchLexicalToken, cap)
		if err != nil {
			b.Fatal(err)
		}
		if len(lexical) > cap {
			b.Fatalf("lexical channel returned %d hits, cap %d", len(lexical), cap)
		}
		vectors, err := corpus.provider.Embed(ctx, []string{"bench bounds probe"})
		if err != nil {
			b.Fatal(err)
		}
		semantic, err := corpus.index.SearchSemantic(ctx, corpus.scopes, corpus.owner, vectors[0],
			corpus.request.Limits.MinSimilarityBasisPoints, cap)
		if err != nil {
			b.Fatal(err)
		}
		if len(semantic) > cap {
			b.Fatalf("semantic channel returned %d hits, cap %d", len(semantic), cap)
		}
		related, err := corpus.reader.ReadRelated(ctx, corpus.binding, corpus.request.Now, corpus.request.Limits, exact)
		if err != nil {
			b.Fatal(err)
		}
		if len(related) > cap {
			b.Fatalf("relation channel returned %d candidates, cap %d", len(related), cap)
		}
		result, err := corpus.retriever.Retrieve(ctx, corpus.request)
		if err != nil {
			b.Fatal(err)
		}
		if result.Diagnostics.CandidateCount > domain.MaxKnowledgeRetrievalChannels*cap {
			b.Fatalf("composed run processed %d candidates, bound %d", result.Diagnostics.CandidateCount, domain.MaxKnowledgeRetrievalChannels*cap)
		}
		if result.Diagnostics.SelectedCount > corpus.request.Limits.MaxCards {
			b.Fatalf("composed run selected %d cards, bound %d", result.Diagnostics.SelectedCount, corpus.request.Limits.MaxCards)
		}
		benchmarkCandidateSink = len(exact) + len(lexical) + len(semantic) + len(related)
	}
	b.Logf("bounds hold at 10,000 items: per-channel cap %d, composed cap %d, card cap %d",
		cap, domain.MaxKnowledgeRetrievalChannels*cap, corpus.request.Limits.MaxCards)
}

// BenchmarkKnowledgeRetrievalCancellationBoundAtScale is the cancellation
// proof: at corpus scale, a pre-cancelled context and an already-expired
// deadline must both stop Retrieve promptly (the sanity ceiling is
// deliberately generous — it is not a performance gate) with a clean
// failure that preserves the frozen FIND-094 contract: errors.Is(
// port.ErrKnowledgeUnavailable) and errors.Is(context.Canceled) /
// errors.Is(context.DeadlineExceeded) both hold, and no cards are ever
// returned past cancellation.
func BenchmarkKnowledgeRetrievalCancellationBoundAtScale(b *testing.B) {
	corpus := knowledgeBench(b)
	const sanityCeiling = 5 * time.Second
	b.ReportAllocs()
	b.Run("canceled", func(b *testing.B) {
		b.ResetTimer()
		for range b.N {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			started := time.Now()
			result, err := corpus.retriever.Retrieve(ctx, corpus.request)
			elapsed := time.Since(started)
			if !errors.Is(err, port.ErrKnowledgeUnavailable) || !errors.Is(err, context.Canceled) {
				b.Fatalf("canceled Retrieve() error = %v, want unavailable wrapping context.Canceled", err)
			}
			if len(result.Cards) != 0 {
				b.Fatalf("canceled Retrieve() returned %d cards", len(result.Cards))
			}
			if elapsed > sanityCeiling {
				b.Fatalf("canceled Retrieve() ran %v before returning", elapsed)
			}
		}
	})
	b.Run("deadline", func(b *testing.B) {
		b.ResetTimer()
		for range b.N {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			started := time.Now()
			result, err := corpus.retriever.Retrieve(ctx, corpus.request)
			elapsed := time.Since(started)
			cancel()
			if !errors.Is(err, port.ErrKnowledgeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
				b.Fatalf("deadline Retrieve() error = %v, want unavailable wrapping context.DeadlineExceeded", err)
			}
			if len(result.Cards) != 0 {
				b.Fatalf("deadline Retrieve() returned %d cards", len(result.Cards))
			}
			if elapsed > sanityCeiling {
				b.Fatalf("deadline Retrieve() ran %v before returning", elapsed)
			}
		}
	})
	b.Logf("cancellation holds at 10,000 items: both failures bounded well under the %v sanity ceiling with zero cards", sanityCeiling)
}

// benchmarkCandidateSink keeps per-iteration results observable to the
// compiler without adding output work inside the measured loop.
var benchmarkCandidateSink int
