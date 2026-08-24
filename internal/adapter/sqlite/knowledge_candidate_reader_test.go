package sqlite

import (
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestKnowledgeCandidateReaderExactAuthorizesInsideSQL(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	seedRetrievalClaim(t, store, "kclaim_project", "db host", "is", "string", "db.internal", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_team_other", "other team secret", "is", "string", "value-two", "", "team", "T00000002", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_archived", "archived secret", "is", "string", "value-three", "", "project", "my-project", "archived", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_superseded", "superseded secret", "is", "string", "value-four", "", "project", "my-project", "superseded", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_expired", "expired secret", "is", "string", "value-five", "", "project", "my-project", "asserted", nowUnix-7200, nowUnix-3600, 1)
	seedRetrievalClaim(t, store, "kclaim_future", "future secret", "is", "string", "value-six", "", "project", "my-project", "asserted", nowUnix+3600, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_number", "port claim", "uses", "number", "42", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_reference", "service mesh", "owns", "reference", "", "gateway.internal", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_boolean", "feature flag", "is", "boolean", "true", "", "project", "my-project", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")

	tests := []struct {
		name    string
		query   string
		tokens  []string
		wantIDs []string
	}{
		{"subject match", "db host", nil, []string{"kclaim_project"}},
		{"string value match", "find db.internal please", []string{"db.internal"}, []string{"kclaim_project"}},
		{"number value match", "port 42", []string{"42"}, []string{"kclaim_number"}},
		{"reference value match", "where is gateway.internal", []string{"gateway.internal"}, []string{"kclaim_reference"}},
		{"boolean value match", "is the flag true", []string{"true"}, []string{"kclaim_boolean"}},
		{"cross-team zero", "other team secret", nil, nil},
		{"archived zero", "archived secret", nil, nil},
		{"superseded zero", "superseded secret", nil, nil},
		{"expired zero", "expired secret", nil, nil},
		{"future zero", "future secret", nil, nil},
		{"no match", "completely absent", nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), test.query, test.tokens)
			if err != nil {
				t.Fatalf("ReadExact() error = %v", err)
			}
			got := candidateIDs(candidates)
			if !equalStrings(got, test.wantIDs) {
				t.Fatalf("ReadExact() ids = %v, want %v", got, test.wantIDs)
			}
		})
	}
}

func TestKnowledgeCandidateReaderExactCrossScopesReturnZero(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	// User, project, conversation, workstream, and team scoped rows owned by
	// other identities must be indistinguishable from missing.
	seedRetrievalClaim(t, store, "kclaim_user_other", "user secret", "is", "string", "user-value", "", "user", "slack:T00000001:user:U99999999", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_project_other", "project secret", "is", "string", "project-value", "", "project", "other-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_conversation_other", "conversation secret", "is", "string", "conversation-value", "", "conversation", "slack:T00000001:dm:C99999999", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_workstream_other", "workstream secret", "is", "string", "workstream-value", "", "workstream", "ws-other", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_team_other", "team secret", "is", "string", "team-value", "", "team", "T00000002", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "ws-mine")

	for _, subject := range []string{"user secret", "project secret", "conversation secret", "workstream secret", "team secret"} {
		candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), subject, nil)
		if err != nil {
			t.Fatalf("ReadExact(%q) error = %v", subject, err)
		}
		if len(candidates) != 0 {
			t.Fatalf("ReadExact(%q) = %v, want zero cross-scope candidates", subject, candidateIDs(candidates))
		}
	}
	// The same binding does read its own project and workstream scopes.
	seedRetrievalClaim(t, store, "kclaim_project_own", "own project secret", "is", "string", "own-project-value", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_workstream_own", "own workstream secret", "is", "string", "own-workstream-value", "", "workstream", "ws-mine", "asserted", nowUnix, 0, 1)
	for _, subject := range []string{"own project secret", "own workstream secret"} {
		candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), subject, nil)
		if err != nil {
			t.Fatalf("ReadExact(%q) error = %v", subject, err)
		}
		if len(candidates) != 1 {
			t.Fatalf("ReadExact(%q) = %v, want one candidate", subject, candidateIDs(candidates))
		}
	}
}

