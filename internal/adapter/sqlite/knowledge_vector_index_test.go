package sqlite

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const (
	validVectorDigest    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherVectorDigest    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	vectorFingerprintOne = "fp-one"
	vectorFingerprintTwo = "fp-two"
)

func validVectorBytes(dimensions int) []byte {
	return make([]byte, dimensions*4)
}

func TestKnowledgeVectorIndexReplaceIsScopedToFingerprint(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeVectorIndexStore(store)

	if err := index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_v1", 2, validVectorDigest, vectorFingerprintOne, validVectorBytes(4)); err != nil {
		t.Fatalf("ReplaceVector(fp-one) error = %v", err)
	}
	if err := index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_v1", 3, otherVectorDigest, vectorFingerprintTwo, validVectorBytes(4)); err != nil {
		t.Fatalf("ReplaceVector(fp-two) error = %v", err)
	}
	// Both fingerprints coexist: the second replacement never deleted the
	// first fingerprint's row.
	rows := vectorRows(t, store, "kclaim_v1")
	if len(rows) != 2 {
		t.Fatalf("vector rows = %+v, want one row per fingerprint", rows)
	}
	var fpOneRevision, fpTwoRevision int
	for _, row := range rows {
		switch row.fingerprint {
		case vectorFingerprintOne:
			fpOneRevision = row.revision
		case vectorFingerprintTwo:
			fpTwoRevision = row.revision
		}
	}
	if fpOneRevision != 2 || fpTwoRevision != 3 {
		t.Fatalf("rows = %+v, want fp-one revision 2 and fp-two revision 3 untouched", rows)
	}
	// Replacing under one fingerprint swaps only that fingerprint's row.
	if err := index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_v1", 5, otherVectorDigest, vectorFingerprintOne, validVectorBytes(4)); err != nil {
		t.Fatalf("ReplaceVector(fp-one again) error = %v", err)
	}
	rows = vectorRows(t, store, "kclaim_v1")
	if len(rows) != 2 {
		t.Fatalf("vector rows after fp-one replacement = %+v, want still 2", rows)
	}
	for _, row := range rows {
		switch row.fingerprint {
		case vectorFingerprintOne:
			if row.revision != 5 {
				t.Fatalf("fp-one row = %+v, want replaced revision 5", row)
			}
		case vectorFingerprintTwo:
			if row.revision != 3 {
				t.Fatalf("fp-two row = %+v, want untouched revision 3", row)
			}
		}
	}
}

func TestKnowledgeVectorIndexDeleteRemovesAllFingerprints(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeVectorIndexStore(store)

	for _, fp := range []string{vectorFingerprintOne, vectorFingerprintTwo} {
		if err := index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalDocument, "kdoc_v1", 1, validVectorDigest, fp, validVectorBytes(2)); err != nil {
			t.Fatalf("ReplaceVector(%s) error = %v", fp, err)
		}
	}
	if err := index.DeleteVector(t.Context(), domain.KnowledgeRetrievalDocument, "kdoc_v1"); err != nil {
		t.Fatalf("DeleteVector() error = %v", err)
	}
	if rows := vectorRows(t, store, "kdoc_v1"); len(rows) != 0 {
		t.Fatalf("vector rows after delete = %+v, want none", rows)
	}
}

func TestKnowledgeVectorIndexListPagesByFingerprintInOrder(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeVectorIndexStore(store)

	seed := func(kind domain.KnowledgeRetrievalItemKind, id string, fp string) {
		if err := index.ReplaceVector(t.Context(), kind, id, 1, validVectorDigest, fp, validVectorBytes(2)); err != nil {
			t.Fatalf("seed ReplaceVector(%s, %s) error = %v", id, fp, err)
		}
	}
	seed(domain.KnowledgeRetrievalClaim, "kclaim_b", vectorFingerprintOne)
	seed(domain.KnowledgeRetrievalClaim, "kclaim_a", vectorFingerprintOne)
	seed(domain.KnowledgeRetrievalClaim, "kclaim_c", vectorFingerprintTwo)

	first, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintOne, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != "kclaim_a" || first[0].Fingerprint != vectorFingerprintOne {
		t.Fatalf("ListVector(first page) = %+v, %v", first, err)
	}
	second, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintOne, "kclaim_a", 1)
	if err != nil || len(second) != 1 || second[0].ID != "kclaim_b" {
		t.Fatalf("ListVector(second page) = %+v, %v", second, err)
	}
	exhausted, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintOne, "kclaim_b", 1)
	if err != nil || len(exhausted) != 0 {
		t.Fatalf("ListVector(exhausted) = %+v, %v", exhausted, err)
	}
	other, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintTwo, "", 16)
	if err != nil || len(other) != 1 || other[0].ID != "kclaim_c" {
		t.Fatalf("ListVector(fp-two) = %+v, %v", other, err)
	}
}

