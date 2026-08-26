package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// HumanCommandPrefix marks the strict-JSON human knowledge command surface.
// It is parsed and validated like the workstream-human prefix.
const HumanCommandPrefix = "memory-human "

// HumanCommand is one validated memory-human payload. Every field is supplied
// by the human; scope selectors are re-validated against the trusted binding
// before any mutation and grant no access on their own.
type HumanCommand struct {
	Action           domain.KnowledgeAction `json:"action"`
	ClaimID          string                 `json:"claim_id,omitempty"`
	DocumentID       string                 `json:"document_id,omitempty"`
	PreferenceKey    string                 `json:"preference_key,omitempty"`
	Subject          string                 `json:"subject,omitempty"`
	Predicate        string                 `json:"predicate,omitempty"`
	ValueKind        string                 `json:"value_kind,omitempty"`
	ValueText        string                 `json:"value_text,omitempty"`
	ValueNumber      *float64               `json:"value_number,omitempty"`
	ValueBoolean     *bool                  `json:"value_boolean,omitempty"`
	ValueReference   string                 `json:"value_reference,omitempty"`
	ScopeKind        string                 `json:"scope_kind,omitempty"`
	ScopeID          string                 `json:"scope_id,omitempty"`
	ValidFrom        string                 `json:"valid_from,omitempty"`
	ValidUntil       string                 `json:"valid_until,omitempty"`
	ExpectedRevision int                    `json:"expected_revision,omitzero"`
}

// ParseHumanCommand decodes one strict-JSON memory-human command. It reports
// matched=false when the text does not carry the knowledge command prefix so
// callers can fall through to normal handling. Field presence is validated
// against the actual JSON keys, never inferred from decoded values, so
// explicitly empty or non-positive fields are rejected like unknown ones.
func ParseHumanCommand(text string) (HumanCommand, bool, error) {
	if !strings.HasPrefix(text, HumanCommandPrefix) {
		return HumanCommand{}, false, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(text, HumanCommandPrefix))
	var command HumanCommand
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return HumanCommand{}, true, fmt.Errorf("JSON payload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return HumanCommand{}, true, errors.New("JSON payload must contain one object")
		}
		return HumanCommand{}, true, fmt.Errorf("JSON payload has trailing data: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return HumanCommand{}, true, fmt.Errorf("JSON payload keys: %w", err)
	}
	present := make(map[string]bool, len(raw))
	for key := range raw {
		if key != "action" {
			present[key] = true
		}
	}
	if err := domain.ValidateKnowledgeAction(command.Action); err != nil {
		return HumanCommand{}, true, err
	}
	if command.ValueKind != "" {
		if err := command.validateValueUnion(present); err != nil {
			return HumanCommand{}, true, err
		}
		if _, err := command.value(); err != nil {
			return HumanCommand{}, true, err
		}
	} else {
		for _, field := range []string{"value_text", "value_number", "value_boolean", "value_reference"} {
			if present[field] {
				return HumanCommand{}, true, fmt.Errorf("%s requires value_kind", field)
			}
		}
	}
	if err := command.validateForAction(present); err != nil {
		return HumanCommand{}, true, err
	}
	return command, true, nil
}

