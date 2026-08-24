package knowledge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	testConversation = domain.ConversationKey("slack:T00000001:channel:C00000001:thread:1234567890.123456")
	testActor        = "U00000001"
	testOwnerKey     = "slack:T00000001:user:U00000001"
)

func testBinding() domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team: "T00000001", Actor: testActor, Conversation: testConversation,
	}
}

func rememberText(subject string) string {
	return HumanCommandPrefix + `{"action":"remember","subject":"` + subject + `","predicate":"is","value_kind":"string","value_text":"pg-01"}`
}

type countingCoordinator struct {
	mu      sync.Mutex
	held    map[string]bool
	inUse   int64
	maxSeen int64
}

func newCountingCoordinator() *countingCoordinator {
	return &countingCoordinator{held: map[string]bool{}, inUse: 0, maxSeen: 0}
}

func (c *countingCoordinator) TryAcquire(key string) (func(), bool) {
	c.mu.Lock()
	if c.held[key] {
		c.mu.Unlock()
		return nil, false
	}
	c.held[key] = true
	now := atomic.AddInt64(&c.inUse, 1)
	for {
		max := atomic.LoadInt64(&c.maxSeen)
		if now <= max || atomic.CompareAndSwapInt64(&c.maxSeen, max, now) {
			break
		}
	}
	c.mu.Unlock()
	return func() {
		atomic.AddInt64(&c.inUse, -1)
		c.mu.Lock()
		delete(c.held, key)
		c.mu.Unlock()
	}, true
}

type busyCoordinator struct{}

func (busyCoordinator) TryAcquire(string) (func(), bool) { return nil, false }

type fakeKnowledgeStore struct {
	mu          sync.Mutex
	claims      map[domain.KnowledgeClaimID]domain.KnowledgeClaim
	preferences map[string]domain.KnowledgePreference
	documents   map[domain.KnowledgeDocumentID]domain.KnowledgeDocument
	tombstones  map[string]bool
	receipts    map[string]domain.KnowledgeCommandReceipt
	nextClaim   int
	nextPref    int

	createdClaims   []domain.KnowledgeClaim
	corrects        []domain.KnowledgeClaim
	transitions     []string
	forgets         []string
	archiveDocCalls []archiveDocumentCall

	createErr     error
	createDelay   time.Duration
	transitionErr error
	forgetResult  bool
}

type archiveDocumentCall struct {
	id        domain.KnowledgeDocumentID
	revision  int
	sourceRef string
}

func newFakeKnowledgeStore() *fakeKnowledgeStore {
	return &fakeKnowledgeStore{
		claims:       map[domain.KnowledgeClaimID]domain.KnowledgeClaim{},
		preferences:  map[string]domain.KnowledgePreference{},
		documents:    map[domain.KnowledgeDocumentID]domain.KnowledgeDocument{},
		tombstones:   map[string]bool{},
		receipts:     map[string]domain.KnowledgeCommandReceipt{},
		forgetResult: true,
	}
}

func scopeAllowed(scopes []domain.KnowledgeScopeRef, kind domain.KnowledgeScopeKind, id string) bool {
	for _, scope := range scopes {
		if scope.Kind == kind && scope.ID == id {
			return true
		}
	}
	return false
}

func (f *fakeKnowledgeStore) CreateClaim(_ context.Context, claim domain.KnowledgeClaim, _ domain.KnowledgeLimits) (domain.KnowledgeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return domain.KnowledgeClaim{}, f.createErr
	}
	for _, existing := range f.claims {
		if existing.Subject == claim.Subject && existing.ScopeKind == claim.ScopeKind && existing.ScopeID == claim.ScopeID && existing.SourceRef == claim.SourceRef {
			return existing, nil
		}
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	f.nextClaim++
	claim.ID = domain.KnowledgeClaimID(fmt.Sprintf("kclaim_%d", f.nextClaim))
	claim.Revision = 1
	claim.CreatedAt = time.Now()
	claim.UpdatedAt = claim.CreatedAt
	f.claims[claim.ID] = claim
	f.createdClaims = append(f.createdClaims, claim)
	return claim, nil
}

func (f *fakeKnowledgeStore) GetClaim(_ context.Context, id domain.KnowledgeClaimID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim, ok := f.claims[id]
	if !ok || !scopeAllowed(readable, claim.ScopeKind, claim.ScopeID) {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeNotFound
	}
	return claim, nil
}

func (f *fakeKnowledgeStore) CorrectClaim(_ context.Context, replacement domain.KnowledgeClaim, _ domain.KnowledgeSourceClass, _ domain.KnowledgeLimits) (domain.KnowledgeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corrects = append(f.corrects, replacement)
	prior, ok := f.claims[replacement.SupersedesID]
	if !ok {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeNotFound
	}
	prior.Status = domain.KnowledgeClaimSuperseded
	f.claims[prior.ID] = prior
	f.nextClaim++
	replacement.ID = domain.KnowledgeClaimID(fmt.Sprintf("kclaim_%d", f.nextClaim))
	replacement.Revision = 1
	f.claims[replacement.ID] = replacement
	return replacement, nil
}

func (f *fakeKnowledgeStore) TransitionClaimStatus(_ context.Context, id domain.KnowledgeClaimID, expectedRev int, next domain.KnowledgeClaimStatus, _ domain.KnowledgeSourceClass, sourceRef string) (domain.KnowledgeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, string(id)+":"+string(next)+":"+sourceRef)
	if f.transitionErr != nil {
		return domain.KnowledgeClaim{}, f.transitionErr
	}
	claim, ok := f.claims[id]
	if !ok {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeNotFound
	}
	if expectedRev != claim.Revision {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeCASConflict
	}
	claim.Status = next
	claim.Revision++
	f.claims[id] = claim
	return claim, nil
}