func TestKnowledgeVectorIndexClearRemovesEveryFingerprint(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeVectorIndexStore(store)

	for _, fp := range []string{vectorFingerprintOne, vectorFingerprintTwo} {
		if err := index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalPreference, "preference:4", 1, validVectorDigest, fp, validVectorBytes(2)); err != nil {
			t.Fatalf("ReplaceVector(%s) error = %v", fp, err)
		}
	}
	if err := index.ClearVector(t.Context()); err != nil {
		t.Fatalf("ClearVector() error = %v", err)
	}
	if rows := vectorRows(t, store, "preference:4"); len(rows) != 0 {
		t.Fatalf("vector rows after clear = %+v, want none", rows)
	}
}

func TestKnowledgeVectorIndexRejectsInvalidMutations(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeVectorIndexStore(store)

	longFingerprint := strings.Repeat("f", 257)
	mutate := func(mutator func() error) {
		if err := mutator(); err == nil {
			t.Fatal("mutation accepted an invalid input")
		}
	}
	mutate(func() error {
		return index.ReplaceVector(t.Context(), "unknown", "kclaim_x", 1, validVectorDigest, vectorFingerprintOne, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "", 1, validVectorDigest, vectorFingerprintOne, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 0, validVectorDigest, vectorFingerprintOne, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, "not-hex", vectorFingerprintOne, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, strings.Repeat("a", 63), vectorFingerprintOne, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, "", validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, longFingerprint, validVectorBytes(2))
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, vectorFingerprintOne, nil)
	})
	mutate(func() error {
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, vectorFingerprintOne, make([]byte, 6))
	})
	mutate(func() error {
		// 4097 dimensions is above the closed 1..4096 bound.
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, vectorFingerprintOne, validVectorBytes(4097))
	})
	mutate(func() error {
		// Six bytes do not encode a whole number of float32 values.
		return index.ReplaceVector(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_x", 1, validVectorDigest, vectorFingerprintOne, make([]byte, 6))
	})
	mutate(func() error {
		return index.DeleteVector(t.Context(), "unknown", "kclaim_x")
	})
	mutate(func() error {
		return index.DeleteVector(t.Context(), domain.KnowledgeRetrievalClaim, "")
	})
	mutate(func() error {
		_, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, "", "", 8)
		return err
	})
	mutate(func() error {
		_, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, longFingerprint, "", 8)
		return err
	})
	mutate(func() error {
		_, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintOne, "", 0)
		return err
	})
	mutate(func() error {
		_, err := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, vectorFingerprintOne, "", domain.HardMaxKnowledgeQueueListLimit+1)
		return err
	})
}

type vectorRow struct {
	id          string
	revision    int
	fingerprint string
	dimensions  int
}

func vectorRows(t *testing.T, store *Store, id string) []vectorRow {
	t.Helper()
	rows, err := store.DB().QueryContext(t.Context(), `
		SELECT item_id, item_revision, model_fingerprint, dimensions FROM knowledge_embeddings WHERE item_id = ? ORDER BY model_fingerprint`, id)
	if err != nil {
		t.Fatalf("query vector rows: %v", err)
	}
	defer rows.Close()
	var result []vectorRow
	for rows.Next() {
		var row vectorRow
		if err := rows.Scan(&row.id, &row.revision, &row.fingerprint, &row.dimensions); err != nil {
			t.Fatalf("scan vector row: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scan vector rows: %v", err)
	}
	return result
}
