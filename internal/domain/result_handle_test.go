package domain

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResultIdentityValidateRequiresCompleteImmutableIdentity(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("result")))
	identity := ResultIdentity{
		ResultID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Producer: ResultProducer{Kind: ResultProducerACPJob, ID: "job-1", Revision: 3},
		Storage:  ResultStorage{Kind: ResultStorageArtifact, Key: "internal-key"},
		SHA256:   digest, Bytes: 6, MediaType: "text/plain; charset=utf-8",
		Scope:     ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		Retention: ResultRetentionWorkstream, State: ResultAvailable, CreatedAt: time.Now().UTC(),
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("validate complete identity: %v", err)
	}

	identity.SHA256 = "forged"
	if err := identity.Validate(); err == nil {
		t.Fatal("forged digest was accepted")
	}
}

func TestResultScopeMatchesRequiresExactTrustedBinding(t *testing.T) {
	stored := ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"}
	if err := stored.Matches(stored); err != nil {
		t.Fatalf("matching scope rejected: %v", err)
	}
	for _, candidate := range []ResultScope{
		{Actor: "U2", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		{Actor: "U1", TeamID: "T2", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D2", Project: "app"},
		{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "other"},
	} {
		if err := stored.Matches(candidate); err == nil {
			t.Fatalf("cross-scope binding accepted: %#v", candidate)
		}
	}
}

func TestResultHandleExposesOnlySafeIdentityAndBoundedRepresentations(t *testing.T) {
	handle := ResultHandle{
		ResultID:          "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SHA256:            fmt.Sprintf("%x", sha256.Sum256([]byte("result"))),
		Bytes:             6,
		MediaType:         "text/plain; charset=utf-8",
		Availability:      []ResultAvailability{ResultAvailabilityRangeRead, ResultAvailabilityPrivateArtifact},
		RepresentationIDs: []string{"1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	if err := handle.Validate(); err != nil {
		t.Fatalf("validate handle: %v", err)
	}
	handle.RepresentationIDs = make([]string, HardMaxResultRepresentationIDs+1)
	for i := range handle.RepresentationIDs {
		handle.RepresentationIDs[i] = fmt.Sprintf("%064x", i+1)
	}
	if err := handle.Validate(); err == nil {
		t.Fatal("unbounded representation IDs were accepted")
	}
}

func TestResultHandleRejectsUnboundedOrNonOpaqueModelVisibleMetadata(t *testing.T) {
	handle, err := testResultIdentity().Handle([]ResultAvailability{ResultAvailabilityRangeRead}, nil)
	if err != nil {
		t.Fatalf("create handle: %v", err)
	}

	handle.MediaType = "text/" + strings.Repeat("x", HardMaxResultMediaTypeBytes)
	if err := handle.Validate(); err == nil {
		t.Fatal("unbounded media type was accepted")
	}

	handle, err = testResultIdentity().Handle([]ResultAvailability{ResultAvailabilityRangeRead}, nil)
	if err != nil {
		t.Fatalf("create second handle: %v", err)
	}
	handle.RepresentationIDs = []string{"https://example.invalid/private-result"}
	if err := handle.Validate(); err == nil {
		t.Fatal("non-opaque representation ID was accepted")
	}
}

// TestResultInlineAvailabilityRequiresExplicitFrameAdmission pins hallazgo 5:
// TRD 02 direct-inline admission belongs to the TRD 04 activation-frame
// layer (see TRD 02 §Representations and Context), not to a provider-token-
// exact ResultIdentity method; HandleWithInlineAdmission was removed as dead
// contract. Every route to a public ResultHandle still rejects
// ResultAvailabilityInline: there is no admission path left in this package.
func TestResultInlineAvailabilityRequiresExplicitFrameAdmission(t *testing.T) {
	identity := testResultIdentity()
	if _, err := identity.Handle([]ResultAvailability{ResultAvailabilityInline}, nil); err != ErrResultInlineAdmission {
		t.Fatalf("generic inline handle error = %v, want %v", err, ErrResultInlineAdmission)
	}
}

func TestResultContractsRejectNonCanonicalDigests(t *testing.T) {
	identity := testResultIdentity()
	identity.SHA256 = strings.ToUpper(identity.SHA256)
	if err := identity.Validate(); err == nil {
		t.Fatal("uppercase identity digest was accepted")
	}

	handle := ResultHandle{
		ResultID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SHA256:   strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256([]byte("result")))), Bytes: 6,
		MediaType: "text/plain", Availability: []ResultAvailability{ResultAvailabilityRangeRead},
	}
	if err := handle.Validate(); err == nil {
		t.Fatal("uppercase handle digest was accepted")
	}

	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("result")))
	representation := ResultRepresentation{
		RepresentationID:         "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ResultID:                 "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Kind:                     ResultRepresentationProducerHandoff,
		State:                    ResultRepresentationAvailable,
		SourceSHA256:             digest,
		SourceBytes:              6,
		AlgorithmOrPromptVersion: "handoff-v1",
		PayloadSHA256:            strings.ToUpper(digest),
		PayloadBytes:             6,
	}
	if err := representation.Validate(); err == nil {
		t.Fatal("uppercase representation digest was accepted")
	}
}

func TestResultIdentityHandleRejectsUnavailableAndLegacyResults(t *testing.T) {
	identity := testResultIdentity()
	identity.State = ResultQuarantined
	if _, err := identity.Handle([]ResultAvailability{ResultAvailabilityRangeRead}, nil); err != ErrResultQuarantined {
		t.Fatalf("quarantined result handle error = %v, want %v", err, ErrResultQuarantined)
	}
	identity = testResultIdentity()
	identity.Producer.Kind = ResultProducerLegacyProjection
	if err := identity.VerifyWorkstreamEligible(); err != ErrResultLegacyNotLinkable {
		t.Fatalf("legacy workstream eligibility error = %v, want %v", err, ErrResultLegacyNotLinkable)
	}
}

func testResultIdentity() ResultIdentity {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("result")))
	return ResultIdentity{
		ResultID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Producer: ResultProducer{Kind: ResultProducerACPJob, ID: "job-1", Revision: 3},
		Storage:  ResultStorage{Kind: ResultStorageArtifact, Key: "internal-key"},
		SHA256:   digest, Bytes: 6, MediaType: "text/plain; charset=utf-8",
		Scope:     ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		Retention: ResultRetentionWorkstream, State: ResultAvailable, CreatedAt: time.Now().UTC(),
	}
}
