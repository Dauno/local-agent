package memoryprojector

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// knowledgeDirName is the fixed bundle path for the knowledge projection.
// Filenames are host-owned constants; no subject, preference key, project,
// actor, or content value ever derives a path.
const knowledgeDirName = "knowledge"

// knowledgeFileNames returns the fixed file set of one knowledge promotion.
func knowledgeFileNames() []string {
	return []string{"index.md", "claims.md", "preferences.md", "documents.md", "evidence.md"}
}

// renderKnowledge validates every persisted knowledge row and renders the
// fixed knowledge file set under dir/knowledge. Any invalid row aborts the
// whole projection before promotion, so the previous bundle stays intact.
// When the snapshot carries no knowledge rows nothing is rendered and
// legacy output stays byte-identical to the pre-knowledge renderer.
func renderKnowledge(dir string, snapshot port.KnowledgeSnapshot, now time.Time) error {
	if !snapshot.Present() {
		return nil
	}
	if err := validateKnowledgeSnapshot(snapshot); err != nil {
		return err
	}
	knowledgeDir := filepath.Join(dir, knowledgeDirName)
	if err := makeSafeDir(knowledgeDir); err != nil {
		return fmt.Errorf("create knowledge directory: %w", err)
	}
	if err := writeKnowledgeClaims(knowledgeDir, snapshot.Claims, now); err != nil {
		return err
	}
	if err := writeKnowledgePreferences(knowledgeDir, snapshot.Preferences); err != nil {
		return err
	}
	if err := writeKnowledgeDocuments(knowledgeDir, snapshot.Documents); err != nil {
		return err
	}
	if err := writeKnowledgeEvidence(knowledgeDir, snapshot.Evidence); err != nil {
		return err
	}
	return writeKnowledgeIndex(knowledgeDir)
}

// validateKnowledgeSnapshot validates persisted rows under hard storage
// maxima so rows admitted under any configured limit remain projectable.
// Host-generated opaque identifiers are validated too: supersession and
// evidence reference claim IDs, so a malformed persisted ID must fail the
// projection closed instead of rendering structure or invalid UTF-8.
// Rendering never writes SQLite back: expiry stays computed.
func validateKnowledgeSnapshot(snapshot port.KnowledgeSnapshot) error {
	limits := domain.HardKnowledgeLimits()
	for _, claim := range snapshot.Claims {
		if !utf8.ValidString(claim.Subject) || !utf8.ValidString(claim.Value.Text) ||
			!utf8.ValidString(claim.Value.Reference) || !utf8.ValidString(claim.ScopeID) ||
			!utf8.ValidString(claim.SourceRef) || !utf8.ValidString(string(claim.ID)) ||
			!utf8.ValidString(string(claim.SupersedesID)) {
			return fmt.Errorf("knowledge claim %q contains invalid UTF-8", claim.ID)
		}
		if !domain.ValidKnowledgeClaimID(claim.ID) {
			return fmt.Errorf("knowledge claim id %q is malformed", claim.ID)
		}
		if claim.SupersedesID != "" && !domain.ValidKnowledgeClaimID(claim.SupersedesID) {
			return fmt.Errorf("knowledge claim %q supersedes malformed id %q", claim.ID, claim.SupersedesID)
		}
		if err := claim.ValidateWithLimits(limits); err != nil {
			return fmt.Errorf("knowledge claim %q is invalid: %v", claim.ID, err)
		}
	}
	for _, preference := range snapshot.Preferences {
		if !utf8.ValidString(preference.OwnerKey) || !utf8.ValidString(preference.Key) ||
			!utf8.ValidString(preference.Value.Text) || !utf8.ValidString(preference.SourceRef) {
			return fmt.Errorf("knowledge preference %d contains invalid UTF-8", preference.ID)
		}
		if err := preference.ValidateWithLimits(limits); err != nil {
			return fmt.Errorf("knowledge preference %d is invalid: %v", preference.ID, err)
		}
	}
	for _, document := range snapshot.Documents {
		if !utf8.ValidString(document.Subject) || !utf8.ValidString(document.ScopeID) ||
			!utf8.ValidString(document.ContentDigest) || !utf8.ValidString(document.ContentHandle) ||
			!utf8.ValidString(document.SourceID) || !utf8.ValidString(string(document.ID)) {
			return fmt.Errorf("knowledge document %q contains invalid UTF-8", document.ID)
		}
		if !domain.ValidKnowledgeDocumentID(document.ID) {
			return fmt.Errorf("knowledge document id %q is malformed", document.ID)
		}
		if err := document.ValidateWithLimits(limits); err != nil {
			return fmt.Errorf("knowledge document %q is invalid: %v", document.ID, err)
		}
	}
	for _, ref := range snapshot.Evidence {
		if !validKnowledgeEvidenceKind(ref.Kind) {
			return fmt.Errorf("knowledge evidence for claim %q has unknown kind %q", ref.ClaimID, ref.Kind)
		}
		if ref.RevisionNumber <= 0 {
			return fmt.Errorf("knowledge evidence for claim %q has non-positive revision", ref.ClaimID)
		}
		if !utf8.ValidString(string(ref.ClaimID)) || !domain.ValidKnowledgeClaimID(ref.ClaimID) {
			return fmt.Errorf("knowledge evidence references malformed claim id %q", ref.ClaimID)
		}
		if !domain.ValidSlackTimestamp(ref.ExchangeTS) {
			return fmt.Errorf("knowledge evidence for claim %q has unsafe exchange timestamp", ref.ClaimID)
		}
	}
	return nil
}

