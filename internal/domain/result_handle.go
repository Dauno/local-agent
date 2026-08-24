package domain

import (
	"errors"
	"fmt"
	"mime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	HardMaxResultRepresentationIDs = 8
	HardMaxResultMediaTypeBytes    = 256
	HardMaxDirectInlineResultBytes = 64 * 1024
)

// Default and hard-maximum retention ages in days for the four TRD 02
// retention classes (orchestration.result_handles.retention.*).
const (
	DefaultResultRetentionContextDays      = 7
	DefaultResultRetentionConversationDays = 30
	DefaultResultRetentionWorkstreamDays   = 180
	DefaultResultRetentionExportedDays     = 30
	HardMaxResultRetentionDays             = 3650
)

// ResultRetentionAges carries the validated per-class retention ages used by
// the offline retention observability check. ResultRetentionClassCounts
// reports bounded counts only: no result ID, content, path, or digest.
type ResultRetentionAges struct {
	Context      time.Duration
	Conversation time.Duration
	Workstream   time.Duration
}

type ResultRetentionClassCounts struct {
	Class                  ResultRetentionClass
	Candidates             int
	ReferenceProtected     int
	MaterializationPending int
}

// ResultRetentionHealth is the bounded result of one offline retention scan.
// ExportedNotImplemented records that the "exported" class is never scanned:
// its age anchor is a verified publication event that does not exist yet
// (see TRD 02 §Retention).
type ResultRetentionHealth struct {
	Classes                []ResultRetentionClassCounts
	ExportedNotImplemented bool
}

var (
	ErrResultInvalid             = errors.New("result identity is invalid")
	ErrResultUnavailable         = errors.New("result is unavailable")
	ErrResultQuarantined         = errors.New("result is quarantined")
	ErrResultScopeMismatch       = errors.New("result scope does not match")
	ErrResultLegacyNotLinkable   = errors.New("legacy result cannot be linked to a workstream")
	ErrResultRepresentationLimit = errors.New("result representation limit exceeded")
	ErrResultInlineAdmission     = errors.New("result inline admission is required")
)

type ResultProducerKind string

const (
	ResultProducerACPJob           ResultProducerKind = "acp_job"
	ResultProducerToolOperation    ResultProducerKind = "tool_operation"
	ResultProducerLegacyProjection ResultProducerKind = "legacy_projection"
)

type ResultStorageKind string

const (
	ResultStorageArtifact    ResultStorageKind = "artifact"
	ResultStorageRecoverable ResultStorageKind = "recoverable"
)

type ResultRetentionClass string

const (
	ResultRetentionContext      ResultRetentionClass = "context"
	ResultRetentionConversation ResultRetentionClass = "conversation"
	ResultRetentionWorkstream   ResultRetentionClass = "workstream"
	ResultRetentionExported     ResultRetentionClass = "exported"
)

type ResultState string

const (
	ResultAvailable   ResultState = "available"
	ResultQuarantined ResultState = "quarantined"
)

type ResultAvailability string

const (
	ResultAvailabilityInline          ResultAvailability = "inline"
	ResultAvailabilityRangeRead       ResultAvailability = "range_read"
	ResultAvailabilityPrivateArtifact ResultAvailability = "private_artifact"
)

type ResultRepresentationKind string

const (
	ResultRepresentationProducerHandoff ResultRepresentationKind = "producer_handoff_v1"
	ResultRepresentationHostReduction   ResultRepresentationKind = "host_reduction"
)

type ResultRepresentationState string

const (
	ResultRepresentationAvailable   ResultRepresentationState = "available"
	ResultRepresentationQuarantined ResultRepresentationState = "quarantined"
)

// ResultScope is host-derived consumer authority. Every component must match
// exactly before a payload or handle can be resolved.
type ResultScope struct {
	Actor           string
	TeamID          string
	ConversationKey string
	Project         string
}

func (s ResultScope) Validate() error {
	if strings.TrimSpace(s.Actor) == "" || strings.TrimSpace(s.TeamID) == "" || strings.TrimSpace(s.ConversationKey) == "" || strings.TrimSpace(s.Project) == "" {
		return ErrResultInvalid
	}
	return nil
}

