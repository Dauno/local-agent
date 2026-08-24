package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxContextEpochIDRunes       = 128
	MaxContextEpochReasonRunes   = 256
	MaxContextEpochVersionRunes  = 128
	MaxContextEpochIdentityRunes = 256
	MaxContextEpochIdentities    = 128
	MaxContextEpochRange         = 128
)

var ErrContextEpochInvalid = errors.New("context epoch is invalid")

// ContextEpoch records the durable identity and selection facts for one model
// working-set boundary. It intentionally contains no selected frame content.
type ContextEpoch struct {
	EpochID               string
	AppName               string
	UserID                string
	SessionID             string
	EpochNumber           int64
	CoveredThroughOrdinal int64
	WorkstreamRevision    int64
	SummaryIdentity       string
	KnowledgeIdentities   []string
	ResultIdentities      []string
	CompilerVersion       string
	CounterVersion        string
	SourceDigest          string
	FrameTokens           int
	FrameCodePoints       int
	SelectedSourceCount   int
	OmittedSourceCount    int
	Reason                string
	CreatedAt             time.Time
}

func (e ContextEpoch) Validate() error {
	for name, value := range map[string]string{
		"epoch ID":         e.EpochID,
		"app name":         e.AppName,
		"user ID":          e.UserID,
		"session ID":       e.SessionID,
		"compiler version": e.CompilerVersion,
		"counter version":  e.CounterVersion,
		"source digest":    e.SourceDigest,
		"reason":           e.Reason,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrContextEpochInvalid, name)
		}
	}
	if utf8.RuneCountInString(e.EpochID) > MaxContextEpochIDRunes {
		return fmt.Errorf("%w: epoch ID exceeds %d code points", ErrContextEpochInvalid, MaxContextEpochIDRunes)
	}
	if utf8.RuneCountInString(e.Reason) > MaxContextEpochReasonRunes {
		return fmt.Errorf("%w: reason exceeds %d code points", ErrContextEpochInvalid, MaxContextEpochReasonRunes)
	}
	if utf8.RuneCountInString(e.CompilerVersion) > MaxContextEpochVersionRunes || utf8.RuneCountInString(e.CounterVersion) > MaxContextEpochVersionRunes {
		return fmt.Errorf("%w: compiler or counter version exceeds %d code points", ErrContextEpochInvalid, MaxContextEpochVersionRunes)
	}
	if e.EpochNumber <= 0 || e.CoveredThroughOrdinal < -1 || e.WorkstreamRevision < 0 {
		return fmt.Errorf("%w: epoch and covered revisions are out of range", ErrContextEpochInvalid)
	}
	if e.FrameTokens < 0 || e.FrameCodePoints < 0 || e.SelectedSourceCount < 0 || e.OmittedSourceCount < 0 {
		return fmt.Errorf("%w: selection counts cannot be negative", ErrContextEpochInvalid)
	}
	if !isLowerHexDigest(e.SourceDigest) {
		return fmt.Errorf("%w: source digest must be lowercase hexadecimal", ErrContextEpochInvalid)
	}
	if err := validateEpochIdentity(e.SummaryIdentity, "summary identity", false); err != nil {
		return err
	}
	if err := validateEpochIdentities(e.KnowledgeIdentities, "knowledge identity"); err != nil {
		return err
	}
	if err := validateEpochIdentities(e.ResultIdentities, "result identity"); err != nil {
		return err
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created time is required", ErrContextEpochInvalid)
	}
	return nil
}

func isLowerHexDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for index := range value {
		if !strings.ContainsRune("0123456789abcdef", rune(value[index])) {
			return false
		}
	}
	return true
}

func validateEpochIdentity(value, name string, required bool) error {
	if strings.TrimSpace(value) == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrContextEpochInvalid, name)
		}
		return nil
	}
	if utf8.RuneCountInString(value) > MaxContextEpochIdentityRunes {
		return fmt.Errorf("%w: %s exceeds %d code points", ErrContextEpochInvalid, name, MaxContextEpochIdentityRunes)
	}
	return nil
}

func validateEpochIdentities(values []string, name string) error {
	if len(values) > MaxContextEpochIdentities {
		return fmt.Errorf("%w: too many %ss", ErrContextEpochInvalid, name)
	}
	if !slices.IsSorted(values) {
		return fmt.Errorf("%w: %ss must be sorted", ErrContextEpochInvalid, name)
	}
	for index, value := range values {
		if err := validateEpochIdentity(value, fmt.Sprintf("%s %d", name, index), true); err != nil {
			return err
		}
		if index > 0 && values[index-1] == value {
			return fmt.Errorf("%w: duplicate %s", ErrContextEpochInvalid, name)
		}
	}
	return nil
}