func (f *fakeKnowledgeStore) ForgetSubject(_ context.Context, subject string, scopeKind domain.KnowledgeScopeKind, scopeID, sourceRef string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgets = append(f.forgets, subject+":"+string(scopeKind)+":"+scopeID+":"+sourceRef)
	for id, claim := range f.claims {
		if claim.Subject == subject && claim.ScopeKind == scopeKind && claim.ScopeID == scopeID {
			delete(f.claims, id)
		}
	}
	digest := domain.KnowledgeSubjectDigest(subject, scopeKind, scopeID)
	if f.tombstones[digest] {
		return false, nil
	}
	f.tombstones[digest] = true
	return f.forgetResult, nil
}

func (f *fakeKnowledgeStore) AddEvidence(context.Context, domain.KnowledgeClaimID, int, domain.KnowledgeEvidence) error {
	return nil
}

func (f *fakeKnowledgeStore) CreatePreference(_ context.Context, preference domain.KnowledgePreference, _ domain.KnowledgeLimits) (domain.KnowledgePreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := preference.OwnerKey + "\x00" + preference.Key
	if existing, ok := f.preferences[key]; ok {
		return existing, nil
	}
	f.nextPref++
	preference.ID = f.nextPref
	preference.Revision = 1
	f.preferences[key] = preference
	return preference, nil
}

