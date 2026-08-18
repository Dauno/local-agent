package domain_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testKnowledgeClaim() domain.KnowledgeClaim {
	return domain.KnowledgeClaim{
		ID:          "claim_0001",
		Subject:     "api",
		Predicate:   domain.KnowledgePredicateRunsOn,
		Value:       domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 17"},
		ScopeKind:   domain.KnowledgeScopeProject,
		ScopeID:     "local-agent",
		SourceClass: domain.KnowledgeSourceHuman,
		SourceRef:   "slack-human:evt-1",
		AuthorID:    "U00000001",
		Status:      domain.KnowledgeClaimAsserted,
		Revision:    1,
	}
}

func testKnowledgeBinding() domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team:         "T00000001",
		Actor:        "U00000001",
		Conversation: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		Project:      "local-agent",
		WorkstreamID: "ws_0001",
	}
}

func testKnowledgeNow() time.Time {
	return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
}

// rawCuratorSource is the historical V1 source class name. The class was
// removed in the checkpoint 7 closure audit: no automatic scoped curator
// exists in V1, so the raw value must fail everywhere with
// ErrKnowledgeInvalidSource.
const rawCuratorSource = domain.KnowledgeSourceClass("curator")

func rawCuratorClaim(claim domain.KnowledgeClaim) domain.KnowledgeClaim {
	claim.SourceClass = rawCuratorSource
	claim.SourceRef = "exchange-1"
	claim.AuthorID = ""
	claim.Status = domain.KnowledgeClaimAsserted
	return claim
}

func TestKnowledgeCuratorSourceClassIsRejectedEverywhere(t *testing.T) {
	binding := testKnowledgeBinding()
	claim := rawCuratorClaim(testKnowledgeClaim())
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator persisted validation error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator candidate error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := claim.TransitionStatus(domain.KnowledgeClaimVerified, rawCuratorSource); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator transition source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	for _, scope := range []domain.KnowledgeScopeKind{domain.KnowledgeScopeProject, domain.KnowledgeScopeUser, domain.KnowledgeScopeWorkstream} {
		if err := domain.ValidateKnowledgeScopeWritable(rawCuratorSource, scope); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
			t.Fatalf("curator scope write to %s error = %v, want ErrKnowledgeInvalidSource", scope, err)
		}
	}
	prior := testKnowledgeClaim()
	correction := rawCuratorClaim(domain.KnowledgeClaim{
		ID: "claim_0002", Subject: prior.Subject, Predicate: prior.Predicate,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind: prior.ScopeKind, ScopeID: prior.ScopeID,
		Status: domain.KnowledgeClaimAsserted, SupersedesID: prior.ID, Revision: 1,
	})
	if err := prior.Correct(correction, rawCuratorSource, domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator correction error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if prior.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by a curator correction", prior.Status)
	}
}

func TestKnowledgeClaimScopeWritableMatrix(t *testing.T) {
	if err := domain.ValidateKnowledgeScopeWritable(domain.KnowledgeSourceHuman, domain.KnowledgeScopeProject); err != nil {
		t.Fatalf("human project write rejected: %v", err)
	}
	if err := domain.ValidateKnowledgeScopeWritable(domain.KnowledgeSourceHuman, domain.KnowledgeScopeUser); err != nil {
		t.Fatalf("human user write rejected: %v", err)
	}
	if err := domain.ValidateKnowledgeScopeWritable(rawCuratorSource, domain.KnowledgeScopeProject); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator project write error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := domain.ValidateKnowledgeScopeWritable(domain.KnowledgeSourceDecision, domain.KnowledgeScopeWorkstream); err != nil {
		t.Fatalf("decision workstream write rejected: %v", err)
	}
	rejected := []struct {
		source domain.KnowledgeSourceClass
		scope  domain.KnowledgeScopeKind
	}{
		{domain.KnowledgeSourceHuman, domain.KnowledgeScopeGlobal},
		{domain.KnowledgeSourceHuman, domain.KnowledgeScopeTeam},
		{domain.KnowledgeSourceHuman, domain.KnowledgeScopeConversation},
		{domain.KnowledgeSourceHuman, domain.KnowledgeScopeWorkstream},
		{domain.KnowledgeSourceDecision, domain.KnowledgeScopeProject},
		{domain.KnowledgeSourceRoot, domain.KnowledgeScopeProject},
		{domain.KnowledgeSourceWorker, domain.KnowledgeScopeUser},
		{domain.KnowledgeSourceObservation, domain.KnowledgeScopeProject},
	}
	for _, candidate := range rejected {
		if err := domain.ValidateKnowledgeScopeWritable(candidate.source, candidate.scope); !errors.Is(err, domain.ErrKnowledgeScopeNotWritable) {
			t.Errorf("write %s to %s error = %v, want ErrKnowledgeScopeNotWritable", candidate.source, candidate.scope, err)
		}
	}
}