func TestKnowledgeCandidateReaderExactPreferencesRequireExactOwner(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	ownOwner := "slack:T00000001:user:U00000001"
	otherOwner := "slack:T00000001:user:U99999999"
	seedRetrievalPreference(t, store, ownOwner, "language", "string", "spanish", "active", 1)
	seedRetrievalPreference(t, store, otherOwner, "language", "string", "english", "active", 1)
	seedRetrievalPreference(t, store, ownOwner, "archived-key", "string", "archived-value", "archived", 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "", "")

	candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), "language", nil)
	if err != nil {
		t.Fatalf("ReadExact() error = %v", err)
	}
	got := candidateIDs(candidates)
	want := []string{"preference:1"}
	if !equalStrings(got, want) {
		t.Fatalf("ReadExact() ids = %v, want %v", got, want)
	}
	// Archived preferences and other-owner values are invisible.
	for _, query := range []string{"archived-key", "english"} {
		candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), query, nil)
		if err != nil {
			t.Fatalf("ReadExact(%q) error = %v", query, err)
		}
		if len(candidates) != 0 {
			t.Fatalf("ReadExact(%q) = %v, want zero", query, candidateIDs(candidates))
		}
	}
}

func TestKnowledgeCandidateReaderExactDocumentsScopeAndStatus(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	seedRetrievalDocument(t, store, "kdoc_global", "onboarding guide", "global", "", "active", "curated", "", 0)
	seedRetrievalDocument(t, store, "kdoc_team_other", "team guide", "team", "T00000002", "active", "curated", "", 0)
	seedRetrievalDocument(t, store, "kdoc_archived", "old guide", "global", "", "archived", "curated", "", 0)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "", "")

	candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), "onboarding guide", nil)
	if err != nil {
		t.Fatalf("ReadExact() error = %v", err)
	}
	got := candidateIDs(candidates)
	if !equalStrings(got, []string{"kdoc_global"}) {
		t.Fatalf("ReadExact() ids = %v, want [kdoc_global]", got)
	}
	for _, subject := range []string{"team guide", "old guide"} {
		candidates, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), subject, nil)
		if err != nil {
			t.Fatalf("ReadExact(%q) error = %v", subject, err)
		}
		if len(candidates) != 0 {
			t.Fatalf("ReadExact(%q) = %v, want zero", subject, candidateIDs(candidates))
		}
	}
}

