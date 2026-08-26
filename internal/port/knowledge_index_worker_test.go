package port

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// fakeKnowledgeIndexSource is the behavioral fake for KnowledgeIndexSource:
// identity lookups never apply scopes and return ErrKnowledgeNotFound for
// missing items.
type fakeKnowledgeIndexSource struct {
	items map[string]KnowledgeAuthoritativeItem
	err   error
}

func newFakeKnowledgeIndexSource(items []KnowledgeAuthoritativeItem) *fakeKnowledgeIndexSource {
	source := &fakeKnowledgeIndexSource{items: make(map[string]KnowledgeAuthoritativeItem)}
	for _, item := range items {
		source.items[string(item.Kind)+"\x00"+item.ID] = item
	}
	return source
}

func (f *fakeKnowledgeIndexSource) ReadIndexSource(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string) (KnowledgeAuthoritativeItem, error) {
	if f.err != nil {
		return KnowledgeAuthoritativeItem{}, f.err
	}
	item, ok := f.items[string(kind)+"\x00"+id]
	if !ok {
		return KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: index source identity is missing", ErrKnowledgeNotFound)
	}
	return item, nil
}

// fakeKnowledgeLexicalIndex is the behavioral fake for KnowledgeLexicalIndex:
// rows are keyed by (kind, id), Replace overwrites atomically, and List pages
// in stable identity order.
type fakeKnowledgeLexicalIndex struct {
	rows map[string]KnowledgeLexicalIndexRow
	err  error
}

func newFakeKnowledgeLexicalIndex(rows []KnowledgeLexicalIndexRow) *fakeKnowledgeLexicalIndex {
	index := &fakeKnowledgeLexicalIndex{rows: make(map[string]KnowledgeLexicalIndexRow)}
	for _, row := range rows {
		index.rows[string(row.Kind)+"\x00"+row.ID] = row
	}
	return index
}

func (f *fakeKnowledgeLexicalIndex) ReplaceLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, subject, body string) error {
	if f.err != nil {
		return f.err
	}
	f.rows[string(kind)+"\x00"+id] = KnowledgeLexicalIndexRow{Kind: kind, ID: id, Revision: revision, SourceDigest: sourceDigest}
	return nil
}

func (f *fakeKnowledgeLexicalIndex) DeleteLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.rows, string(kind)+"\x00"+id)
	return nil
}

func (f *fakeKnowledgeLexicalIndex) ListLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]KnowledgeLexicalIndexRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	if limit <= 0 || limit > domain.HardMaxKnowledgeQueueListLimit {
		return nil, fmt.Errorf("%w: index list limit %d is not bounded", ErrKnowledgeValidation, limit)
	}
	ids := make([]string, 0, len(f.rows))
	for key := range f.rows {
		ids = append(ids, key)
	}
	sortStrings(ids)
	rows := make([]KnowledgeLexicalIndexRow, 0, limit)
	for _, key := range ids {
		row := f.rows[key]
		if row.Kind != kind || row.ID <= afterID {
			continue
		}
		rows = append(rows, row)
		if len(rows) == limit {
			break
		}
	}
	return rows, nil
}

func (f *fakeKnowledgeLexicalIndex) ClearLexical(_ context.Context) error {
	if f.err != nil {
		return f.err
	}
	clear(f.rows)
	return nil
}

// fakeKnowledgeIdentityLister is the behavioral fake for
// KnowledgeIdentityLister: pages identities in stable order with current
// revisions and never returns content.
type fakeKnowledgeIdentityLister struct {
	identities map[domain.KnowledgeRetrievalItemKind][]KnowledgeTruthIdentity
	err        error
}

// fakeKnowledgeIdentityEntry pairs a truth identity with its kind for test
// seeding.
type fakeKnowledgeIdentityEntry struct {
	Kind     domain.KnowledgeRetrievalItemKind
	Identity KnowledgeTruthIdentity
}

func newFakeKnowledgeIdentityListerWithKinds(entries []fakeKnowledgeIdentityEntry) *fakeKnowledgeIdentityLister {
	lister := &fakeKnowledgeIdentityLister{identities: make(map[domain.KnowledgeRetrievalItemKind][]KnowledgeTruthIdentity)}
	for _, entry := range entries {
		lister.identities[entry.Kind] = append(lister.identities[entry.Kind], entry.Identity)
	}
	return lister
}