func TestKnowledgeWriteBindingEnforcesTrustedIdentity(t *testing.T) {
	binding := testKnowledgeBinding()

	claim := testKnowledgeClaim()
	claim.ScopeKind = domain.KnowledgeScopeProject
	claim.ScopeID = "other-project"
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("cross-project claim error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}

	claim = testKnowledgeClaim()
	claim.ScopeKind = domain.KnowledgeScopeUser
	claim.ScopeID = "slack:T00000001:user:U9999"
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("foreign user scope error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
	claim.ScopeID = domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); err != nil {
		t.Fatalf("own user scope rejected: %v", err)
	}

	claim = testKnowledgeClaim()
	claim.ScopeKind = domain.KnowledgeScopeWorkstream
	claim.ScopeID = "ws_other"
	claim.SourceClass = domain.KnowledgeSourceDecision
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("foreign workstream scope error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}

	claim = testKnowledgeClaim()
	claim.ScopeKind = domain.KnowledgeScopeUser
	claim.ScopeID = "slack:T00000001:user:U00000001"
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), domain.KnowledgeWriteBinding{}); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("empty binding error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
}

func TestKnowledgeHumanCandidatesBindTheTrustedActor(t *testing.T) {
	binding := testKnowledgeBinding()
	claim := testKnowledgeClaim()
	claim.AuthorID = "U9999"
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("foreign author error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
	claim.AuthorID = ""
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("missing author error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
	claim.AuthorID = binding.Actor
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); err != nil {
		t.Fatalf("bound author rejected: %v", err)
	}

	curator := rawCuratorClaim(claim)
	curator.AuthorID = "U00000001"
	if err := curator.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator with author error = %v, want ErrKnowledgeInvalidSource", err)
	}
}

func TestKnowledgeReadBindingEnforcesScopeIsolation(t *testing.T) {
	binding := testKnowledgeBinding()
	allowed := []struct {
		kind domain.KnowledgeScopeKind
		id   string
	}{
		{domain.KnowledgeScopeGlobal, ""},
		{domain.KnowledgeScopeTeam, "T00000001"},
		{domain.KnowledgeScopeUser, "slack:T00000001:user:U00000001"},
		{domain.KnowledgeScopeProject, "local-agent"},
		{domain.KnowledgeScopeConversation, "slack:T00000001:dm:D00000001"},
		{domain.KnowledgeScopeWorkstream, "ws_0001"},
	}
	for _, candidate := range allowed {
		if err := domain.ValidateKnowledgeReadBinding(candidate.kind, candidate.id, binding); err != nil {
			t.Errorf("read %s:%s denied: %v", candidate.kind, candidate.id, err)
		}
	}
	denied := []struct {
		kind domain.KnowledgeScopeKind
		id   string
	}{
		{domain.KnowledgeScopeTeam, "T99999999"},
		{domain.KnowledgeScopeUser, "slack:T00000001:user:U9999"},
		{domain.KnowledgeScopeProject, "other-project"},
		{domain.KnowledgeScopeConversation, "slack:T00000001:dm:D99999999"},
		{domain.KnowledgeScopeWorkstream, "ws_other"},
	}
	for _, candidate := range denied {
		if err := domain.ValidateKnowledgeReadBinding(candidate.kind, candidate.id, binding); !errors.Is(err, domain.ErrKnowledgeReadNotAllowed) {
			t.Errorf("read %s:%s error = %v, want ErrKnowledgeReadNotAllowed", candidate.kind, candidate.id, err)
		}
	}
	empty := domain.KnowledgeWriteBinding{}
	for _, kind := range []domain.KnowledgeScopeKind{
		domain.KnowledgeScopeTeam, domain.KnowledgeScopeUser, domain.KnowledgeScopeProject,
		domain.KnowledgeScopeConversation, domain.KnowledgeScopeWorkstream,
	} {
		if err := domain.ValidateKnowledgeReadBinding(kind, "anything", empty); !errors.Is(err, domain.ErrKnowledgeReadNotAllowed) {
			t.Errorf("unbound read %s error = %v, want ErrKnowledgeReadNotAllowed", kind, err)
		}
	}
}

func TestKnowledgeClaimGlobalScopeRejectsIdentityAndWrites(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.ScopeKind = domain.KnowledgeScopeGlobal
	claim.ScopeID = "local-agent"
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidScope) {
		t.Fatalf("global scope with identity error = %v, want ErrKnowledgeInvalidScope", err)
	}
	claim.ScopeID = ""
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeScopeNotWritable) {
		t.Fatalf("global claim write error = %v, want ErrKnowledgeScopeNotWritable", err)
	}

	claim = testKnowledgeClaim()
	claim.ScopeID = ""
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeScopeIdentityRequired) {
		t.Fatalf("project scope without identity error = %v, want ErrKnowledgeScopeIdentityRequired", err)
	}
}

func TestKnowledgeCandidateAdmissionRestrictsStatus(t *testing.T) {
	binding := testKnowledgeBinding()

	curator := rawCuratorClaim(testKnowledgeClaim())
	for _, status := range []domain.KnowledgeClaimStatus{
		domain.KnowledgeClaimAsserted, domain.KnowledgeClaimVerified, domain.KnowledgeClaimDisputed,
		domain.KnowledgeClaimSuperseded, domain.KnowledgeClaimArchived, domain.KnowledgeClaimExpired,
	} {
		curator.Status = status
		if err := curator.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
			t.Errorf("curator candidate with status %s error = %v, want ErrKnowledgeInvalidSource", status, err)
		}
	}

	human := testKnowledgeClaim()
	human.Status = domain.KnowledgeClaimVerified
	if err := human.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); err != nil {
		t.Fatalf("verified human candidate rejected: %v", err)
	}
	human.Status = domain.KnowledgeClaimSuperseded
	if err := human.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeStatusTransition) {
		t.Fatalf("superseded candidate error = %v, want ErrKnowledgeStatusTransition", err)
	}
}

func TestKnowledgePersistedValidationAllowsPromotedClaims(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.SourceRef = "exchange-1"
	if err := claim.TransitionStatus(domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("human promotion rejected: %v", err)
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("promoted claim failed persisted validation: %v", err)
	}
	if err := claim.TransitionStatus(domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("dispute rejected: %v", err)
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("disputed promoted claim failed persisted validation: %v", err)
	}
}