func (c HumanCommand) validateForAction(present map[string]bool) error {
	switch c.Action {
	case domain.KnowledgeActionRemember:
		if err := c.requireNonEmpty(present, "preference_key"); err != nil {
			return fmt.Errorf("remember: %w", err)
		}
		if present["preference_key"] {
			if err := c.allowOnly(present, "preference_key", "value_kind", "value_text", "value_number", "value_boolean", "expected_revision"); err != nil {
				return fmt.Errorf("preference remember: %w", err)
			}
			if c.ValueKind == "" {
				return errors.New("preference remember requires value_kind")
			}
			if err := c.requirePositiveRevision(present); err != nil {
				return err
			}
			return nil
		}
		if err := c.allowOnly(
			present,
			"subject",
			"predicate",
			"value_kind",
			"value_text",
			"value_number",
			"value_boolean",
			"value_reference",
			"scope_kind",
			"scope_id",
			"valid_from",
			"valid_until",
		); err != nil {
			return fmt.Errorf("claim remember: %w", err)
		}
		if strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.Predicate) == "" {
			return errors.New("claim remember requires subject and predicate")
		}
		if c.ValueKind == "" {
			return errors.New("claim remember requires value_kind")
		}
		if err := c.requirePairedScope(present); err != nil {
			return fmt.Errorf("claim remember: %w", err)
		}
	case domain.KnowledgeActionCorrect:
		if err := c.allowOnly(present, "claim_id", "predicate", "value_kind", "value_text", "value_number", "value_boolean", "value_reference", "valid_from", "valid_until"); err != nil {
			return fmt.Errorf("correct: %w", err)
		}
		if strings.TrimSpace(c.ClaimID) == "" {
			return errors.New("claim_id is required to correct a claim")
		}
		if c.ValueKind == "" {
			return errors.New("correct requires a replacement value")
		}
	case domain.KnowledgeActionForget:
		if err := c.allowOnly(present, "subject", "scope_kind", "scope_id"); err != nil {
			return fmt.Errorf("forget: %w", err)
		}
		if err := c.requirePairedScope(present); err != nil {
			return fmt.Errorf("forget: %w", err)
		}
		if strings.TrimSpace(c.Subject) == "" {
			return errors.New("forget requires a subject")
		}
	case domain.KnowledgeActionArchive:
		if err := c.requireNonEmpty(present, "claim_id", "document_id", "preference_key"); err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		switch {
		case present["claim_id"]:
			if err := c.allowOnly(present, "claim_id", "expected_revision"); err != nil {
				return fmt.Errorf("claim archive: %w", err)
			}
			if c.ExpectedRevision <= 0 {
				return errors.New("claim archive requires a positive expected_revision")
			}
		case present["document_id"]:
			if err := c.allowOnly(present, "document_id", "expected_revision"); err != nil {
				return fmt.Errorf("document archive: %w", err)
			}
			if c.ExpectedRevision <= 0 {
				return errors.New("document archive requires a positive expected_revision")
			}
		case present["preference_key"]:
			if err := c.allowOnly(present, "preference_key", "expected_revision"); err != nil {
				return fmt.Errorf("preference archive: %w", err)
			}
			if c.ExpectedRevision <= 0 {
				return errors.New("preference archive requires a positive expected_revision")
			}
		default:
			return errors.New("archive requires exactly one of claim_id, document_id, or preference_key")
		}
	case domain.KnowledgeActionDispute:
		if err := c.allowOnly(present, "claim_id", "expected_revision"); err != nil {
			return fmt.Errorf("dispute: %w", err)
		}
		if strings.TrimSpace(c.ClaimID) == "" {
			return errors.New("claim_id is required to dispute a claim")
		}
		if c.ExpectedRevision <= 0 {
			return errors.New("dispute requires a positive expected_revision")
		}
	case domain.KnowledgeActionInspect:
		if err := c.allowOnly(present, "claim_id", "document_id", "preference_key", "subject"); err != nil {
			return fmt.Errorf("inspect: %w", err)
		}
		if err := c.requireNonEmpty(present, "claim_id", "document_id", "preference_key", "subject"); err != nil {
			return fmt.Errorf("inspect: %w", err)
		}
		targets := 0
		for _, field := range []string{"claim_id", "document_id", "preference_key"} {
			if present[field] {
				targets++
			}
		}
		if targets > 1 {
			return errors.New("inspect accepts at most one of claim_id, document_id, or preference_key")
		}
		if present["subject"] && targets > 0 {
			return errors.New("inspect subject selector cannot combine with claim_id, document_id, or preference_key")
		}
	}
	return nil
}

// fieldValue resolves the decoded string value for a present key so empty
// explicit selectors can be rejected.
func (c HumanCommand) fieldValue(field string) string {
	switch field {
	case "claim_id":
		return c.ClaimID
	case "document_id":
		return c.DocumentID
	case "preference_key":
		return c.PreferenceKey
	case "subject":
		return c.Subject
	case "predicate":
		return c.Predicate
	case "scope_kind":
		return c.ScopeKind
	case "scope_id":
		return c.ScopeID
	case "value_kind":
		return c.ValueKind
	case "valid_from":
		return c.ValidFrom
	case "valid_until":
		return c.ValidUntil
	}
	return ""
}

// requireNonEmpty rejects present fields whose decoded value is empty or
// whitespace, so empty selectors can never reroute a command.
func (c HumanCommand) requireNonEmpty(present map[string]bool, fields ...string) error {
	for _, field := range fields {
		if present[field] && strings.TrimSpace(c.fieldValue(field)) == "" {
			return fmt.Errorf("%s must not be empty", field)
		}
	}
	return nil
}