func (f *fakeKnowledgeStore) UpdatePreference(_ context.Context, preference domain.KnowledgePreference, expectedRev int, _ domain.KnowledgeLimits) (domain.KnowledgePreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := preference.OwnerKey + "\x00" + preference.Key
	existing, ok := f.preferences[key]
	if !ok {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	if expectedRev != existing.Revision {
		return domain.KnowledgePreference{}, port.ErrKnowledgeCASConflict
	}
	existing.Value = preference.Value
	existing.Revision++
	f.preferences[key] = existing
	return existing, nil
}

func (f *fakeKnowledgeStore) GetPreference(_ context.Context, ownerKey, key string) (domain.KnowledgePreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	preference, ok := f.preferences[ownerKey+"\x00"+key]
	if !ok {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	return preference, nil
}

func (f *fakeKnowledgeStore) ListPreferencesForOwner(_ context.Context, ownerKey string, _ domain.KnowledgeLimits) ([]domain.KnowledgePreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var listed []domain.KnowledgePreference
	for _, preference := range f.preferences {
		if preference.OwnerKey == ownerKey {
			listed = append(listed, preference)
		}
	}
	return listed, nil
}

func (f *fakeKnowledgeStore) ArchivePreference(_ context.Context, ownerKey, key string, expectedRev int, _ string) (domain.KnowledgePreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.preferences[ownerKey+"\x00"+key]
	if !ok {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	if expectedRev != existing.Revision {
		return domain.KnowledgePreference{}, port.ErrKnowledgeCASConflict
	}
	existing.Status = domain.KnowledgePreferenceArchived
	existing.Revision++
	f.preferences[ownerKey+"\x00"+key] = existing
	return existing, nil
}

func (f *fakeKnowledgeStore) CreateDocument(_ context.Context, document domain.KnowledgeDocument, _ domain.KnowledgeLimits) (domain.KnowledgeDocument, error) {
	return document, nil
}

func (f *fakeKnowledgeStore) GetDocument(_ context.Context, id domain.KnowledgeDocumentID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	document, ok := f.documents[id]
	if !ok || !scopeAllowed(readable, document.ScopeKind, document.ScopeID) {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeNotFound
	}
	return document, nil
}

func (f *fakeKnowledgeStore) ListClaimsInScopes(_ context.Context, scopes []domain.KnowledgeScopeRef, subject string, limits domain.KnowledgeLimits) ([]domain.KnowledgeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var listed []domain.KnowledgeClaim
	for _, claim := range f.claims {
		if subject != "" && claim.Subject != subject {
			continue
		}
		if scopeAllowed(scopes, claim.ScopeKind, claim.ScopeID) {
			listed = append(listed, claim)
		}
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].ID < listed[j].ID })
	maxRows := limits.WithDefaults().MaxClaimsListing + 1
	if len(listed) > maxRows {
		listed = listed[:maxRows]
	}
	return listed, nil
}

func (f *fakeKnowledgeStore) ListDocumentsInScopes(_ context.Context, scopes []domain.KnowledgeScopeRef, _ domain.KnowledgeLimits) ([]domain.KnowledgeDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var listed []domain.KnowledgeDocument
	for _, document := range f.documents {
		if scopeAllowed(scopes, document.ScopeKind, document.ScopeID) {
			listed = append(listed, document)
		}
	}
	return listed, nil
}

func (f *fakeKnowledgeStore) ArchiveDocument(_ context.Context, id domain.KnowledgeDocumentID, expectedRev int, sourceRef string) (domain.KnowledgeDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archiveDocCalls = append(f.archiveDocCalls, archiveDocumentCall{id: id, revision: expectedRev, sourceRef: sourceRef})
	document, ok := f.documents[id]
	if !ok {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeNotFound
	}
	if expectedRev != document.Revision {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeCASConflict
	}
	document.Status = domain.KnowledgeDocumentArchived
	document.Revision++
	f.documents[id] = document
	return document, nil
}

func (f *fakeKnowledgeStore) CommitCommandReceipt(_ context.Context, receipt domain.KnowledgeCommandReceipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.receipts[receipt.SourceRef]
	if ok {
		if existing.Action != receipt.Action || existing.PayloadDigest != receipt.PayloadDigest || existing.Target != receipt.Target {
			return port.ErrKnowledgeCASConflict
		}
		return nil
	}
	f.receipts[receipt.SourceRef] = receipt
	return nil
}

func (f *fakeKnowledgeStore) EnqueueProjection(context.Context) error { return nil }
func (f *fakeKnowledgeStore) ClaimProjectionBatch(context.Context) ([]domain.KnowledgeProjectionItem, error) {
	return nil, nil
}
func (f *fakeKnowledgeStore) CompleteProjectionBatch(context.Context, []int, time.Time) error {
	return nil
}
func (f *fakeKnowledgeStore) RetryProjectionBatch(context.Context, []int, time.Time, time.Time) error {
	return nil
}
func (f *fakeKnowledgeStore) FailProjectionBatch(context.Context, []int, time.Time, string) error {
	return nil
}
func (f *fakeKnowledgeStore) CleanupProjection(context.Context, time.Time) error { return nil }

func newTestService(t *testing.T, store *fakeKnowledgeStore, coordinator port.ConversationCoordinator) *Service {
	t.Helper()
	service, err := New(Config{Enabled: true}, Dependencies{Store: store, Coordinator: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNewServiceValidation(t *testing.T) {
	store := newFakeKnowledgeStore()
	if _, err := New(Config{}, Dependencies{Coordinator: newCountingCoordinator()}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(Config{}, Dependencies{Store: store}); err == nil {
		t.Error("nil coordinator accepted")
	}
	if _, err := New(Config{Limits: domain.KnowledgeLimits{MaxSubjectRunes: domain.HardMaxKnowledgeSubjectRunes + 1}}, Dependencies{Store: store, Coordinator: newCountingCoordinator()}); err == nil {
		t.Error("invalid limits accepted")
	}
}

func TestCommittedKnowledgeMutationWakesAllConsumers(t *testing.T) {
	for _, test := range []struct {
		name     string
		storeErr error
		want     int
	}{
		{name: "committed", want: 1},
		{name: "failed", storeErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeKnowledgeStore()
			store.createErr = test.storeErr
			counts := make([]int, 3)
			wakes := make([]func(), len(counts))
			for i := range counts {
				wakes[i] = func() { counts[i]++ }
			}
			service, err := New(Config{Enabled: true}, Dependencies{Store: store, Coordinator: newCountingCoordinator(), Wakes: wakes})
			if err != nil {
				t.Fatal(err)
			}
			_, _, gotErr := service.Execute(t.Context(), testBinding(), "evt-wake-"+test.name, rememberText("wake-subject"))
			if (gotErr != nil) != (test.storeErr != nil) {
				t.Fatalf("Execute() error = %v", gotErr)
			}
			for i, count := range counts {
				if count != test.want {
					t.Fatalf("consumer %d wake count = %d, want %d", i, count, test.want)
				}
			}
		})
	}
}

func TestParseHumanCommandStrictness(t *testing.T) {
	if _, matched, err := ParseHumanCommand("not a knowledge command"); matched || err != nil {
		t.Fatalf("non-command text matched = %v, err = %v", matched, err)
	}
	cases := map[string]string{
		"unknown field":                       HumanCommandPrefix + `{"action":"forget","subject":"api","unknown":1}`,
		"trailing data":                       HumanCommandPrefix + `{"action":"forget","subject":"api"} {}`,
		"unknown action":                      HumanCommandPrefix + `{"action":"delete","subject":"api"}`,
		"two payloads":                        HumanCommandPrefix + `{"action":"forget","subject":"api"}{"action":"forget","subject":"api"}`,
		"empty payload":                       HumanCommandPrefix,
		"forget without subject":              HumanCommandPrefix + `{"action":"forget"}`,
		"correct without claim":               HumanCommandPrefix + `{"action":"correct","value_kind":"string","value_text":"x"}`,
		"correct with scope":                  HumanCommandPrefix + `{"action":"correct","claim_id":"kclaim_1","value_kind":"string","value_text":"x","scope_kind":"project"}`,
		"remember preference with subject":    HumanCommandPrefix + `{"action":"remember","preference_key":"language","subject":"api"}`,
		"archive without target":              HumanCommandPrefix + `{"action":"archive"}`,
		"archive with two targets":            HumanCommandPrefix + `{"action":"archive","claim_id":"kclaim_1","preference_key":"language"}`,
		"dispute without claim":               HumanCommandPrefix + `{"action":"dispute"}`,
		"dispute without revision":            HumanCommandPrefix + `{"action":"dispute","claim_id":"kclaim_1"}`,
		"archive claim without revision":      HumanCommandPrefix + `{"action":"archive","claim_id":"kclaim_1"}`,
		"archive preference without revision": HumanCommandPrefix + `{"action":"archive","preference_key":"language"}`,
		"inspect with both targets":           HumanCommandPrefix + `{"action":"inspect","claim_id":"kclaim_1","preference_key":"k"}`,
		"remember without predicate":          HumanCommandPrefix + `{"action":"remember","subject":"api","value_kind":"string","value_text":"x"}`,
	}
	for name, text := range cases {
		if _, matched, err := ParseHumanCommand(text); !matched || err == nil {
			t.Errorf("%s: matched = %v, err = %v, want parse rejection", name, matched, err)
		}
	}
	command, matched, err := ParseHumanCommand(HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_number":18}`)
	if err != nil || !matched || command.Action != domain.KnowledgeActionRemember {
		t.Fatalf("valid command rejected: %v %v %v", command, matched, err)
	}
	if command.ValueNumber == nil || *command.ValueNumber != 18 {
		t.Fatalf("value_number = %v", command.ValueNumber)
	}
	if _, _, err := ParseHumanCommand(HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_text":"x"}`); err == nil {
		t.Error("number value without value_number accepted")
	}
}

func TestExecuteRequiresEnabledBindingAndCoordinator(t *testing.T) {
	store := newFakeKnowledgeStore()
	disabled, err := New(Config{}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := disabled.Execute(t.Context(), testBinding(), "evt-1", rememberText("api")); !errors.Is(err, port.ErrKnowledgeDisabled) {
		t.Fatalf("disabled error = %v", err)
	}
	service := newTestService(t, store, newCountingCoordinator())
	if _, _, err := service.Execute(t.Context(), domain.KnowledgeWriteBinding{Conversation: testConversation}, "evt-1", rememberText("api")); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("missing actor error = %v", err)
	}
	if _, _, err := service.Execute(t.Context(), domain.KnowledgeWriteBinding{Actor: testActor, Conversation: "not-canonical"}, "evt-1", rememberText("api")); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("non-canonical conversation error = %v", err)
	}
	if _, _, err := service.Execute(t.Context(), testBinding(), strings.Repeat("e", 1100), rememberText("api")); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("oversized event identity error = %v", err)
	}
	busy := newTestService(t, store, busyCoordinator{})
	if _, _, err := busy.Execute(t.Context(), testBinding(), "evt-2", rememberText("api")); !errors.Is(err, port.ErrKnowledgeBusy) {
		t.Fatalf("busy coordinator error = %v", err)
	}
	if len(store.createdClaims) != 0 {
		t.Fatal("rejected commands must not reach the store")
	}
}

func TestRememberResolvesTrustedScope(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	created := store.createdClaims[0]
	if created.ScopeKind != domain.KnowledgeScopeUser || created.ScopeID != testOwnerKey {
		t.Fatalf("default scope = %s:%s, want user:%s", created.ScopeKind, created.ScopeID, testOwnerKey)
	}
	if created.SourceRef != "slack-human:evt-1" || created.AuthorID != testActor || created.Status != domain.KnowledgeClaimVerified {
		t.Fatalf("trusted provenance = %q %q %q", created.SourceRef, created.AuthorID, created.Status)
	}
	projectBinding := testBinding()
	projectBinding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-2", rememberText("db")); err != nil {
		t.Fatal(err)
	}
	if got := store.createdClaims[1]; got.ScopeKind != domain.KnowledgeScopeProject || got.ScopeID != "proj-a" {
		t.Fatalf("project default scope = %s:%s", got.ScopeKind, got.ScopeID)
	}
}

func TestRememberExplicitScopeMustMatchBinding(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	binding.Project = "proj-a"
	for _, text := range []string{
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"proj-b"}`,
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"user","scope_id":"slack:T00000001:user:U00000002"}`,
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"global"}`,
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"team","scope_id":"T00000001"}`,
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"conversation","scope_id":"` + string(testConversation) + `"}`,
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"workstream","scope_id":"ws-1"}`,
	} {
		if _, _, err := service.Execute(t.Context(), binding, "evt-3", text); err == nil {
			t.Errorf("scope payload %s unexpectedly admitted", text)
		}
	}
	if len(store.createdClaims) != 0 {
		t.Fatal("scope-spoofed claims reached the store")
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-4",
		HumanCommandPrefix+`{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"proj-a"}`); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectEnforcesReadBinding(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	projectBinding := testBinding()
	projectBinding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	claimID := store.createdClaims[0].ID
	correctText := HumanCommandPrefix + fmt.Sprintf(`{"action":"correct","claim_id":"%s","value_kind":"string","value_text":"pg-02"}`, claimID)
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-2", correctText); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("cross-project correction error = %v, want ErrKnowledgeNotFound", err)
	}
	otherBinding := projectBinding
	otherBinding.Project = "proj-b"
	if _, _, err := service.Execute(t.Context(), otherBinding, "evt-3", correctText); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("foreign project correction error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-4", correctText); err != nil {
		t.Fatal(err)
	}
	replacement := store.corrects[0]
	if replacement.Subject != "api" || replacement.ScopeKind != domain.KnowledgeScopeProject || replacement.ScopeID != "proj-a" || replacement.SupersedesID != claimID {
		t.Fatalf("replacement identity = %#v", replacement)
	}
	if replacement.SourceRef != "slack-human:evt-4" || replacement.AuthorID != testActor {
		t.Fatalf("replacement provenance = %q %q", replacement.SourceRef, replacement.AuthorID)
	}
}