func TestKnowledgeModelSourcesCannotPromoteToVerified(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.Status = domain.KnowledgeClaimVerified
	claim.SourceClass = rawCuratorSource
	claim.AuthorID = ""
	if err := claim.ValidateCandidate(domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator verified candidate error = %v, want ErrKnowledgeInvalidSource", err)
	}
	for _, source := range []domain.KnowledgeSourceClass{
		domain.KnowledgeSourceRoot, domain.KnowledgeSourceWorker,
		domain.KnowledgeSourceObservation,
	} {
		if got := source.MaxKnowledgeClaimStatus(); got != domain.KnowledgeClaimAsserted {
			t.Errorf("source %s max status = %s, want asserted (V1 verified-observation set is empty)", source, got)
		}
	}
	for _, source := range []domain.KnowledgeSourceClass{
		domain.KnowledgeSourceHuman, domain.KnowledgeSourceDecision,
	} {
		if got := source.MaxKnowledgeClaimStatus(); got != domain.KnowledgeClaimVerified {
			t.Errorf("source %s max status = %s, want verified", source, got)
		}
	}

	observation := testKnowledgeClaim()
	observation.SourceClass = domain.KnowledgeSourceObservation
	observation.Status = domain.KnowledgeClaimVerified
	if err := observation.ValidateCandidate(domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeScopeNotWritable) {
		t.Fatalf("observation candidate error = %v, want ErrKnowledgeScopeNotWritable (empty V1 set)", err)
	}
}

func TestKnowledgeClaimStatusTransitionsEnforceAuthority(t *testing.T) {
	claim := testKnowledgeClaim()
	if err := claim.TransitionStatus(domain.KnowledgeClaimVerified, rawCuratorSource); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator verify error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := claim.TransitionStatus(domain.KnowledgeClaimVerified, "invented"); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("invalid source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := claim.TransitionStatus(domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("human verify rejected: %v", err)
	}
	if claim.Status != domain.KnowledgeClaimVerified {
		t.Fatalf("status after verify = %s, want verified", claim.Status)
	}

	verified := claim
	if err := verified.TransitionStatus(domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("human dispute rejected: %v", err)
	}
	if verified.Status != domain.KnowledgeClaimDisputed {
		t.Fatalf("status after dispute = %s, want disputed", verified.Status)
	}

	curatorClaim := testKnowledgeClaim()
	if err := curatorClaim.TransitionStatus(domain.KnowledgeClaimDisputed, rawCuratorSource); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator dispute error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := curatorClaim.TransitionStatus(domain.KnowledgeClaimArchived, rawCuratorSource); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator archive error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := curatorClaim.TransitionStatus(domain.KnowledgeClaimSuperseded, domain.KnowledgeSourceHuman); !errors.Is(err, domain.ErrKnowledgeStatusTransition) {
		t.Fatalf("direct supersede error = %v, want ErrKnowledgeStatusTransition", err)
	}

	if err := curatorClaim.TransitionStatus(domain.KnowledgeClaimExpired, domain.KnowledgeSourceHuman); !errors.Is(err, domain.ErrKnowledgeStatusTransition) {
		t.Fatalf("explicit expiry error = %v, want ErrKnowledgeStatusTransition", err)
	}
}

func TestKnowledgeArchivedIsTerminalAndHumanOnly(t *testing.T) {
	archived := testKnowledgeClaim()
	if err := archived.TransitionStatus(domain.KnowledgeClaimArchived, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("archive rejected: %v", err)
	}
	if err := archived.TransitionStatus(domain.KnowledgeClaimVerified, domain.KnowledgeSourceHuman); !errors.Is(err, domain.ErrKnowledgeStatusTransition) {
		t.Fatalf("archived to verified error = %v, want ErrKnowledgeStatusTransition", err)
	}
}

func TestKnowledgeCorrectionSupersedesPriorClaimWithoutDeletingIt(t *testing.T) {
	prior := testKnowledgeClaim()
	correction := domain.KnowledgeClaim{
		ID:           "claim_0002",
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
		Revision:     1,
	}
	if err := prior.Correct(correction, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); err != nil {
		t.Fatalf("correction rejected: %v", err)
	}
	if prior.Status != domain.KnowledgeClaimSuperseded {
		t.Fatalf("prior status = %s, want superseded", prior.Status)
	}
	if prior.Value.Text != "PostgreSQL 17" {
		t.Fatalf("prior value mutated to %q; provenance must be preserved", prior.Value.Text)
	}
	if prior.SourceRef != "slack-human:evt-1" {
		t.Fatalf("prior provenance mutated to %q", prior.SourceRef)
	}
	if err := prior.Correct(correction, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); err != nil {
		t.Fatalf("idempotent replay rejected: %v", err)
	}

	unrelated := correction
	unrelated.Subject = "other-system"
	if err := prior.Correct(unrelated, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeSubjectMismatch) {
		t.Fatalf("subject mismatch error = %v, want ErrKnowledgeSubjectMismatch", err)
	}
	otherScope := correction
	otherScope.ScopeID = "other-project"
	if err := prior.Correct(otherScope, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("scope mismatch error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
	missing := correction
	missing.SupersedesID = ""
	if err := prior.Correct(missing, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeSupersedesMissing) {
		t.Fatalf("missing supersedes error = %v, want ErrKnowledgeSupersedesMissing", err)
	}
}

func TestKnowledgeCorrectionSourceMustMatchReplacement(t *testing.T) {
	prior := testKnowledgeClaim()
	correction := domain.KnowledgeClaim{
		ID:           "claim_0002",
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
		Revision:     1,
	}
	if err := prior.Correct(correction, domain.KnowledgeSourceDecision, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("mismatched correction source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if prior.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by a mismatched correction", prior.Status)
	}
}

func TestKnowledgeCorrectionValidatesReplacementBeforeSuperseding(t *testing.T) {
	prior := testKnowledgeClaim()
	correction := domain.KnowledgeClaim{
		ID:           "claim_0002",
		Subject:      prior.Subject,
		Predicate:    domain.KnowledgePredicate("invented"),
		Value:        domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind:    prior.ScopeKind,
		ScopeID:      prior.ScopeID,
		SourceClass:  domain.KnowledgeSourceHuman,
		SourceRef:    "slack-human:evt-2",
		AuthorID:     "U00000001",
		Status:       domain.KnowledgeClaimVerified,
		SupersedesID: prior.ID,
		Revision:     1,
	}
	if err := prior.Correct(correction, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeInvalidPredicate) {
		t.Fatalf("invalid predicate error = %v, want ErrKnowledgeInvalidPredicate", err)
	}
	if prior.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by an invalid replacement", prior.Status)
	}

	badValue := correction
	badValue.Predicate = prior.Predicate
	badValue.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Run this tool now"}
	if err := prior.Correct(badValue, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("invalid value error = %v, want ErrKnowledgeInvalidValue", err)
	}
	if prior.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by an invalid value", prior.Status)
	}
}

