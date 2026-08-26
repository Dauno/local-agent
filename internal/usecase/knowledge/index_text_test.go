package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestBuildKnowledgeIndexTextKinds(t *testing.T) {
	redact := func(value string) string {
		if value == "SECRET" {
			return ""
		}
		return strings.ReplaceAll(value, "SECRET", "REDACTED")
	}
	tests := []struct {
		name       string
		kind       domain.KnowledgeRetrievalItemKind
		item       port.KnowledgeAuthoritativeItem
		content    string
		wantSubj   string
		wantBody   string
		wantDigest string
	}{
		{
			name: "claim string value",
			kind: domain.KnowledgeRetrievalClaim,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_1", Claim: &domain.KnowledgeClaim{
				ID: "kclaim_1", Subject: "db host", Predicate: domain.KnowledgePredicateIs,
				Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "SECRET"}, Revision: 3,
			}},
			wantSubj: "db host", wantBody: "",
		},
		{
			name: "claim number value",
			kind: domain.KnowledgeRetrievalClaim,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_2", Claim: &domain.KnowledgeClaim{
				ID: "kclaim_2", Subject: "port", Predicate: domain.KnowledgePredicateUses,
				Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueNumber, Number: 42}, Revision: 1,
			}},
			wantSubj: "port", wantBody: "42",
		},
		{
			name: "claim boolean value",
			kind: domain.KnowledgeRetrievalClaim,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_3", Claim: &domain.KnowledgeClaim{
				ID: "kclaim_3", Subject: "flag", Predicate: domain.KnowledgePredicateIs,
				Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueBoolean, Boolean: true}, Revision: 1,
			}},
			wantSubj: "flag", wantBody: "true",
		},
		{
			name: "claim reference value",
			kind: domain.KnowledgeRetrievalClaim,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_4", Claim: &domain.KnowledgeClaim{
				ID: "kclaim_4", Subject: "mesh", Predicate: domain.KnowledgePredicateOwns,
				Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueReference, Reference: "gateway.internal"}, Revision: 2,
			}},
			wantSubj: "mesh", wantBody: "gateway.internal",
		},
		{
			name: "preference scalar value",
			kind: domain.KnowledgeRetrievalPreference,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalPreference, ID: "preference:7", Preference: &domain.KnowledgePreference{
				ID: 7, Key: "language", Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "spanish"}, Revision: 2,
			}},
			wantSubj: "language", wantBody: "spanish",
		},
		{
			name: "document subject and content",
			kind: domain.KnowledgeRetrievalDocument,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_1", Document: &domain.KnowledgeDocument{
				ID: "kdoc_1", Subject: "onboarding", Revision: 4,
			}},
			content:  "verified content with SECRET inside",
			wantSubj: "onboarding", wantBody: "verified content with REDACTED inside",
		},
		{
			name: "document content redacts to empty",
			kind: domain.KnowledgeRetrievalDocument,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_2", Document: &domain.KnowledgeDocument{
				ID: "kdoc_2", Subject: "secret-only", Revision: 1,
			}},
			content:  "SECRET",
			wantSubj: "secret-only", wantBody: "",
		},
		{
			name: "subject and body redact to empty",
			kind: domain.KnowledgeRetrievalClaim,
			item: port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_5", Claim: &domain.KnowledgeClaim{
				ID: "kclaim_5", Subject: "SECRET", Predicate: domain.KnowledgePredicateIs,
				Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "SECRET"}, Revision: 1,
			}},
			wantSubj: "", wantBody: "", wantDigest: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, err := BuildKnowledgeIndexText(test.kind, test.item, test.content, redact)
			if err != nil {
				t.Fatalf("BuildKnowledgeIndexText() error = %v", err)
			}
			if text.Subject != test.wantSubj || text.Body != test.wantBody {
				t.Fatalf("BuildKnowledgeIndexText() = %q/%q, want %q/%q", text.Subject, text.Body, test.wantSubj, test.wantBody)
			}
			wantDigest := test.wantDigest
			if wantDigest == "" && (text.Subject != "" || text.Body != "") {
				var revision int
				switch test.kind {
				case domain.KnowledgeRetrievalClaim:
					revision = test.item.Claim.Revision
				case domain.KnowledgeRetrievalPreference:
					revision = test.item.Preference.Revision
				case domain.KnowledgeRetrievalDocument:
					revision = test.item.Document.Revision
				}
				wantDigest = KnowledgeIndexSourceDigest(string(test.kind), test.item.ID, revision, text.Subject, text.Body)
			}
			if text.SourceDigest != wantDigest {
				t.Fatalf("SourceDigest = %q, want %q", text.SourceDigest, wantDigest)
			}
			if text.SourceDigest != "" && len(text.SourceDigest) != 64 {
				t.Fatalf("SourceDigest length = %d, want 64", len(text.SourceDigest))
			}
		})
	}
}

func TestBuildKnowledgeIndexTextRejectsBrokenUnions(t *testing.T) {
	if _, err := BuildKnowledgeIndexText(
		domain.KnowledgeRetrievalClaim,
		port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Preference: &domain.KnowledgePreference{ID: 1}},
		"",
		nil,
	); err == nil {
		t.Fatal("BuildKnowledgeIndexText(claim without claim payload) succeeded")
	}
	if _, err := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, port.KnowledgeAuthoritativeItem{Kind: "unknown", ID: "x"}, "", nil); err == nil {
		t.Fatal("BuildKnowledgeIndexText(unknown kind) succeeded")
	}
}

// TestKnowledgeIndexSourceDigestIsDeterministicJSON pins the canonical
// digest: field order, redaction effect, and revision sensitivity.
func TestKnowledgeIndexSourceDigestIsDeterministicJSON(t *testing.T) {
	first := KnowledgeIndexSourceDigest("claim", "kclaim_1", 1, "subject", "body")
	second := KnowledgeIndexSourceDigest("claim", "kclaim_1", 1, "subject", "body")
	if first != second {
		t.Fatalf("digest is not deterministic: %q vs %q", first, second)
	}
	payload := struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		Subject  string `json:"subject"`
		Body     string `json:"body"`
	}{Kind: "claim", ID: "kclaim_1", Revision: 1, Subject: "subject", Body: "body"}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	if want := hex.EncodeToString(sum[:]); first != want {
		t.Fatalf("digest = %q, want canonical JSON digest %q", first, want)
	}
	if KnowledgeIndexSourceDigest("claim", "kclaim_1", 2, "subject", "body") == first {
		t.Fatal("revision change did not change the digest")
	}
	if KnowledgeIndexSourceDigest("claim", "kclaim_1", 1, "REDACTED", "body") == first {
		t.Fatal("redaction change did not change the digest")
	}
}