func TestForgetResolveScopeAndIsolation(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	binding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-1",
		HumanCommandPrefix+`{"action":"forget","subject":"api","scope_kind":"project","scope_id":"proj-b"}`); err == nil {
		t.Fatal("forget into foreign project accepted")
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-2", HumanCommandPrefix+`{"action":"forget","subject":"api"}`); err != nil {
		t.Fatal(err)
	}
	if store.forgets[0] != "api:project:proj-a:slack-human:evt-2" {
		t.Fatalf("forget target = %q", store.forgets[0])
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-3",
		HumanCommandPrefix+`{"action":"forget","subject":"api","scope_kind":"project","scope_id":"proj-a"}`); err != nil {
		t.Fatal(err)
	}
}

func TestTransitionCommandsEnforceReadBinding(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	projectBinding := testBinding()
	projectBinding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	claimID := store.createdClaims[0].ID
	disputeText := HumanCommandPrefix + fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, claimID)
	archiveText := HumanCommandPrefix + fmt.Sprintf(`{"action":"archive","claim_id":"%s","expected_revision":1}`, claimID)
	for _, text := range []string{disputeText, archiveText} {
		if _, _, err := service.Execute(t.Context(), testBinding(), "evt-2", text); !errors.Is(err, port.ErrKnowledgeNotFound) {
			t.Fatalf("foreign-binding transition %q error = %v, want ErrKnowledgeNotFound", text, err)
		}
	}
	if len(store.transitions) != 0 {
		t.Fatal("foreign-binding transitions reached the store")
	}
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-3", disputeText); err != nil {
		t.Fatal(err)
	}
	if store.transitions[0] != string(claimID)+":disputed:slack-human:evt-3" {
		t.Fatalf("dispute transition = %q", store.transitions[0])
	}
}