func (f *fakeKnowledgeIdentityLister) ListTruthIdentities(_ context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]KnowledgeTruthIdentity, error) {
	if f.err != nil {
		return nil, f.err
	}
	if limit <= 0 || limit > domain.HardMaxKnowledgeQueueListLimit {
		return nil, fmt.Errorf("%w: identity list limit %d is not bounded", ErrKnowledgeValidation, limit)
	}
	identities := append([]KnowledgeTruthIdentity(nil), f.identities[kind]...)
	sortKnowledgeTruthIdentities(identities)
	rows := make([]KnowledgeTruthIdentity, 0, limit)
	for _, identity := range identities {
		if identity.ID <= afterID {
			continue
		}
		rows = append(rows, identity)
		if len(rows) == limit {
			break
		}
	}
	return rows, nil
}

func sortKnowledgeTruthIdentities(identities []KnowledgeTruthIdentity) {
	slices.SortFunc(identities, func(a, b KnowledgeTruthIdentity) int { return cmp.Compare(a.ID, b.ID) })
}

func sortStrings(values []string) {
	sort.Strings(values)
}

// TestKnowledgeIndexSourceReadsByIdentityWithoutScopes pins the behavioral
// contract: identity reads carry no scope predicates and missing identities
// report the not-found sentinel.
func TestKnowledgeIndexSourceReadsByIdentityWithoutScopes(t *testing.T) {
	claim := domain.KnowledgeClaim{
		ID:          "kclaim_1",
		Subject:     "db host",
		Predicate:   domain.KnowledgePredicateIs,
		Value:       domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "db.internal"},
		ScopeKind:   domain.KnowledgeScopeProject,
		ScopeID:     "my-project",
		SourceClass: domain.KnowledgeSourceHuman,
		SourceRef:   "slack:msg:1",
		Status:      domain.KnowledgeClaimAsserted,
		Revision:    1,
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("claim.Validate() error = %v", err)
	}
	source := newFakeKnowledgeIndexSource([]KnowledgeAuthoritativeItem{{Kind: domain.KnowledgeRetrievalClaim, ID: string(claim.ID), Claim: &claim}})
	item, err := source.ReadIndexSource(t.Context(), domain.KnowledgeRetrievalClaim, string(claim.ID))
	if err != nil {
		t.Fatalf("ReadIndexSource() error = %v", err)
	}
	if item.Claim == nil || item.Claim.ID != claim.ID {
		t.Fatalf("ReadIndexSource() item = %+v, want the authoritative claim", item)
	}
	if _, err := source.ReadIndexSource(t.Context(), domain.KnowledgeRetrievalClaim, "missing"); !errors.Is(err, ErrKnowledgeNotFound) {
		t.Fatalf("ReadIndexSource(missing) error = %v, want ErrKnowledgeNotFound", err)
	}
}

// TestKnowledgeLexicalIndexFakeReplacesAndListsAtomically pins the worker
// index contract: Replace overwrites one identity, List pages in identity
// order, and Clear removes everything.
func TestKnowledgeLexicalIndexFakeReplacesAndListsAtomically(t *testing.T) {
	index := newFakeKnowledgeLexicalIndex(nil)
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_1", 3, "aabb", "subject", "body"); err != nil {
		t.Fatalf("ReplaceLexical() error = %v", err)
	}
	rows, err := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10)
	if err != nil || len(rows) != 1 || rows[0].Revision != 3 || rows[0].SourceDigest != "aabb" {
		t.Fatalf("ListLexical() = %+v, %v", rows, err)
	}
	if err := index.ClearLexical(t.Context()); err != nil {
		t.Fatalf("ClearLexical() error = %v", err)
	}
	rows, err = index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListLexical() after clear = %+v, %v", rows, err)
	}
}

// TestKnowledgeIdentityListerFakePagesStableOrder pins the truth-identity
// pagination contract used by reconciliation.
func TestKnowledgeIdentityListerFakePagesStableOrder(t *testing.T) {
	lister := newFakeKnowledgeIdentityListerWithKinds([]fakeKnowledgeIdentityEntry{
		{Kind: domain.KnowledgeRetrievalClaim, Identity: KnowledgeTruthIdentity{ID: "kclaim_2", Revision: 1}},
		{Kind: domain.KnowledgeRetrievalClaim, Identity: KnowledgeTruthIdentity{ID: "kclaim_1", Revision: 4}},
	})
	first, err := lister.ListTruthIdentities(t.Context(), domain.KnowledgeRetrievalClaim, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != "kclaim_1" || first[0].Revision != 4 {
		t.Fatalf("ListTruthIdentities(first) = %+v, %v", first, err)
	}
	second, err := lister.ListTruthIdentities(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_1", 1)
	if err != nil || len(second) != 1 || second[0].ID != "kclaim_2" {
		t.Fatalf("ListTruthIdentities(second) = %+v, %v", second, err)
	}
}
