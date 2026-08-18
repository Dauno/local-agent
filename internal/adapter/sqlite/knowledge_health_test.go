package sqlite

import (
	"strings"
	"testing"
	"time"
)

const doctorClaimID = "kclaim_0000000000000000000000a1"

func doctorSeedClaim(t *testing.T, store *Store, id, subject string) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, current_rev, created_at, updated_at)
		VALUES (?, ?, 'is', 'string', 'value', 'project', 'workspace', 'human', ?, 'asserted', 1, 100, 100)`, id, subject, id); err != nil {
		t.Fatal(err)
	}
}

func doctorSeedQueueRow(t *testing.T, store *Store, table, kind, id, status string, attempts int, leaseUntil int64) {
	t.Helper()
	lastError := ""
	if status == "failed" {
		lastError = "source_invalid"
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO `+table+` (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, 0, ?, ?, 100, 100)
		ON CONFLICT(item_kind, item_id) DO UPDATE SET
			status = excluded.status, attempts = excluded.attempts, next_attempt = excluded.next_attempt,
			lease_until = excluded.lease_until, last_error = excluded.last_error`, kind, id, status, attempts, leaseUntil, lastError); err != nil {
		t.Fatal(err)
	}
}

func doctorSeedFTSRow(t *testing.T, store *Store, kind, id string, revision int) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_retrieval_fts (item_kind, item_id, item_revision, source_digest, subject, body)
		VALUES (?, ?, ?, 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', ?, ?)`, kind, id, revision, id, "body"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckKnowledgeRetrievalStatePassesFreshDatabase(t *testing.T) {
	store, _ := newTestStore(t)
	health, err := store.CheckKnowledgeRetrievalState(t.Context())
	if err != nil {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v", err)
	}
	if health.LexicalQueuePending != 0 || health.EmbeddingQueuePending != 0 || health.LexicalRepairableMismatch != 0 {
		t.Fatalf("fresh health = %#v", health)
	}
}

func TestCheckKnowledgeRetrievalStateFailsOnMissingObject(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.DB().ExecContext(t.Context(), `DROP TABLE knowledge_embeddings`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "knowledge_embeddings") {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want a missing-object failure", err)
	}
}

func TestCheckKnowledgeRetrievalStateFailsOnInvalidQueueLease(t *testing.T) {
	store, _ := newTestStore(t)
	doctorSeedQueueRow(t, store, "knowledge_lexical_queue", "claim", doctorClaimID, "pending", 0, 500)
	if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid state") {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want an invalid-lease failure", err)
	}
}

func TestCheckKnowledgeRetrievalStateReportsRepairableOrphan(t *testing.T) {
	store, _ := newTestStore(t)
	doctorSeedFTSRow(t, store, "claim", doctorClaimID, 1)
	doctorSeedQueueRow(t, store, "knowledge_lexical_queue", "claim", doctorClaimID, "pending", 0, 0)
	health, err := store.CheckKnowledgeRetrievalState(t.Context())
	if err != nil {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want bounded repairable state", err)
	}
	if health.LexicalRepairableMismatch != 1 {
		t.Fatalf("repairable mismatches = %d, want 1", health.LexicalRepairableMismatch)
	}
}

func TestCheckKnowledgeRetrievalStateFailsOnUnrepairableOrphan(t *testing.T) {
	store, _ := newTestStore(t)
	doctorSeedFTSRow(t, store, "claim", doctorClaimID, 1)
	if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want an unrepairable-orphan failure", err)
	}
}

func TestCheckKnowledgeRetrievalStateRevisionMismatchRepairability(t *testing.T) {
	t.Run("repairable revision mismatch", func(t *testing.T) {
		store, _ := newTestStore(t)
		doctorSeedClaim(t, store, doctorClaimID, "subject")
		doctorSeedFTSRow(t, store, "claim", doctorClaimID, 2)
		doctorSeedQueueRow(t, store, "knowledge_lexical_queue", "claim", doctorClaimID, "processing", 1, time.Now().Add(time.Hour).Unix())
		health, err := store.CheckKnowledgeRetrievalState(t.Context())
		if err != nil {
			t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want bounded repairable state", err)
		}
		if health.LexicalRepairableMismatch != 1 || health.LexicalQueueStaleProcessing != 0 {
			t.Fatalf("health = %#v", health)
		}
	})
	t.Run("unrepairable revision mismatch", func(t *testing.T) {
		store, _ := newTestStore(t)
		doctorSeedClaim(t, store, doctorClaimID, "subject")
		doctorSeedFTSRow(t, store, "claim", doctorClaimID, 2)
		doctorSeedQueueRow(t, store, "knowledge_lexical_queue", "claim", doctorClaimID, "done", 0, 0)
		if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "orphan") {
			t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want an unrepairable-mismatch failure", err)
		}
	})
}