func TestArchiveDocumentAndPreferenceEnforceBinding(t *testing.T) {
	store := newFakeKnowledgeStore()
	store.documents["kdoc_1"] = domain.KnowledgeDocument{
		ID: "kdoc_1", Subject: "runbook", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "proj-a",
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "handle", Status: domain.KnowledgeDocumentActive, Revision: 1,
	}
	service := newTestService(t, store, newCountingCoordinator())
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-1",
		HumanCommandPrefix+`{"action":"archive","document_id":"kdoc_1","expected_revision":1}`); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("foreign document archive error = %v, want ErrKnowledgeNotFound", err)
	}
	projectBinding := testBinding()
	projectBinding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-2",
		HumanCommandPrefix+`{"action":"archive","document_id":"kdoc_1","expected_revision":1}`); err != nil {
		t.Fatal(err)
	}
	if store.archiveDocCalls[0].revision != 1 || store.archiveDocCalls[0].sourceRef != "slack-human:evt-2" {
		t.Fatalf("document archive call = %+v", store.archiveDocCalls[0])
	}
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-3",
		HumanCommandPrefix+`{"action":"remember","preference_key":"language","value_kind":"string","value_text":"Spanish"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-4",
		HumanCommandPrefix+`{"action":"archive","preference_key":"language","expected_revision":1}`); err != nil {
		t.Fatal(err)
	}
	if store.preferences[testOwnerKey+"\x00"+"language"].Status != domain.KnowledgePreferenceArchived {
		t.Fatal("preference not archived")
	}
}

func TestInspectEnforcesReadBinding(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	projectBinding := testBinding()
	projectBinding.Project = "proj-a"
	if _, _, err := service.Execute(t.Context(), projectBinding, "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	claimID := store.createdClaims[0].ID
	_, card, err := service.Execute(t.Context(), projectBinding, "evt-2",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, "api is pg-01") || !strings.Contains(card, "human inspect") {
		t.Fatalf("card rendering = %q", card)
	}
	if !strings.Contains(card, "verified") {
		t.Fatalf("card status = %q", card)
	}
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-3",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","claim_id":"%s"}`, claimID)); !errors.Is(err, port.ErrKnowledgeNotFound) {
		t.Fatalf("foreign inspect error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestInspectListingIsBindingBound(t *testing.T) {
	store := newFakeKnowledgeStore()
	store.preferences[testOwnerKey+"\x00"+"language"] = domain.KnowledgePreference{
		OwnerKey: testOwnerKey, Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, Revision: 1,
	}
	store.documents["kdoc_1"] = domain.KnowledgeDocument{ID: "kdoc_1", Subject: "runbook", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "proj-a", Revision: 3, Status: domain.KnowledgeDocumentActive}
	store.documents["kdoc_2"] = domain.KnowledgeDocument{ID: "kdoc_2", Subject: "notes", ScopeKind: domain.KnowledgeScopeUser, ScopeID: testOwnerKey, Revision: 1, Status: domain.KnowledgeDocumentActive}
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	binding.Project = "proj-a"
	_, listing, err := service.Execute(t.Context(), binding, "evt-1", HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"language = Spanish", "runbook", "notes"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing missing %q: %s", want, listing)
		}
	}
	_, empty, err := service.Execute(t.Context(), domain.KnowledgeWriteBinding{Team: "T00000002", Actor: "U00000009", Conversation: domain.ConversationKey("slack:T00000002:channel:C00000009:thread:1234567890.123456")}, "evt-2", HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if empty != "No knowledge visible in this binding." {
		t.Fatalf("foreign listing = %q", empty)
	}
}

func TestExecuteSerializesCommandsPerConversation(t *testing.T) {
	store := newFakeKnowledgeStore()
	store.createDelay = 50 * time.Millisecond
	coordinator := newCountingCoordinator()
	service := newTestService(t, store, coordinator)
	var wg sync.WaitGroup
	var busy, ok int64
	for i := range 8 {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			subject := fmt.Sprintf("api-%d", seq)
			if _, _, err := service.Execute(t.Context(), testBinding(), fmt.Sprintf("evt-%d", seq), rememberText(subject)); err != nil {
				if errors.Is(err, port.ErrKnowledgeBusy) {
					atomic.AddInt64(&busy, 1)
					return
				}
				t.Error(err)
				return
			}
			atomic.AddInt64(&ok, 1)
		}(i)
	}
	wg.Wait()
	if coordinator.maxSeen != 1 {
		t.Fatalf("coordinator observed %d concurrent commands, want 1", coordinator.maxSeen)
	}
	if ok != 1 || busy != 7 {
		t.Fatalf("serialization admitted %d commands and rejected %d busy, want 1 and 7", ok, busy)
	}
	if len(store.createdClaims) != 1 {
		t.Fatalf("created claims = %d, want 1", len(store.createdClaims))
	}
}

