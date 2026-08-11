package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const HardMaxResultRepresentationIDs = 8

var (
	ErrResultInvalid             = errors.New("result identity is invalid")
	ErrResultUnavailable         = errors.New("result is unavailable")
	ErrResultQuarantined         = errors.New("result is quarantined")
	ErrResultScopeMismatch       = errors.New("result scope does not match")
	ErrResultLegacyNotLinkable   = errors.New("legacy result cannot be linked to a workstream")
	ErrResultRepresentationLimit = errors.New("result representation limit exceeded")
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
	if !validResultID(i.ResultID) || strings.TrimSpace(i.MediaType) == "" || i.Bytes <= 0 || i.CreatedAt.IsZero() {
		return ErrResultInvalid
	}
	if digest, bytes := ValidResultIdentity(i.SHA256, i.Bytes); digest == "" || bytes != i.Bytes {
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
	if err := i.Validate(); err != nil {
		return ResultHandle{}, err
	}
	if i.State == ResultQuarantined {
		return ResultHandle{}, ErrResultQuarantined
	}
	handle := ResultHandle{
		ResultID: i.ResultID, SHA256: strings.ToLower(i.SHA256), Bytes: i.Bytes, MediaType: i.MediaType,
		Availability: slices.Clone(availability), RepresentationIDs: slices.Clone(representationIDs),
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
}

func (h ResultHandle) Validate() error {
	if !validResultID(h.ResultID) || strings.TrimSpace(h.MediaType) == "" {
		return ErrResultInvalid
	}
	if digest, bytes := ValidResultIdentity(h.SHA256, h.Bytes); digest == "" || bytes != h.Bytes {
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
		seenAvailability[availability] = struct{}{}
	}
	seenRepresentations := make(map[string]struct{}, len(h.RepresentationIDs))
	for _, id := range h.RepresentationIDs {
		if strings.TrimSpace(id) == "" {
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
	if strings.TrimSpace(r.RepresentationID) == "" || !validResultID(r.ResultID) || strings.TrimSpace(r.AlgorithmOrPromptVersion) == "" {
		return ErrResultInvalid
	}
	if digest, bytes := ValidResultIdentity(r.SourceSHA256, r.SourceBytes); digest == "" || bytes != r.SourceBytes {
		return ErrResultInvalid
	}
	if digest, bytes := ValidResultIdentity(r.PayloadSHA256, r.PayloadBytes); digest == "" || bytes != r.PayloadBytes {
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

func (i ResultIdentity) String() string {
	return fmt.Sprintf("result:%s", i.ResultID)
}