func TestCheckKnowledgeRetrievalStateStaleProcessingIsBoundedState(t *testing.T) {
	store, _ := newTestStore(t)
	doctorSeedClaim(t, store, doctorClaimID, "subject")
	doctorSeedFTSRow(t, store, "claim", doctorClaimID, 1)
	doctorSeedQueueRow(t, store, "knowledge_lexical_queue", "claim", doctorClaimID, "processing", 1, time.Now().Add(-time.Hour).Unix())
	health, err := store.CheckKnowledgeRetrievalState(t.Context())
	if err != nil {
		t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want bounded stale-lease state", err)
	}
	if health.LexicalQueueStaleProcessing != 1 {
		t.Fatalf("stale processing = %d, want 1", health.LexicalQueueStaleProcessing)
	}
}

func TestCheckKnowledgeRetrievalStateVectorStructureAndFingerprintConsistency(t *testing.T) {
	t.Run("vector length mismatch fails", func(t *testing.T) {
		store, _ := newTestStore(t)
		if _, err := store.DB().ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`); err != nil {
			t.Fatal(err)
		}
		doctorSeedClaim(t, store, doctorClaimID, "subject")
		if _, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
			VALUES ('claim', ?, 1, 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', 'fp-1', 4, X'00000000', 100)`, doctorClaimID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "vector length") {
			t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want a vector-length failure", err)
		}
	})
	t.Run("fingerprint dimension inconsistency fails", func(t *testing.T) {
		store, _ := newTestStore(t)
		doctorSeedClaim(t, store, doctorClaimID, "subject")
		second := "kclaim_0000000000000000000000a2"
		doctorSeedClaim(t, store, second, "subject two")
		for _, seed := range []struct {
			id         string
			dimensions int
			vector     []byte
		}{
			{doctorClaimID, 2, make([]byte, 8)},
			{second, 3, make([]byte, 12)},
		} {
			if _, err := store.DB().ExecContext(t.Context(), `
				INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
				VALUES ('claim', ?, 1, 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', 'fp-1', ?, ?, 100)`,
				seed.id, seed.dimensions, seed.vector); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "inconsistent dimensions") {
			t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want a fingerprint-consistency failure", err)
		}
	})
}

func TestCheckKnowledgeRetrievalStateRejectsOutOfRangeDimensions(t *testing.T) {
	tests := []struct {
		name       string
		dimensions int
		vector     []byte
	}{
		{
			// Above range: 5000 dimensions with a matching 20000-byte
			// vector. Only the closed 1..4096 bound can reject this row
			// when CHECK constraints are omitted.
			name:       "above range",
			dimensions: 5000,
			vector:     make([]byte, 20000),
		},
		{
			// Below range: zero dimensions with a matching zero-byte
			// vector (0 * 4 = 0). The byte-length predicate passes, so
			// only the closed 1..4096 bound can reject this row when
			// CHECK constraints are omitted.
			name:       "below range",
			dimensions: 0,
			vector:     make([]byte, 0),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTestStore(t)
			if _, err := store.DB().ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`); err != nil {
				t.Fatal(err)
			}
			doctorSeedClaim(t, store, doctorClaimID, "subject")
			if _, err := store.DB().ExecContext(t.Context(), `
				INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
				VALUES ('claim', ?, 1, 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', 'fp-1', ?, ?, 100)`,
				doctorClaimID, tc.dimensions, tc.vector); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CheckKnowledgeRetrievalState(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid kind") {
				t.Fatalf("CheckKnowledgeRetrievalState() error = %v, want a dimension-bound failure", err)
			}
		})
	}
}