func TestExecuteRejectsReplayWithDifferentContent(t *testing.T) {
	store := newFakeKnowledgeStore()
	store.createErr = port.ErrKnowledgeCASConflict
	service := newTestService(t, store, newCountingCoordinator())
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-1", rememberText("api")); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("replay conflict error = %v", err)
	}
}

func TestCommandIdentityIsGlobalPerEvent(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	if _, _, err := service.Execute(t.Context(), binding, "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-1", rememberText("api")); err != nil {
		t.Fatalf("identical replay rejected: %v", err)
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+`{"action":"remember","subject":"db","predicate":"is","value_kind":"string","value_text":"pg-02"}`); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same event different target error = %v, want ErrKnowledgeCASConflict", err)
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+`{"action":"forget","subject":"api"}`); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("same event different action error = %v, want ErrKnowledgeCASConflict", err)
	}
	if len(store.createdClaims) != 1 || len(store.forgets) != 0 {
		t.Fatalf("identity-conflicting commands mutated state: claims=%d forgets=%d", len(store.createdClaims), len(store.forgets))
	}
}

func TestExecuteRejectsEmptyEventIdentity(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	for _, eventID := range []string{"", "   ", " evt-1"} {
		if _, _, err := service.Execute(t.Context(), testBinding(), eventID, rememberText("api")); err == nil || !errors.Is(err, port.ErrKnowledgeValidation) {
			t.Fatalf("event identity %q error = %v, want ErrKnowledgeValidation", eventID, err)
		}
	}
	if len(store.createdClaims) != 0 {
		t.Fatal("empty event identity reached the store")
	}
}

func TestParseRejectsImpreciseValueUnionsAndIrrelevantFields(t *testing.T) {
	cases := map[string]string{
		"number with text":                  HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_number":18,"value_text":"x"}`,
		"number with boolean":               HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_number":18,"value_boolean":true}`,
		"boolean with number":               HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"boolean","value_boolean":true,"value_number":1}`,
		"string with number":                HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","value_number":1}`,
		"reference with text":               HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"owns","value_kind":"reference","value_reference":"r-1","value_text":"x"}`,
		"forget with claim_id":              HumanCommandPrefix + `{"action":"forget","subject":"api","claim_id":"kclaim_1"}`,
		"forget with value":                 HumanCommandPrefix + `{"action":"forget","subject":"api","value_kind":"string","value_text":"x"}`,
		"dispute with preference":           HumanCommandPrefix + `{"action":"dispute","claim_id":"kclaim_1","expected_revision":1,"preference_key":"k"}`,
		"remember claim with revision":      HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","expected_revision":1}`,
		"inspect with empty subject":        HumanCommandPrefix + `{"action":"inspect","subject":""}`,
		"correct with scope":                HumanCommandPrefix + `{"action":"correct","claim_id":"kclaim_1","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"p"}`,
		"correct with revision":             HumanCommandPrefix + `{"action":"correct","claim_id":"kclaim_1","value_kind":"string","value_text":"x","expected_revision":2}`,
		"archive document without revision": HumanCommandPrefix + `{"action":"archive","document_id":"kdoc_1"}`,
		"preference remember with scope":    HumanCommandPrefix + `{"action":"remember","preference_key":"k","value_kind":"string","value_text":"x","scope_kind":"user"}`,
	}
	for name, text := range cases {
		if _, matched, err := ParseHumanCommand(text); !matched || err == nil {
			t.Errorf("%s: matched = %v, err = %v, want strict rejection", name, matched, err)
		}
	}
	valid := []string{
		HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_number":18}`,
		HumanCommandPrefix + `{"action":"remember","preference_key":"k","value_kind":"boolean","value_boolean":true}`,
		HumanCommandPrefix + `{"action":"correct","claim_id":"kclaim_1","value_kind":"reference","value_reference":"r-1","predicate":"owns"}`,
		HumanCommandPrefix + `{"action":"inspect"}`,
		HumanCommandPrefix + `{"action":"inspect","document_id":"kdoc_1"}`,
		HumanCommandPrefix + `{"action":"inspect","subject":"api"}`,
	}
	for _, text := range valid {
		if _, matched, err := ParseHumanCommand(text); !matched || err != nil {
			t.Errorf("valid command %q rejected: %v", text, err)
		}
	}
}