func validKnowledgeEvidenceKind(kind domain.KnowledgeEvidenceKind) bool {
	return kind == domain.KnowledgeEvidenceSource || kind == domain.KnowledgeEvidenceDecision
}

// escapeKnowledgeText neutralizes control characters and escapes Markdown
// structure so untrusted payloads can never inject frontmatter, headings,
// links, or code spans into the projection.
func escapeKnowledgeText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	return escapeMarkdownText(value)
}

// knowledgeScopeLabel renders the scope kind and, only for project and
// workstream scopes where the identity is useful, the identity. User, team,
// and conversation identities are owner, actor, or conversation keys and
// never appear as projection metadata.
func knowledgeScopeLabel(scopeKind domain.KnowledgeScopeKind, scopeID string) string {
	switch scopeKind {
	case domain.KnowledgeScopeProject, domain.KnowledgeScopeWorkstream:
		if scopeID != "" {
			return string(scopeKind) + ":" + escapeKnowledgeText(scopeID)
		}
	}
	return string(scopeKind)
}

// knowledgeValueLabel renders a typed scalar value with all untrusted text
// escaped so payloads can never inject headings, links, code spans, or
// frontmatter into the projection.
func knowledgeValueLabel(value domain.KnowledgeValue) string {
	switch value.Kind {
	case domain.KnowledgeValueNumber:
		return strconv.FormatFloat(value.Number, 'g', -1, 64)
	case domain.KnowledgeValueBoolean:
		if value.Boolean {
			return "true"
		}
		return "false"
	case domain.KnowledgeValueReference:
		return escapeKnowledgeText(value.Reference)
	default:
		return escapeKnowledgeText(value.Text)
	}
}