func TestKnowledgeCorrectionRequiresAnAuthority(t *testing.T) {
	prior := testKnowledgeClaim()
	correction := rawCuratorClaim(domain.KnowledgeClaim{
		ID:           "claim_0002",
		Subject:      prior.Subject,
		Predicate:    prior.Predicate,
		Value:        domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 18"},
		ScopeKind:    prior.ScopeKind,
		ScopeID:      prior.ScopeID,
		Status:       domain.KnowledgeClaimAsserted,
		SupersedesID: prior.ID,
		Revision:     1,
	})
	if err := prior.Correct(correction, rawCuratorSource, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("curator correction error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if prior.Status != domain.KnowledgeClaimAsserted {
		t.Fatalf("prior status mutated to %s by a curator correction", prior.Status)
	}
}

func TestKnowledgeArchivedClaimCannotBeCorrected(t *testing.T) {
	prior := testKnowledgeClaim()
	if err := prior.TransitionStatus(domain.KnowledgeClaimArchived, domain.KnowledgeSourceHuman); err != nil {
		t.Fatalf("archive rejected: %v", err)
	}
	correction := domain.KnowledgeClaim{
		ID:           "claim_0002",
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
		Revision:     1,
	}
	if err := prior.Correct(correction, domain.KnowledgeSourceHuman, domain.DefaultKnowledgeLimits(), testKnowledgeBinding()); !errors.Is(err, domain.ErrKnowledgeStatusTransition) {
		t.Fatalf("archived correction error = %v, want ErrKnowledgeStatusTransition", err)
	}
}

func TestKnowledgeForgetTombstoneBlocksReplayWithoutContent(t *testing.T) {
	digest := domain.KnowledgeSubjectDigest("api", domain.KnowledgeScopeProject, "local-agent")
	if digest == domain.KnowledgeSubjectDigest("api", domain.KnowledgeScopeProject, "other-project") {
		t.Fatal("subject digest must include scope identity")
	}
	if digest == domain.KnowledgeSubjectDigest("other", domain.KnowledgeScopeProject, "local-agent") {
		t.Fatal("subject digest must include the subject")
	}
	tombstone := domain.KnowledgeTombstone{
		SubjectDigest: digest,
		ScopeKind:     domain.KnowledgeScopeProject,
		ScopeID:       "local-agent",
		ForgottenAt:   time.Now().UTC(),
		SourceRef:     "slack-human:evt-3",
	}
	if err := tombstone.Validate(); err != nil {
		t.Fatalf("tombstone validation failed: %v", err)
	}
	if !tombstone.Blocks("api", domain.KnowledgeScopeProject, "local-agent") {
		t.Fatal("tombstone must block its own subject and scope")
	}
	if tombstone.Blocks("other", domain.KnowledgeScopeProject, "local-agent") {
		t.Fatal("tombstone must not block other subjects")
	}
	if tombstone.Blocks("api", domain.KnowledgeScopeProject, "other-project") {
		t.Fatal("tombstone must not block other projects with the same subject")
	}
	invalid := tombstone
	invalid.SubjectDigest = "not-hex"
	if err := invalid.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidTombstoneDigest) {
		t.Fatalf("invalid digest error = %v, want ErrKnowledgeInvalidTombstoneDigest", err)
	}
	credential := tombstone
	credential.SourceRef = "password = xoxb-abcdef1234567890"
	if err := credential.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("credential source ref error = %v, want ErrKnowledgeInvalidSource", err)
	}
}

func TestKnowledgeTombstoneEnforcesScope(t *testing.T) {
	tombstone := domain.KnowledgeTombstone{
		SubjectDigest: strings.Repeat("a", 64),
		ScopeKind:     domain.KnowledgeScopeUser,
		ForgottenAt:   time.Now().UTC(),
		SourceRef:     "slack-human:evt-3",
	}
	if err := tombstone.Validate(); !errors.Is(err, domain.ErrKnowledgeScopeIdentityRequired) {
		t.Fatalf("identity scope without ID error = %v, want ErrKnowledgeScopeIdentityRequired", err)
	}
	tombstone.ScopeKind = domain.KnowledgeScopeGlobal
	tombstone.ScopeID = "slack:T00000001:user:U0001"
	if err := tombstone.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidScope) {
		t.Fatalf("global scope with identity error = %v, want ErrKnowledgeInvalidScope", err)
	}
}