func TestInspectListsClaimsWithRediscoverableIdentities(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	if _, _, err := service.Execute(t.Context(), binding, "evt-1", rememberText("api")); err != nil {
		t.Fatal(err)
	}
	claimID := store.createdClaims[0].ID
	_, listing, err := service.Execute(t.Context(), binding, "evt-2", HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "claim "+string(claimID)+": api is pg-01") {
		t.Fatalf("listing must rediscover claim identities: %q", listing)
	}
	store.documents["kdoc_1"] = domain.KnowledgeDocument{
		ID: "kdoc_1", Subject: "runbook", ScopeKind: domain.KnowledgeScopeUser, ScopeID: testOwnerKey,
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "handle", Status: domain.KnowledgeDocumentActive, Revision: 2,
	}
	_, detail, err := service.Execute(t.Context(), binding, "evt-3", HumanCommandPrefix+`{"action":"inspect","document_id":"kdoc_1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "runbook") || !strings.Contains(detail, "revision 2") {
		t.Fatalf("document detail = %q", detail)
	}
}

func TestInspectListingOmitsForeignScopes(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	foreign := domain.KnowledgeClaim{
		ID: "kclaim_foreign", Subject: "db", Predicate: domain.KnowledgePredicateRunsOn,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "oracle"},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "elsewhere",
	}
	store.claims[foreign.ID] = foreign
	_, listing, err := service.Execute(t.Context(), binding, "evt-1", HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, "kclaim_foreign") {
		t.Fatalf("listing leaked a foreign-scope claim: %q", listing)
	}
}

func TestCommandReceiptTargetsCarryDigestsNotPlaintext(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	subject := "database"
	if _, _, err := service.Execute(t.Context(), binding, "evt-1", rememberText(subject)); err != nil {
		t.Fatal(err)
	}
	claimReceipt := store.receipts["slack-human:evt-1"]
	if strings.Contains(claimReceipt.Target, subject) {
		t.Fatalf("claim receipt target carries plaintext subject: %q", claimReceipt.Target)
	}
	if claimReceipt.Target != "claim:"+domain.KnowledgeSubjectDigest(subject, domain.KnowledgeScopeUser, testOwnerKey) {
		t.Fatalf("claim receipt target = %q, want digest target", claimReceipt.Target)
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-2",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatal(err)
	}
	forgetReceipt := store.receipts["slack-human:evt-2"]
	if strings.Contains(forgetReceipt.Target, subject) {
		t.Fatalf("forget receipt target carries plaintext subject: %q", forgetReceipt.Target)
	}
	if forgetReceipt.Target != "subject:"+domain.KnowledgeSubjectDigest(subject, domain.KnowledgeScopeUser, testOwnerKey) {
		t.Fatalf("forget receipt target = %q, want digest target", forgetReceipt.Target)
	}
}

func TestRejectedForgetDoesNotConsumeCommandIdentity(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	if _, _, err := service.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+`{"action":"forget","subject":"password: hunter2secret"}`); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("credential subject error = %v, want ErrKnowledgeValidation", err)
	}
	if _, ok := store.receipts["slack-human:evt-1"]; ok {
		t.Fatal("rejected forget consumed the command identity")
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+`{"action":"forget","subject":"database"}`); err != nil {
		t.Fatalf("retry after rejected forget failed: %v", err)
	}
}

func TestParseRejectsExplicitlyInvalidPresentFields(t *testing.T) {
	cases := map[string]string{
		"negative revision on remember":       HumanCommandPrefix + `{"action":"remember","preference_key":"k","value_kind":"string","value_text":"x","expected_revision":-1}`,
		"zero revision on remember":           HumanCommandPrefix + `{"action":"remember","preference_key":"k","value_kind":"string","value_text":"x","expected_revision":0}`,
		"negative revision on claim remember": HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","expected_revision":-1}`,
		"empty claim_id on forget":            HumanCommandPrefix + `{"action":"forget","subject":"api","claim_id":""}`,
		"empty text on number value":          HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"number","value_number":18,"value_text":""}`,
		"null number on string value":         HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","value_number":null}`,
		"scope id without kind":               HumanCommandPrefix + `{"action":"forget","subject":"api","scope_id":"proj-a"}`,
		"scope kind without id":               HumanCommandPrefix + `{"action":"forget","subject":"api","scope_kind":"project"}`,
		"value fields without kind":           HumanCommandPrefix + `{"action":"forget","subject":"api","value_text":"x"}`,
	}
	for name, text := range cases {
		if _, matched, err := ParseHumanCommand(text); !matched || err == nil {
			t.Errorf("%s: matched = %v, err = %v, want strict rejection", name, matched, err)
		}
	}
}