func TestKnowledgeCandidateReaderExactAuthorizationBeforeLimit(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	// Unauthorized rows sort first alphabetically; the cap must apply
	// after authorization so they can never displace authorized rows.
	for i := range 5 {
		seedRetrievalClaim(t, store, "aaa_unauthorized_"+string(rune('a'+i)), "shared subject", "is", "string", "value-"+string(rune('a'+i)), "", "team", "T00000002", "asserted", nowUnix, 0, 1)
	}
	seedRetrievalClaim(t, store, "zzz_authorized", "shared subject", "is", "string", "value-last", "", "project", "my-project", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	limits := retrievalTestLimits()
	limits.MaxCandidatesPerChannel = 2

	candidates, err := reader.ReadExact(t.Context(), binding, now, limits, "shared subject", nil)
	if err != nil {
		t.Fatalf("ReadExact() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != "zzz_authorized" {
		t.Fatalf("ReadExact() = %v, want only the authorized row", candidateIDs(candidates))
	}
}

func TestKnowledgeCandidateReaderRelationOneHopOnly(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	// Seed: app owns db.internal; db.internal runs_on linux; linux located_in
	// rack-a. One hop from the app seed reaches db.internal only.
	seedRetrievalClaim(t, store, "kclaim_app", "app", "owns", "reference", "", "db.internal", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_db", "db.internal", "runs_on", "string", "linux", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_linux", "linux", "located_in", "string", "rack-a", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	// Cross-team endpoint must not leak the second hop's subject.
	seedRetrievalClaim(t, store, "kclaim_hidden", "db.internal", "is", "string", "hidden-value", "", "team", "T00000002", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")

	seeds, err := reader.ReadExact(t.Context(), binding, now, retrievalTestLimits(), "app", nil)
	if err != nil || len(seeds) != 1 {
		t.Fatalf("ReadExact() = %v, %v, want the app seed", candidateIDs(seeds), err)
	}
	related, err := reader.ReadRelated(t.Context(), binding, now, retrievalTestLimits(), seeds)
	if err != nil {
		t.Fatalf("ReadRelated() error = %v", err)
	}
	got := candidateIDs(related)
	if !equalStrings(got, []string{"kclaim_db"}) {
		t.Fatalf("ReadRelated() ids = %v, want [kclaim_db] (one hop only, cross-team hidden)", got)
	}
	// Non-relation seeds produce no expansion.
	seedCandidates := []port.KnowledgeEligibleCandidate{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_db"}}
	related, err = reader.ReadRelated(t.Context(), binding, now, retrievalTestLimits(), seedCandidates)
	if err != nil {
		t.Fatalf("ReadRelated(non-edge seed) error = %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("ReadRelated(non-edge seed) = %v, want zero", candidateIDs(related))
	}
}

func TestKnowledgeCandidateReaderRelationRechecksScopeAndStatus(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	seedRetrievalClaim(t, store, "kclaim_owner", "owner subject", "relates_to", "reference", "", "shared-ref", "project", "my-project", "asserted", nowUnix, 0, 1)
	// The result claims referencing the endpoint must pass status and scope
	// again inside SQL.
	seedRetrievalClaim(t, store, "kclaim_archived_target", "target subject", "relates_to", "reference", "", "shared-ref", "project", "my-project", "archived", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_foreign_target", "target subject", "relates_to", "reference", "", "shared-ref", "team", "T00000002", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_ok_target", "target subject", "relates_to", "reference", "", "shared-ref", "project", "my-project", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	seeds := []port.KnowledgeEligibleCandidate{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_owner"}}
	related, err := reader.ReadRelated(t.Context(), binding, now, retrievalTestLimits(), seeds)
	if err != nil {
		t.Fatalf("ReadRelated() error = %v", err)
	}
	got := candidateIDs(related)
	if !equalStrings(got, []string{"kclaim_ok_target"}) {
		t.Fatalf("ReadRelated() ids = %v, want only the eligible in-scope target", got)
	}
}

func TestKnowledgeCandidateReaderReadItemReauthorizes(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	seedRetrievalClaim(t, store, "kclaim_read", "readable claim", "is", "string", "readable-value", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_foreign", "foreign claim", "is", "string", "foreign-value", "", "team", "T00000002", "asserted", nowUnix, 0, 1)
	ownOwner := "slack:T00000001:user:U00000001"
	seedRetrievalPreference(t, store, ownOwner, "theme", "string", "dark", "active", 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")

	item, err := reader.ReadItem(t.Context(), binding, now, retrievalTestLimits(), domain.KnowledgeRetrievalClaim, "kclaim_read")
	if err != nil {
		t.Fatalf("ReadItem(claim) error = %v", err)
	}
	if item.Claim == nil || item.Claim.ID != "kclaim_read" {
		t.Fatalf("ReadItem(claim) = %+v", item)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("item.Validate() error = %v", err)
	}
	if _, err := reader.ReadItem(t.Context(), binding, now, retrievalTestLimits(), domain.KnowledgeRetrievalClaim, "kclaim_foreign"); err != port.ErrKnowledgeNotFound {
		t.Fatalf("ReadItem(foreign) error = %v, want ErrKnowledgeNotFound", err)
	}
	if _, err := reader.ReadItem(t.Context(), binding, now, retrievalTestLimits(), domain.KnowledgeRetrievalClaim, "missing"); err != port.ErrKnowledgeNotFound {
		t.Fatalf("ReadItem(missing) error = %v, want ErrKnowledgeNotFound", err)
	}
	preference, err := reader.ReadItem(t.Context(), binding, now, retrievalTestLimits(), domain.KnowledgeRetrievalPreference, "preference:1")
	if err != nil {
		t.Fatalf("ReadItem(preference) error = %v", err)
	}
	if preference.Preference == nil || preference.Preference.Key != "theme" {
		t.Fatalf("ReadItem(preference) = %+v", preference)
	}
	// Another owner must be indistinguishable from missing.
	otherBinding := retrievalTestBinding("T00000001", "U99999999", "my-project", "")
	if _, err := reader.ReadItem(t.Context(), otherBinding, now, retrievalTestLimits(), domain.KnowledgeRetrievalPreference, "preference:1"); err != port.ErrKnowledgeNotFound {
		t.Fatalf("ReadItem(other owner) error = %v, want ErrKnowledgeNotFound", err)
	}
}

func TestKnowledgeCandidateReaderRejectsInvalidReadInputs(t *testing.T) {
	store, _ := newTestStore(t)
	reader := NewKnowledgeCandidateReader(store)
	now := retrievalTestNow()
	binding := retrievalTestBinding("T00000001", "U00000001", "", "")
	limits := retrievalTestLimits()

	invalidBindings := []domain.KnowledgeWriteBinding{
		{Team: "not-plausible", Actor: "U00000001", Conversation: "slack:T00000001:dm:C00000001"},
		{Team: "T00000001", Actor: "bad", Conversation: "slack:T00000001:dm:C00000001"},
		{Team: "T00000001", Actor: "U00000001", Conversation: "not-a-key"},
		{Team: "T99999999", Actor: "U00000001", Conversation: "slack:T00000001:dm:C00000001"},
	}
	for _, bad := range invalidBindings {
		if _, err := reader.ReadExact(t.Context(), bad, now, limits, "query", nil); err == nil {
			t.Fatalf("ReadExact(bad binding %+v) succeeded", bad)
		}
	}
	if _, err := reader.ReadExact(t.Context(), binding, time.Time{}, limits, "query", nil); err == nil {
		t.Fatal("ReadExact(zero clock) succeeded")
	}
	badLimits := limits
	badLimits.MaxCandidatesPerChannel = domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel + 1
	if _, err := reader.ReadExact(t.Context(), binding, now, badLimits, "query", nil); err == nil {
		t.Fatal("ReadExact(invalid limits) succeeded")
	}
	if _, err := reader.ReadItem(t.Context(), binding, now, limits, "unknown", "x"); err == nil {
		t.Fatal("ReadItem(unknown kind) succeeded")
	}
	if _, err := reader.ReadItem(t.Context(), binding, now, limits, domain.KnowledgeRetrievalClaim, ""); err == nil {
		t.Fatal("ReadItem(empty id) succeeded")
	}
	if _, err := reader.ReadRelated(t.Context(), binding, now, limits, []port.KnowledgeEligibleCandidate{{Kind: "unknown", ID: "x"}}); err == nil {
		t.Fatal("ReadRelated(unknown seed kind) succeeded")
	}
}

func candidateIDs(candidates []port.KnowledgeEligibleCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestKnowledgeCandidateReaderRelationRejectsForeignSeeds pins FIND-088:
// relation seeds are re-verified in SQL against scope, status, and
// validity, so a cross-team seed can never expand a hop on behalf of the
// caller.
func TestKnowledgeCandidateReaderRelationRejectsForeignSeeds(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	// A foreign-team edge claim whose reference points at a local target.
	seedRetrievalClaim(t, store, "kclaim_foreign_seed", "foreign app", "owns", "reference", "", "local.target", "team", "T00000002", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_local_target", "local.target", "runs_on", "string", "linux", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	// A stale seed (archived after the exact read) must not expand either.
	seedRetrievalClaim(t, store, "kclaim_stale_seed", "stale app", "owns", "reference", "", "other.target", "project", "my-project", "archived", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_other_target", "other.target", "runs_on", "string", "linux", "", "project", "my-project", "asserted", nowUnix, 0, 1)

	reader := NewKnowledgeCandidateReader(store)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	seeds := []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_foreign_seed"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_stale_seed"},
	}
	related, err := reader.ReadRelated(t.Context(), binding, now, retrievalTestLimits(), seeds)
	if err != nil {
		t.Fatalf("ReadRelated() error = %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("ReadRelated(foreign/stale seeds) = %v, want zero expansion", candidateIDs(related))
	}
}