func TestKnowledgePredicateValueKindsAreEnforced(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueReference, Reference: "ref_0001"}
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("scalar predicate with reference error = %v, want ErrKnowledgeInvalidValue", err)
	}
	referenceClaim := testKnowledgeClaim()
	referenceClaim.Predicate = domain.KnowledgePredicateOwns
	referenceClaim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "team"}
	if err := referenceClaim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("owns with scalar error = %v, want ErrKnowledgeInvalidValue", err)
	}
	referenceClaim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueReference, Reference: "workstream_1"}
	if err := referenceClaim.Validate(); err != nil {
		t.Fatalf("owns with reference rejected: %v", err)
	}

	nan := testKnowledgeClaim()
	nan.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueNumber, Number: math.NaN()}
	if err := nan.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("NaN value error = %v, want ErrKnowledgeInvalidValue", err)
	}
}

func TestKnowledgeValueIsAnEnforcedTaggedUnion(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL", Reference: "hidden"}
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("string with reference error = %v, want ErrKnowledgeInvalidValue", err)
	}
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueNumber, Number: 5, Text: "hidden"}
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("number with text error = %v, want ErrKnowledgeInvalidValue", err)
	}
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueBoolean, Boolean: true, Number: 5}
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("boolean with number error = %v, want ErrKnowledgeInvalidValue", err)
	}
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueBoolean, Boolean: false}
	if err := claim.Validate(); err != nil {
		t.Fatalf("false boolean rejected: %v", err)
	}
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueNumber, Number: 0}
	if err := claim.Validate(); err != nil {
		t.Fatalf("zero number rejected: %v", err)
	}
}

func TestKnowledgeClaimExpiryIsComputedNotWritten(t *testing.T) {
	now := testKnowledgeNow()
	claim := testKnowledgeClaim()
	claim.ValidFrom = now.Add(-time.Hour)
	claim.ValidUntil = now.Add(-time.Minute)
	if got := claim.EffectiveStatus(now); got != domain.KnowledgeClaimExpired {
		t.Fatalf("effective status = %s, want expired", got)
	}
	if got := claim.EffectiveStatus(now.Add(-2 * time.Hour)); got != domain.KnowledgeClaimAsserted {
		t.Fatalf("effective status before valid_from = %s, want asserted", got)
	}
	terminal := claim
	terminal.Status = domain.KnowledgeClaimArchived
	if got := terminal.EffectiveStatus(now); got != domain.KnowledgeClaimArchived {
		t.Fatalf("terminal claim effective status = %s, want archived", got)
	}
	reversed := testKnowledgeClaim()
	reversed.ValidFrom = now
	reversed.ValidUntil = now.Add(-time.Hour)
	if err := reversed.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("reversed validity error = %v, want ErrKnowledgeInvalidValue", err)
	}
}

func TestKnowledgeCardsAreSelectedAtomicallyWithinBudget(t *testing.T) {
	small := domain.CardFromClaim(domain.KnowledgeClaim{
		ID: "c1", Subject: "api", Predicate: domain.KnowledgePredicateRunsOn,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL"},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "e1",
		Status: domain.KnowledgeClaimAsserted,
	}, "lexical match", testKnowledgeNow())
	large := domain.CardFromClaim(domain.KnowledgeClaim{
		ID: "c2", Subject: strings.Repeat("x", 200), Predicate: domain.KnowledgePredicateIs,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: strings.Repeat("y", 500)},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "e2",
		Status: domain.KnowledgeClaimAsserted,
	}, "semantic match", testKnowledgeNow())

	smallCombinedCost := len([]rune(domain.RenderKnowledgeCards([]domain.KnowledgeCard{small})))
	selected, err := domain.FitKnowledgeCards([]domain.KnowledgeCard{small, large}, smallCombinedCost+10, nil)
	if err != nil {
		t.Fatalf("default cost selection failed: %v", err)
	}
	if len(selected) != 1 || selected[0].ClaimID != "c1" {
		t.Fatalf("budget selection = %v, want only the small card", selected)
	}
	selected, err = domain.FitKnowledgeCards([]domain.KnowledgeCard{small, large}, 2000, nil)
	if err != nil {
		t.Fatalf("generous selection failed: %v", err)
	}
	if len(selected) != 2 {
		t.Fatalf("generous budget selection count = %d, want 2", len(selected))
	}
	if selected, err = domain.FitKnowledgeCards([]domain.KnowledgeCard{small}, 0, nil); err != nil || len(selected) != 0 {
		t.Fatalf("empty budget selection = %v, %v; want none, nil", selected, err)
	}
	if selected, err = domain.FitKnowledgeCards([]domain.KnowledgeCard{small}, smallCombinedCost-1, nil); err != nil || len(selected) != 0 {
		t.Fatalf("too-small budget selection = %v, %v; want none, nil", selected, err)
	}
	if !strings.Contains(small.Render(), "lexical match") || !strings.Contains(small.Render(), "[project:local-agent") {
		t.Fatalf("rendered card omits retrieval reason or scope framing: %q", small.Render())
	}
}