func TestInspectListingTruncatesWithIndicator(t *testing.T) {
	store := newFakeKnowledgeStore()
	service, err := New(Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxClaimsListing: 2}}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	for _, subject := range []string{"a", "b", "c"} {
		if _, _, err := service.Execute(t.Context(), binding, fmt.Sprintf("evt-%s", subject), rememberText(subject)); err != nil {
			t.Fatal(err)
		}
	}
	_, listing, err := service.Execute(t.Context(), binding, "evt-listing", HumanCommandPrefix+`{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "claim listing truncated at 2 items") {
		t.Fatalf("listing missing truncation indicator: %q", listing)
	}
	for _, subject := range []string{"a", "b"} {
		if !strings.Contains(listing, subject+" is pg-01") {
			t.Fatalf("listing missing bounded claim %s: %q", subject, listing)
		}
	}
	if strings.Contains(listing, "c is pg-01") {
		t.Fatalf("listing exceeded the bound: %q", listing)
	}
}

func TestForgetRespectsConfiguredLimits(t *testing.T) {
	store := newFakeKnowledgeStore()
	service, err := New(Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxSubjectRunes: 512}}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	subject := strings.Repeat("s", 300)
	if _, _, err := service.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"x"}`, subject)); err != nil {
		t.Fatalf("amplified remember rejected: %v", err)
	}
	if _, _, err := service.Execute(t.Context(), binding, "evt-2",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatalf("amplified forget rejected: %v", err)
	}
	if len(store.forgets) != 1 || store.forgets[0] != subject+":user:"+testOwnerKey+":slack-human:evt-2" {
		t.Fatalf("forget calls = %v", store.forgets)
	}
	if _, ok := store.receipts["slack-human:evt-2"]; !ok {
		t.Fatal("successful forget must carry a command receipt")
	}
	_, listing, err := service.Execute(t.Context(), binding, "evt-3",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"inspect","subject":"%s"}`, subject))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, subject+" is x") {
		t.Fatalf("forgotten subject still listed: %q", listing)
	}
}

func TestParseRejectsEmptySelectors(t *testing.T) {
	cases := map[string]string{
		"archive with empty claim_id":       HumanCommandPrefix + `{"action":"archive","claim_id":"","expected_revision":1}`,
		"archive with empty preference_key": HumanCommandPrefix + `{"action":"archive","preference_key":"","expected_revision":1}`,
		"archive with empty document_id":    HumanCommandPrefix + `{"action":"archive","document_id":"","expected_revision":1}`,
		"inspect with empty claim_id":       HumanCommandPrefix + `{"action":"inspect","claim_id":""}`,
		"inspect with empty document_id":    HumanCommandPrefix + `{"action":"inspect","document_id":"  "}`,
		"inspect with empty preference_key": HumanCommandPrefix + `{"action":"inspect","preference_key":""}`,
		"forget with empty scope kind":      HumanCommandPrefix + `{"action":"forget","subject":"api","scope_kind":"","scope_id":"p"}`,
		"forget with empty scope id":        HumanCommandPrefix + `{"action":"forget","subject":"api","scope_kind":"project","scope_id":""}`,
		"remember with empty scope id":      HumanCommandPrefix + `{"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":""}`,
		"inspect subject with target":       HumanCommandPrefix + `{"action":"inspect","subject":"api","claim_id":"kclaim_1"}`,
	}
	for name, text := range cases {
		if _, matched, err := ParseHumanCommand(text); !matched || err == nil {
			t.Errorf("%s: matched = %v, err = %v, want strict rejection", name, matched, err)
		}
	}
}

func TestInspectSubjectSelectorFiltersClaims(t *testing.T) {
	store := newFakeKnowledgeStore()
	service := newTestService(t, store, newCountingCoordinator())
	binding := testBinding()
	for _, subject := range []string{"api", "db"} {
		if _, _, err := service.Execute(t.Context(), binding, "evt-"+subject, rememberText(subject)); err != nil {
			t.Fatal(err)
		}
	}
	_, listing, err := service.Execute(t.Context(), binding, "evt-listing",
		HumanCommandPrefix+`{"action":"inspect","subject":"db"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "db is pg-01") || strings.Contains(listing, "api is pg-01") {
		t.Fatalf("subject-scoped listing = %q", listing)
	}
}

func TestForgetAcceptsHistoricallyPersistibleSubjectsAfterRestart(t *testing.T) {
	store := newFakeKnowledgeStore()
	amplified, err := New(Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxSubjectRunes: 512}}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	subject := strings.Repeat("s", 300)
	if _, _, err := amplified.Execute(t.Context(), binding, "evt-1",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"remember","subject":"%s","predicate":"is","value_kind":"string","value_text":"x"}`, subject)); err != nil {
		t.Fatal(err)
	}
	defaulted, err := New(Config{Enabled: true}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := defaulted.Execute(t.Context(), binding, "evt-2",
		HumanCommandPrefix+fmt.Sprintf(`{"action":"forget","subject":"%s"}`, subject)); err != nil {
		t.Fatalf("forget under default limits rejected: %v", err)
	}
	if len(store.forgets) != 1 {
		t.Fatalf("forget calls = %v, want 1", store.forgets)
	}
	if _, ok := store.receipts["slack-human:evt-2"]; !ok {
		t.Fatal("forget must carry a command receipt")
	}
}

func TestRememberRejectsEmptyPreferenceKey(t *testing.T) {
	cases := []string{
		HumanCommandPrefix + `{"action":"remember","preference_key":"","value_kind":"string","value_text":"x"}`,
		HumanCommandPrefix + `{"action":"remember","preference_key":null,"value_kind":"string","value_text":"x"}`,
		HumanCommandPrefix + `{"action":"remember","preference_key":"  ","value_kind":"string","value_text":"x"}`,
	}
	for _, text := range cases {
		if _, matched, err := ParseHumanCommand(text); !matched || err == nil {
			t.Errorf("empty preference_key %q accepted: matched=%v err=%v", text, matched, err)
		}
	}
}

func TestInspectSubjectSelectorEscapesGlobalListingBound(t *testing.T) {
	store := newFakeKnowledgeStore()
	service, err := New(Config{Enabled: true, Limits: domain.KnowledgeLimits{MaxClaimsListing: 2, MaxClaimsPerSubject: 32}}, Dependencies{Store: store, Coordinator: newCountingCoordinator()})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	for i := range 3 {
		subject := fmt.Sprintf("subject-%d", i)
		if _, _, err := service.Execute(t.Context(), binding, fmt.Sprintf("evt-s%d", i), rememberText(subject)); err != nil {
			t.Fatal(err)
		}
	}
	// Three claims of the SAME subject are created via three events: the
	// subject selector must list all of them, not the global-bound prefix.
	for i := range 3 {
		if _, _, err := service.Execute(t.Context(), binding, fmt.Sprintf("evt-same-%d", i),
			HumanCommandPrefix+`{"action":"remember","subject":"same","predicate":"is","value_kind":"string","value_text":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	_, listing, err := service.Execute(t.Context(), binding, "evt-listing",
		HumanCommandPrefix+`{"action":"inspect","subject":"same"}`)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(listing, "same is x"); count != 3 {
		t.Fatalf("subject-scoped listing shows %d claims of 3: %q", count, listing)
	}
	if strings.Contains(listing, "truncated") {
		t.Fatalf("subject-scoped listing truncated unexpectedly: %q", listing)
	}
}
