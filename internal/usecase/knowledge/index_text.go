package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// KnowledgeIndexText is the redacted canonical lexical index text for one
// identity: the subject, the body, and the source digest over canonical
// JSON carrying kind, stable identity, revision, and the complete redacted
// text. The digest is compared after an authorized row is loaded and is
// never logged or rendered.
type KnowledgeIndexText struct {
	Subject      string
	Body         string
	SourceDigest string
}

// BuildKnowledgeIndexText builds the redacted canonical index text for one
// authoritative item. Claims index subject plus canonical value or
// reference; preferences index key plus canonical scalar value; documents
// index subject plus the complete verified resolved content. documentContent
// must be the resolver-verified exact bytes for document identities and is
// ignored for other kinds. Redaction happens before anything is persisted;
// text that redacts to empty yields an empty digest so the worker removes
// the index rows and completes.
func BuildKnowledgeIndexText(kind domain.KnowledgeRetrievalItemKind, item port.KnowledgeAuthoritativeItem, documentContent string, redact func(string) string) (KnowledgeIndexText, error) {
	redacted := func(value string) string {
		if redact != nil {
			return redact(value)
		}
		return value
	}
	var subject, body, id string
	var revision int
	switch kind {
	case domain.KnowledgeRetrievalClaim:
		if item.Claim == nil {
			return KnowledgeIndexText{}, domain.ErrKnowledgeInvalidValue
		}
		claim := item.Claim
		id = string(claim.ID)
		revision = claim.Revision
		subject = redacted(claim.Subject)
		switch claim.Value.Kind {
		case domain.KnowledgeValueString:
			body = redacted(claim.Value.Text)
		case domain.KnowledgeValueNumber:
			body = redacted(strconv.FormatFloat(claim.Value.Number, 'g', -1, 64))
		case domain.KnowledgeValueBoolean:
			body = redacted(strconv.FormatBool(claim.Value.Boolean))
		case domain.KnowledgeValueReference:
			body = redacted(claim.Value.Reference)
		}
	case domain.KnowledgeRetrievalPreference:
		if item.Preference == nil {
			return KnowledgeIndexText{}, domain.ErrKnowledgeInvalidValue
		}
		preference := item.Preference
		id = "preference:" + strconv.Itoa(preference.ID)
		revision = preference.Revision
		subject = redacted(preference.Key)
		switch preference.Value.Kind {
		case domain.KnowledgeValueString:
			body = redacted(preference.Value.Text)
		case domain.KnowledgeValueNumber:
			body = redacted(strconv.FormatFloat(preference.Value.Number, 'g', -1, 64))
		case domain.KnowledgeValueBoolean:
			body = redacted(strconv.FormatBool(preference.Value.Boolean))
		}
	case domain.KnowledgeRetrievalDocument:
		if item.Document == nil {
			return KnowledgeIndexText{}, domain.ErrKnowledgeInvalidValue
		}
		document := item.Document
		id = string(document.ID)
		revision = document.Revision
		subject = redacted(document.Subject)
		body = redacted(documentContent)
	default:
		return KnowledgeIndexText{}, domain.ErrKnowledgeInvalidValue
	}
	digest := ""
	if subject != "" || body != "" {
		digest = KnowledgeIndexSourceDigest(string(kind), id, revision, subject, body)
	}
	return KnowledgeIndexText{Subject: subject, Body: body, SourceDigest: digest}, nil
}

// KnowledgeIndexSourceDigest computes the canonical source digest for one
// lexical index identity: SHA-256 over canonical JSON with kind, stable
// identity, revision, subject, and body in field order. A redactor change
// alters the redacted text digest without persisting a secret-derived
// policy fingerprint.
func KnowledgeIndexSourceDigest(kind, id string, revision int, subject, body string) string {
	payload := struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		Subject  string `json:"subject"`
		Body     string `json:"body"`
	}{Kind: kind, ID: id, Revision: revision, Subject: subject, Body: body}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