func TestKnowledgeCardsUseCumulativeCostWithSharedFraming(t *testing.T) {
	card := domain.CardFromClaim(domain.KnowledgeClaim{
		ID: "c1", Subject: "api", Predicate: domain.KnowledgePredicateRunsOn,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL"},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "e1",
		Status: domain.KnowledgeClaimAsserted,
	}, "lexical match", testKnowledgeNow())
	combined := domain.RenderKnowledgeCards([]domain.KnowledgeCard{card})
	single := card.Runes()
	if len([]rune(combined)) <= single {
		t.Fatalf("combined rendering must include shared preamble and framing: %q", combined)
	}
	tokenCost := func(selected []domain.KnowledgeCard) (int, error) {
		return 10 * len(selected), nil
	}
	selected, err := domain.FitKnowledgeCards([]domain.KnowledgeCard{card}, 9, tokenCost)
	if err != nil || len(selected) != 0 {
		t.Fatalf("provider-shaped budget of 9 selected %v (%v), want none", selected, err)
	}
	selected, err = domain.FitKnowledgeCards([]domain.KnowledgeCard{card, card}, 15, tokenCost)
	if err != nil || len(selected) != 1 {
		t.Fatalf("provider-shaped budget of 15 selected %d cards (%v), want 1", len(selected), err)
	}
}

func TestKnowledgeCardSelectionFailsClosedWhenCostingErrors(t *testing.T) {
	card := domain.CardFromClaim(domain.KnowledgeClaim{
		ID: "c1", Subject: "api", Predicate: domain.KnowledgePredicateRunsOn,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL"},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "e1",
		Status: domain.KnowledgeClaimAsserted,
	}, "lexical match", testKnowledgeNow())
	failing := func([]domain.KnowledgeCard) (int, error) {
		return 0, errors.New("counter unavailable")
	}
	if selected, err := domain.FitKnowledgeCards([]domain.KnowledgeCard{card}, 100, failing); err == nil || selected != nil {
		t.Fatalf("failing counter selection = %v, %v; want nil, error", selected, err)
	}
}

func TestKnowledgeCardReflectsEffectiveValidity(t *testing.T) {
	now := testKnowledgeNow()
	claim := testKnowledgeClaim()
	claim.Status = domain.KnowledgeClaimVerified
	claim.ValidFrom = now.Add(-2 * time.Hour)
	claim.ValidUntil = now.Add(-time.Hour)
	card := domain.CardFromClaim(claim, "lexical match", now)
	if card.Status != domain.KnowledgeClaimExpired {
		t.Fatalf("expired card status = %s, want expired", card.Status)
	}
	if !strings.Contains(card.Render(), "from ") || !strings.Contains(card.Render(), "until ") {
		t.Fatalf("card rendering must include validity framing: %q", card.Render())
	}
	card = domain.CardFromClaim(claim, "lexical match", now.Add(-3*time.Hour))
	if card.Status != domain.KnowledgeClaimVerified {
		t.Fatalf("in-window card status = %s, want verified", card.Status)
	}
}

func TestKnowledgeActionsExcludeWorkstreamAndTaskMutations(t *testing.T) {
	for _, action := range []domain.KnowledgeAction{
		domain.KnowledgeActionRemember, domain.KnowledgeActionCorrect, domain.KnowledgeActionForget,
		domain.KnowledgeActionArchive, domain.KnowledgeActionDispute, domain.KnowledgeActionInspect,
	} {
		if err := domain.ValidateKnowledgeAction(action); err != nil {
			t.Errorf("action %s rejected: %v", action, err)
		}
	}
	for _, action := range []domain.KnowledgeAction{
		"start_task", "propose_task", "resolve_question", "create_workstream", "approve",
	} {
		if err := domain.ValidateKnowledgeAction(action); err == nil {
			t.Errorf("workstream action %q must not be a valid knowledge action", action)
		}
	}
}

func TestKnowledgePreferencesAreOwnerBoundScalars(t *testing.T) {
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U00000001", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4", Revision: 1,
	}
	if err := preference.Validate(); err != nil {
		t.Fatalf("preference validation failed: %v", err)
	}
	preference.OwnerKey = ""
	if err := preference.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("missing owner error = %v, want ErrKnowledgeInvalidValue", err)
	}
	preference.OwnerKey = "slack:T00000001:user:U00000001"
	preference.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueReference, Reference: "other"}
	if err := preference.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("reference preference error = %v, want ErrKnowledgeInvalidValue", err)
	}
	preference.Status = "pending"
	if err := preference.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("unknown preference status error = %v, want ErrKnowledgeInvalidValue", err)
	}
	preference.Status = domain.KnowledgePreferenceActive
	preference.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"}
	preference.SourceRef = "password = xoxb-abcdef1234567890"
	if err := preference.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("credential source ref error = %v, want ErrKnowledgeInvalidSource", err)
	}
}

func TestKnowledgePreferenceCandidateRequiresTrustedOwner(t *testing.T) {
	binding := testKnowledgeBinding()
	preference := domain.KnowledgePreference{
		OwnerKey: "slack:T00000001:user:U9999", Key: "language",
		Value:  domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status: domain.KnowledgePreferenceActive, SourceRef: "slack-human:evt-4", Revision: 1,
	}
	if err := preference.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); !errors.Is(err, domain.ErrKnowledgeScopeBindingMismatch) {
		t.Fatalf("foreign owner error = %v, want ErrKnowledgeScopeBindingMismatch", err)
	}
	preference.OwnerKey = domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	if err := preference.ValidateCandidate(domain.DefaultKnowledgeLimits(), binding); err != nil {
		t.Fatalf("trusted owner rejected: %v", err)
	}
}