func (s ResultScope) Matches(required ResultScope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := required.Validate(); err != nil {
		return err
	}
	if s != required {
		return ErrResultScopeMismatch
	}
	return nil
}

// KnowledgeDocumentAuthorizesResult reports whether a curated knowledge
// document's scope authorizes access to a result carrying the given
// ResultScope. Only scope kinds with an unambiguous identity mapped onto
// ResultScope are authorized: global and workstream scope carry no
// comparable identity on a result, so they never authorize access even
// though the scope kind itself is otherwise valid for knowledge.
func KnowledgeDocumentAuthorizesResult(kind KnowledgeScopeKind, scopeID string, result ResultScope) bool {
	if result.Validate() != nil || strings.TrimSpace(scopeID) == "" {
		return false
	}
	switch kind {
	case KnowledgeScopeTeam:
		return scopeID == result.TeamID
	case KnowledgeScopeUser:
		owner := SlackOwnerKey(ConversationKey(result.ConversationKey), result.Actor)
		return owner != "" && scopeID == owner
	case KnowledgeScopeProject:
		return scopeID == result.Project
	case KnowledgeScopeConversation:
		return scopeID == result.ConversationKey
	default:
		return false
	}
}

// ResultProducer is host-created lineage. Different revisions intentionally
// identify different result records even when their payload digest is equal.
type ResultProducer struct {
	Kind     ResultProducerKind
	ID       string
	Revision int
}

func (p ResultProducer) Validate() error {
	if strings.TrimSpace(p.ID) == "" || p.Revision < 0 {
		return ErrResultInvalid
	}
	switch p.Kind {
	case ResultProducerACPJob, ResultProducerToolOperation, ResultProducerLegacyProjection:
		return nil
	default:
		return ErrResultInvalid
	}
}

type ResultStorage struct {
	Kind ResultStorageKind
	Key  string
}

func (s ResultStorage) Validate() error {
	if strings.TrimSpace(s.Key) == "" {
		return ErrResultInvalid
	}
	switch s.Kind {
	case ResultStorageArtifact, ResultStorageRecoverable:
		return nil
	default:
		return ErrResultInvalid
	}
}

// ResultIdentity is immutable host truth. Storage is intentionally internal
// and never copied into ResultHandle or supplied by a model consumer.
type ResultIdentity struct {
	ResultID  string
	Producer  ResultProducer
	Storage   ResultStorage
	SHA256    string
	Bytes     int64
	MediaType string
	Scope     ResultScope
	Retention ResultRetentionClass
	CreatedAt time.Time
	State     ResultState
}

func (i ResultIdentity) Validate() error {
	if !validResultID(i.ResultID) || !validResultMediaType(i.MediaType) || i.Bytes <= 0 || i.CreatedAt.IsZero() {
		return ErrResultInvalid
	}
	if !validCanonicalResultIdentity(i.SHA256, i.Bytes) {
		return ErrResultInvalid
	}
	if err := i.Producer.Validate(); err != nil {
		return err
	}
	if err := i.Storage.Validate(); err != nil {
		return err
	}
	if err := i.Scope.Validate(); err != nil {
		return err
	}
	switch i.Retention {
	case ResultRetentionContext, ResultRetentionConversation, ResultRetentionWorkstream, ResultRetentionExported:
	default:
		return ErrResultInvalid
	}
	switch i.State {
	case ResultAvailable, ResultQuarantined:
		return nil
	default:
		return ErrResultInvalid
	}
}

func (i ResultIdentity) Handle(availability []ResultAvailability, representationIDs []string) (ResultHandle, error) {
	return i.handle(availability, representationIDs, false)
}

func (i ResultIdentity) handle(availability []ResultAvailability, representationIDs []string, inlineAdmitted bool) (ResultHandle, error) {
	if err := i.Validate(); err != nil {
		return ResultHandle{}, err
	}
	if i.State == ResultQuarantined {
		return ResultHandle{}, ErrResultQuarantined
	}
	handle := ResultHandle{
		ResultID: i.ResultID, SHA256: i.SHA256, Bytes: i.Bytes, MediaType: i.MediaType,
		Availability: slices.Clone(availability), RepresentationIDs: slices.Clone(representationIDs),
		inlineAdmitted: inlineAdmitted,
	}
	if err := handle.Validate(); err != nil {
		return ResultHandle{}, err
	}
	return handle, nil
}