// requirePositiveRevision rejects an explicitly present expected_revision
// that is not positive; omission is the only way to express creation.
func (c HumanCommand) requirePositiveRevision(present map[string]bool) error {
	if present["expected_revision"] && c.ExpectedRevision <= 0 {
		return errors.New("expected_revision must be positive when present")
	}
	return nil
}

// requirePairedScope rejects a scope_kind without scope_id or vice versa so a
// half-specified payload can never silently fall back to the default scope,
// and rejects explicitly empty scope selectors.
func (c HumanCommand) requirePairedScope(present map[string]bool) error {
	if present["scope_kind"] != present["scope_id"] {
		return errors.New("scope_kind and scope_id must be present together")
	}
	if present["scope_kind"] && strings.TrimSpace(c.ScopeKind) == "" {
		return errors.New("scope_kind must not be empty")
	}
	if present["scope_id"] && strings.TrimSpace(c.ScopeID) == "" {
		return errors.New("scope_id must not be empty")
	}
	return nil
}

// allowOnly rejects any JSON key present outside the action's closed surface.
func (c HumanCommand) allowOnly(present map[string]bool, allowed ...string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = true
	}
	for field := range present {
		if !allowedSet[field] {
			return fmt.Errorf("field %q is not valid for action %q", field, c.Action)
		}
	}
	return nil
}

// validateValueUnion enforces the strict tagged union: exactly the field
// matching value_kind may be present, and no incompatible field may carry a
// JSON key. Presence follows the raw object keys, so an explicitly empty
// incompatible field is still rejected.
func (c HumanCommand) validateValueUnion(present map[string]bool) error {
	switch domain.KnowledgeValueKind(c.ValueKind) {
	case domain.KnowledgeValueString:
		for _, field := range []string{"value_number", "value_boolean", "value_reference"} {
			if present[field] {
				return fmt.Errorf("string values must not carry %s", field)
			}
		}
	case domain.KnowledgeValueNumber:
		for _, field := range []string{"value_text", "value_boolean", "value_reference"} {
			if present[field] {
				return fmt.Errorf("number values must not carry %s", field)
			}
		}
	case domain.KnowledgeValueBoolean:
		for _, field := range []string{"value_text", "value_number", "value_reference"} {
			if present[field] {
				return fmt.Errorf("boolean values must not carry %s", field)
			}
		}
	case domain.KnowledgeValueReference:
		for _, field := range []string{"value_text", "value_number", "value_boolean"} {
			if present[field] {
				return fmt.Errorf("reference values must not carry %s", field)
			}
		}
	default:
		return fmt.Errorf("%w: %q", domain.ErrKnowledgeInvalidValue, c.ValueKind)
	}
	return nil
}

// value decodes the tagged value union. Exactly the field matching the kind
// carries the value, mirroring the persisted KnowledgeValue contract.
func (c HumanCommand) value() (domain.KnowledgeValue, error) {
	switch domain.KnowledgeValueKind(c.ValueKind) {
	case domain.KnowledgeValueString:
		return domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: c.ValueText}, nil
	case domain.KnowledgeValueNumber:
		if c.ValueNumber == nil {
			return domain.KnowledgeValue{}, errors.New("value_number is required for number values")
		}
		return domain.KnowledgeValue{Kind: domain.KnowledgeValueNumber, Number: *c.ValueNumber}, nil
	case domain.KnowledgeValueBoolean:
		if c.ValueBoolean == nil {
			return domain.KnowledgeValue{}, errors.New("value_boolean is required for boolean values")
		}
		return domain.KnowledgeValue{Kind: domain.KnowledgeValueBoolean, Boolean: *c.ValueBoolean}, nil
	case domain.KnowledgeValueReference:
		return domain.KnowledgeValue{Kind: domain.KnowledgeValueReference, Reference: c.ValueReference}, nil
	default:
		return domain.KnowledgeValue{}, fmt.Errorf("%w: %q", domain.ErrKnowledgeInvalidValue, c.ValueKind)
	}
}

func (c HumanCommand) validity() (from, until time.Time, err error) {
	if c.ValidFrom != "" {
		from, err = time.Parse(time.RFC3339, c.ValidFrom)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("valid_from must be RFC3339: %w", err)
		}
	}
	if c.ValidUntil != "" {
		until, err = time.Parse(time.RFC3339, c.ValidUntil)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("valid_until must be RFC3339: %w", err)
		}
	}
	return from, until, nil
}