func TestKnowledgeDocumentsRequireHandleDigestScopeAndProvenance(t *testing.T) {
	document := domain.KnowledgeDocument{
		ID: "doc_0001", Subject: "architecture-overview",
		ScopeKind: domain.KnowledgeScopeGlobal, ContentDigest: strings.Repeat("a", 64),
		ContentHandle: "mem_topic_12345",
		SourceID:      "mem_abc123",
		SourceRev:     3,
		Provenance:    domain.KnowledgeProvenanceLegacyCurated, Status: domain.KnowledgeDocumentActive,
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("legacy document validation failed: %v", err)
	}
	document.ContentDigest = strings.Repeat("a", 63)
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidDocumentDigest) {
		t.Fatalf("short digest error = %v, want ErrKnowledgeInvalidDocumentDigest", err)
	}
	document.ContentDigest = strings.Repeat("A", 64)
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidDocumentDigest) {
		t.Fatalf("uppercase digest error = %v, want ErrKnowledgeInvalidDocumentDigest", err)
	}
	document.ContentDigest = strings.Repeat("a", 64)
	document.Provenance = "invented"
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("unknown provenance error = %v, want ErrKnowledgeInvalidValue", err)
	}
	document.Provenance = domain.KnowledgeProvenanceLegacyCurated
	document.ScopeKind = domain.KnowledgeScopeUser
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeScopeIdentityRequired) {
		t.Fatalf("user scope without identity error = %v, want ErrKnowledgeScopeIdentityRequired", err)
	}
	document.ScopeKind = domain.KnowledgeScopeGlobal
	document.SourceID = ""
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("legacy without source identity error = %v, want ErrKnowledgeInvalidValue", err)
	}
	document.SourceID = "mem_abc123"
	document.SourceRev = 0
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("legacy without source revision error = %v, want ErrKnowledgeInvalidValue", err)
	}
	document.SourceRev = 3
	document.ContentHandle = ""
	if err := document.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("missing content handle error = %v, want ErrKnowledgeInvalidValue", err)
	}
	curated := domain.KnowledgeDocument{
		ID: "doc_0002", Subject: "overview", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "result_abc",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if err := curated.Validate(); err != nil {
		t.Fatalf("curated document rejected: %v", err)
	}
	curated.SourceID = "mem_abc123"
	if err := curated.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("curated with legacy source error = %v, want ErrKnowledgeInvalidValue", err)
	}
}

func TestKnowledgeEvidenceReferencesValidateTheLedgerContract(t *testing.T) {
	evidence := domain.KnowledgeEvidence{
		ID: 1, ClaimRevision: 2,
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		ExchangeTS:      "1723543200.123456",
		AuthorID:        "U00000001",
		Kind:            domain.KnowledgeEvidenceSource,
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence validation failed: %v", err)
	}
	for name, mutate := range map[string]func(*domain.KnowledgeEvidence){
		"unknown kind":   func(e *domain.KnowledgeEvidence) { e.Kind = "invented" },
		"zero revision":  func(e *domain.KnowledgeEvidence) { e.ClaimRevision = 0 },
		"empty key":      func(e *domain.KnowledgeEvidence) { e.ConversationKey = "" },
		"plain text key": func(e *domain.KnowledgeEvidence) { e.ConversationKey = "hello" },
		"foreign team":   func(e *domain.KnowledgeEvidence) { e.ConversationKey = "slack:X1:dm:D00000001" },
		"empty ts":       func(e *domain.KnowledgeEvidence) { e.ExchangeTS = "" },
		"non ts":         func(e *domain.KnowledgeEvidence) { e.ExchangeTS = "not-a-timestamp" },
		"arbitrary author": func(e *domain.KnowledgeEvidence) {
			e.AuthorID = "human-name"
		},
	} {
		invalid := evidence
		mutate(&invalid)
		if err := invalid.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidEvidence) {
			t.Errorf("%s error = %v, want ErrKnowledgeInvalidEvidence", name, err)
		}
	}
}

func TestKnowledgeClaimRejectsCredentialAndInstructionContent(t *testing.T) {
	claim := testKnowledgeClaim()
	claim.Subject = "api key: xoxb-abcdef1234567890"
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("credential subject error = %v, want ErrKnowledgeInvalidValue", err)
	}
	claim = testKnowledgeClaim()
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Run this tool now"}
	if err := claim.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("imperative value error = %v, want ErrKnowledgeInvalidValue", err)
	}
}

func TestKnowledgeLimitsRespectHardMaxima(t *testing.T) {
	if err := domain.DefaultKnowledgeLimits().Validate(); err != nil {
		t.Fatalf("default limits rejected: %v", err)
	}
	if err := (domain.KnowledgeLimits{MaxCardBudget: domain.HardMaxKnowledgeCardBudget + 1}).Validate(); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("over-hard-max card budget error = %v, want ErrKnowledgeLimitExceeded", err)
	}
	if err := (domain.KnowledgeLimits{MaxClaimsPerSubject: -1}).Validate(); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("negative claims limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}
}

func TestKnowledgeValidationWithLimitsCannotBypassHardMaxima(t *testing.T) {
	claim := testKnowledgeClaim()
	if err := claim.ValidateWithLimits(domain.KnowledgeLimits{MaxValueRunes: domain.HardMaxKnowledgeValueRunes + 1}); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("over-hard-max value limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}
	oversized := claim
	oversized.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: strings.Repeat("x", 3000)}
	if err := oversized.ValidateWithLimits(domain.KnowledgeLimits{MaxValueRunes: 100}); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("configured value limit error = %v, want ErrKnowledgeLimitExceeded", err)
	}
}