func (i ResultIdentity) VerifyWorkstreamEligible() error {
	if err := i.Validate(); err != nil {
		return err
	}
	if i.State == ResultQuarantined {
		return ErrResultQuarantined
	}
	if i.Producer.Kind == ResultProducerLegacyProjection {
		return ErrResultLegacyNotLinkable
	}
	return nil
}

// ResultHandle is the bounded model-visible projection of ResultIdentity.
// It contains no scope, storage, path, URL, provider error, or credentials.
type ResultHandle struct {
	ResultID          string
	SHA256            string
	Bytes             int64
	MediaType         string
	Availability      []ResultAvailability
	RepresentationIDs []string
	inlineAdmitted    bool
}

func (h ResultHandle) Validate() error {
	if !validResultID(h.ResultID) || !validResultMediaType(h.MediaType) {
		return ErrResultInvalid
	}
	if !validCanonicalResultIdentity(h.SHA256, h.Bytes) {
		return ErrResultInvalid
	}
	if len(h.Availability) == 0 || len(h.RepresentationIDs) > HardMaxResultRepresentationIDs {
		if len(h.RepresentationIDs) > HardMaxResultRepresentationIDs {
			return ErrResultRepresentationLimit
		}
		return ErrResultInvalid
	}
	seenAvailability := make(map[ResultAvailability]struct{}, len(h.Availability))
	for _, availability := range h.Availability {
		switch availability {
		case ResultAvailabilityInline, ResultAvailabilityRangeRead, ResultAvailabilityPrivateArtifact:
		default:
			return ErrResultInvalid
		}
		if _, exists := seenAvailability[availability]; exists {
			return ErrResultInvalid
		}
		if availability == ResultAvailabilityInline && !h.inlineAdmitted {
			return ErrResultInlineAdmission
		}
		seenAvailability[availability] = struct{}{}
	}
	seenRepresentations := make(map[string]struct{}, len(h.RepresentationIDs))
	for _, id := range h.RepresentationIDs {
		if !validResultID(id) {
			return ErrResultInvalid
		}
		if _, exists := seenRepresentations[id]; exists {
			return ErrResultInvalid
		}
		seenRepresentations[id] = struct{}{}
	}
	return nil
}

type ResultRepresentation struct {
	RepresentationID         string
	ResultID                 string
	Kind                     ResultRepresentationKind
	State                    ResultRepresentationState
	SourceSHA256             string
	SourceBytes              int64
	AlgorithmOrPromptVersion string
	PayloadSHA256            string
	PayloadBytes             int64
}

func (r ResultRepresentation) Validate() error {
	if !validResultID(r.RepresentationID) || !validResultID(r.ResultID) || strings.TrimSpace(r.AlgorithmOrPromptVersion) == "" {
		return ErrResultInvalid
	}
	if !validCanonicalResultIdentity(r.SourceSHA256, r.SourceBytes) {
		return ErrResultInvalid
	}
	if !validCanonicalResultIdentity(r.PayloadSHA256, r.PayloadBytes) {
		return ErrResultInvalid
	}
	switch r.Kind {
	case ResultRepresentationProducerHandoff, ResultRepresentationHostReduction:
	default:
		return ErrResultInvalid
	}
	switch r.State {
	case ResultRepresentationAvailable, ResultRepresentationQuarantined:
		return nil
	default:
		return ErrResultInvalid
	}
}

// ValidResultID reports whether value is a well-formed opaque V2 result
// identity (64 lowercase hex characters). It is exported so consumers that
// scan model-facing content for admitted result identities, such as the
// context-epoch recorder, can validate candidates without duplicating the
// format.
func ValidResultID(value string) bool {
	return validResultID(value)
}

func validResultID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func validCanonicalResultIdentity(sha256Hex string, bytes int64) bool {
	digest, validBytes := ValidResultIdentity(sha256Hex, bytes)
	return digest != "" && digest == sha256Hex && validBytes == bytes
}

func validResultMediaType(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > HardMaxResultMediaTypeBytes || !utf8.ValidString(value) {
		return false
	}
	_, _, err := mime.ParseMediaType(value)
	return err == nil
}

func (i ResultIdentity) String() string {
	return fmt.Sprintf("result:%s", i.ResultID)
}