func writeKnowledgeClaims(dir string, claims []domain.KnowledgeClaim, now time.Time) error {
	ordered := append([]domain.KnowledgeClaim(nil), claims...)
	slices.SortFunc(ordered, func(a, b domain.KnowledgeClaim) int {
		if a.ScopeKind != b.ScopeKind {
			return cmp.Compare(a.ScopeKind, b.ScopeKind)
		}
		if a.ScopeID != b.ScopeID {
			return cmp.Compare(a.ScopeID, b.ScopeID)
		}
		if a.Subject != b.Subject {
			return cmp.Compare(a.Subject, b.Subject)
		}
		if a.SourceRef != b.SourceRef {
			return cmp.Compare(a.SourceRef, b.SourceRef)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var b strings.Builder
	b.WriteString("# Knowledge Claims\n\n")
	b.WriteString("Scoped claims projected from the durable SQLite authority. Manual edits are replaced, never read back.\n\n")
	for index, claim := range ordered {
		fmt.Fprintf(&b, "## %d. %s\n\n", index+1, escapeKnowledgeText(claim.Subject))
		fmt.Fprintf(&b, "- id: `%s`\n", claim.ID)
		fmt.Fprintf(&b, "- predicate: `%s`\n", claim.Predicate)
		fmt.Fprintf(&b, "- value: %s\n", knowledgeValueLabel(claim.Value))
		fmt.Fprintf(&b, "- scope: %s\n", knowledgeScopeLabel(claim.ScopeKind, claim.ScopeID))
		fmt.Fprintf(&b, "- source: `%s` / %s\n", claim.SourceClass, escapeKnowledgeText(claim.SourceRef))
		fmt.Fprintf(&b, "- status: `%s`\n", claim.EffectiveStatus(now))
		if !claim.ValidFrom.IsZero() {
			fmt.Fprintf(&b, "- valid_from: %s\n", claim.ValidFrom.Format(time.RFC3339))
		}
		if !claim.ValidUntil.IsZero() {
			fmt.Fprintf(&b, "- valid_until: %s\n", claim.ValidUntil.Format(time.RFC3339))
		}
		if claim.SupersedesID != "" {
			fmt.Fprintf(&b, "- supersedes: `%s`\n", claim.SupersedesID)
		}
		fmt.Fprintf(&b, "- revision: %d\n\n", claim.Revision)
	}
	return atomicWrite(filepath.Join(dir, "claims.md"), b.String())
}

func writeKnowledgePreferences(dir string, preferences []domain.KnowledgePreference) error {
	ordered := append([]domain.KnowledgePreference(nil), preferences...)
	slices.SortFunc(ordered, func(a, b domain.KnowledgePreference) int {
		if a.Key != b.Key {
			return cmp.Compare(a.Key, b.Key)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var b strings.Builder
	b.WriteString("# Knowledge Preferences\n\n")
	b.WriteString("Scoped preferences projected from the durable SQLite authority. Owner identity is never projected.\n\n")
	for index, preference := range ordered {
		fmt.Fprintf(&b, "## %d. %s\n\n", index+1, escapeKnowledgeText(preference.Key))
		fmt.Fprintf(&b, "- value: %s\n", knowledgeValueLabel(preference.Value))
		fmt.Fprintf(&b, "- status: `%s`\n", preference.Status)
		fmt.Fprintf(&b, "- revision: %d\n\n", preference.Revision)
	}
	return atomicWrite(filepath.Join(dir, "preferences.md"), b.String())
}

func writeKnowledgeDocuments(dir string, documents []domain.KnowledgeDocument) error {
	ordered := append([]domain.KnowledgeDocument(nil), documents...)
	slices.SortFunc(ordered, func(a, b domain.KnowledgeDocument) int {
		if a.Subject != b.Subject {
			return cmp.Compare(a.Subject, b.Subject)
		}
		if a.ScopeKind != b.ScopeKind {
			return cmp.Compare(a.ScopeKind, b.ScopeKind)
		}
		if a.ScopeID != b.ScopeID {
			return cmp.Compare(a.ScopeID, b.ScopeID)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var b strings.Builder
	b.WriteString("# Knowledge Documents\n\n")
	b.WriteString("Document references projected from the durable SQLite authority. Content handles are never projected.\n\n")
	for index, document := range ordered {
		fmt.Fprintf(&b, "## %d. %s\n\n", index+1, escapeKnowledgeText(document.Subject))
		fmt.Fprintf(&b, "- id: `%s`\n", document.ID)
		fmt.Fprintf(&b, "- scope: %s\n", knowledgeScopeLabel(document.ScopeKind, document.ScopeID))
		fmt.Fprintf(&b, "- digest: `%s`\n", document.ContentDigest)
		fmt.Fprintf(&b, "- provenance: `%s`\n", document.Provenance)
		if document.SourceRev > 0 {
			fmt.Fprintf(&b, "- source_revision: %d\n", document.SourceRev)
		}
		fmt.Fprintf(&b, "- status: `%s`\n", document.Status)
		fmt.Fprintf(&b, "- revision: %d\n\n", document.Revision)
	}
	return atomicWrite(filepath.Join(dir, "documents.md"), b.String())
}

func writeKnowledgeEvidence(dir string, evidence []port.KnowledgeEvidenceRef) error {
	ordered := append([]port.KnowledgeEvidenceRef(nil), evidence...)
	slices.SortFunc(ordered, func(a, b port.KnowledgeEvidenceRef) int {
		if a.ClaimID != b.ClaimID {
			return cmp.Compare(a.ClaimID, b.ClaimID)
		}
		if a.RevisionNumber != b.RevisionNumber {
			return cmp.Compare(a.RevisionNumber, b.RevisionNumber)
		}
		if a.ExchangeTS != b.ExchangeTS {
			return cmp.Compare(a.ExchangeTS, b.ExchangeTS)
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	var b strings.Builder
	b.WriteString("# Knowledge Evidence\n\n")
	b.WriteString("Episodic references to the conversation ledger. No ledger content is copied; conversation and author identity are never projected.\n\n")
	for _, ref := range ordered {
		fmt.Fprintf(&b, "- claim `%s` revision %d: `%s` %s\n", ref.ClaimID, ref.RevisionNumber, ref.Kind, ref.ExchangeTS)
	}
	return atomicWrite(filepath.Join(dir, "evidence.md"), b.String())
}

func writeKnowledgeIndex(dir string) error {
	var b strings.Builder
	b.WriteString("# Knowledge Index\n\n")
	b.WriteString("Scoped knowledge projected from the durable SQLite authority. Every promotion replaces this bundle from committed state; manual edits are never read back.\n\n")
	for _, name := range knowledgeFileNames() {
		if name == "index.md" {
			continue
		}
		fmt.Fprintf(&b, "- [%s](%s)\n", strings.TrimSuffix(name, ".md"), name)
	}
	b.WriteString("\nSee the [root index](../index.md) for the bundle entry point.\n")
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}