func TestKnowledgeClaimValidatesAgainstConfiguredLimits(t *testing.T) {
	claim := testKnowledgeClaim()
	if err := claim.ValidateWithLimits(domain.KnowledgeLimits{MaxValueRunes: 5}); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("over-limit value error = %v, want ErrKnowledgeLimitExceeded", err)
	}
}

func TestValidateKnowledgeSourceRefEnforcesIdentityRules(t *testing.T) {
	if err := domain.ValidateKnowledgeSourceRef("slack-human:evt-1"); err != nil {
		t.Fatalf("valid mutation source rejected: %v", err)
	}
	if err := domain.ValidateKnowledgeSourceRef(""); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("empty mutation source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := domain.ValidateKnowledgeSourceRef("   "); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("blank mutation source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := domain.ValidateKnowledgeSourceRef("password: hunter2secret"); !errors.Is(err, domain.ErrKnowledgeInvalidSource) {
		t.Fatalf("credential mutation source error = %v, want ErrKnowledgeInvalidSource", err)
	}
	if err := domain.ValidateKnowledgeSourceRef(strings.Repeat("x", domain.DefaultMaxKnowledgeSourceRefRunes+1)); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("oversized mutation source error = %v, want ErrKnowledgeLimitExceeded", err)
	}
}

func TestKnowledgeReadableScopesDeriveClosedSetFromBinding(t *testing.T) {
	binding := domain.KnowledgeWriteBinding{
		Team: "T00000001", Actor: "U00000001",
		Conversation: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		Project:      "proj-a", WorkstreamID: "ws-1",
	}
	scopes := domain.KnowledgeReadableScopes(binding)
	want := map[string]string{
		string(domain.KnowledgeScopeGlobal):       "",
		string(domain.KnowledgeScopeTeam):         "T00000001",
		string(domain.KnowledgeScopeUser):         "slack:T00000001:user:U00000001",
		string(domain.KnowledgeScopeProject):      "proj-a",
		string(domain.KnowledgeScopeConversation): "slack:T00000001:dm:D00000001",
		string(domain.KnowledgeScopeWorkstream):   "ws-1",
	}
	if len(scopes) != len(want) {
		t.Fatalf("readable scopes = %v, want %d entries", scopes, len(want))
	}
	seen := map[string]string{}
	for _, scope := range scopes {
		seen[string(scope.Kind)] = scope.ID
	}
	for kind, id := range want {
		if seen[kind] != id {
			t.Fatalf("scope %s = %q, want %q", kind, seen[kind], id)
		}
	}
	empty := domain.KnowledgeReadableScopes(domain.KnowledgeWriteBinding{Actor: "U00000001", Conversation: domain.ConversationKey("slack:T00000001:dm:D00000001")})
	if len(empty) != 3 {
		t.Fatalf("minimal binding scopes = %v, want global, user, and conversation", empty)
	}
}

func TestKnowledgeCommandReceiptValidatesIdentity(t *testing.T) {
	valid := domain.KnowledgeCommandReceipt{
		SourceRef: "slack-human:evt-1", Action: domain.KnowledgeActionRemember,
		PayloadDigest: strings.Repeat("a", 64), Target: "claim:api",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	for name, mutate := range map[string]func(*domain.KnowledgeCommandReceipt){
		"empty source":     func(r *domain.KnowledgeCommandReceipt) { r.SourceRef = "" },
		"read-only action": func(r *domain.KnowledgeCommandReceipt) { r.Action = domain.KnowledgeActionInspect },
		"short digest":     func(r *domain.KnowledgeCommandReceipt) { r.PayloadDigest = "abc" },
		"uppercase digest": func(r *domain.KnowledgeCommandReceipt) { r.PayloadDigest = strings.Repeat("A", 64) },
		"empty target":     func(r *domain.KnowledgeCommandReceipt) { r.Target = "" },
		"oversized target": func(r *domain.KnowledgeCommandReceipt) {
			r.Target = strings.Repeat("t", domain.DefaultMaxKnowledgeSourceRefRunes+1)
		},
	} {
		receipt := valid
		mutate(&receipt)
		if err := receipt.Validate(); err == nil {
			t.Errorf("%s: invalid receipt accepted", name)
		}
	}
}

func TestKnowledgeDocumentCarriesStorageRevision(t *testing.T) {
	document := domain.KnowledgeDocument{
		Subject: "runbook", ScopeKind: domain.KnowledgeScopeGlobal,
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "mem_topic_1",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
		Revision: 3,
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("revisioned document rejected: %v", err)
	}
}

func TestKnowledgeOpaqueIDValidation(t *testing.T) {
	validClaim := domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 24))
	if !domain.ValidKnowledgeClaimID(validClaim) {
		t.Fatalf("valid claim id rejected: %q", validClaim)
	}
	validDoc := domain.KnowledgeDocumentID("kdoc_" + strings.Repeat("f", 24))
	if !domain.ValidKnowledgeDocumentID(validDoc) {
		t.Fatalf("valid document id rejected: %q", validDoc)
	}
	for _, id := range []domain.KnowledgeClaimID{
		domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 23)),
		domain.KnowledgeClaimID("kclaim_" + strings.Repeat("g", 24)),
		domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 24) + "x"),
		domain.KnowledgeClaimID("kclaim_`code`\n"),
		domain.KnowledgeClaimID(""),
	} {
		if domain.ValidKnowledgeClaimID(id) {
			t.Errorf("malformed claim id accepted: %q", id)
		}
	}
	if domain.ValidKnowledgeClaimID(domain.KnowledgeClaimID("kdoc_" + strings.Repeat("a", 24))) {
		t.Errorf("document-shaped id accepted as claim id")
	}
}
