package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ProtocolFrontierStatus is the provider-neutral classification of the latest
// durable protocol boundary. It intentionally contains no protocol identity.
type ProtocolFrontierStatus string

const (
	ProtocolReady                 ProtocolFrontierStatus = "ready"
	ProtocolPendingConfirmation   ProtocolFrontierStatus = "pending_confirmation"
	ProtocolRecoverableDurableJob ProtocolFrontierStatus = "recoverable_durable_job"
	ProtocolCompletionUnknown     ProtocolFrontierStatus = "completion_unknown"
	ProtocolCorrupt               ProtocolFrontierStatus = "corrupt_protocol"
)

// ProtocolFrontier is safe to expose to callers and operators. Call IDs,
// function names, arguments, response data, and event content stay private to
// the scanner and later host-evidence reconciliation.
type ProtocolFrontier struct {
	Status          ProtocolFrontierStatus
	OpenCallCount   int
	SessionRevision int64
	RecoveryID      string
}

// ProtocolValidationOptions keeps structural validation, completeness,
// continuation policy, and provider ordering as separate controls.
type ProtocolValidationOptions struct {
	RequireComplete            bool
	AllowConfirmationLifecycle bool
	AllowContinuationResponses bool
	RequireProviderReadyOrder  bool
}

// ProviderProtocolCapabilities describes protocol behavior supplied by a
// provider adapter. The current providers leave continuations disabled.
type ProviderProtocolCapabilities struct {
	AllowContinuationResponses bool
}

var ErrProtocolValidation = errors.New("protocol_validation_failed")

// ProtocolValidationRule identifies the independently testable structural or
// policy rule that rejected a content sequence.
type ProtocolValidationRule string

const (
	ProtocolRuleContentRole            ProtocolValidationRule = "content_role"
	ProtocolRuleContentPart            ProtocolValidationRule = "content_part"
	ProtocolRuleCallIdentity           ProtocolValidationRule = "call_identity"
	ProtocolRuleDuplicateCall          ProtocolValidationRule = "duplicate_call"
	ProtocolRuleResponseIdentity       ProtocolValidationRule = "response_identity"
	ProtocolRuleResponseBeforeCall     ProtocolValidationRule = "response_before_call"
	ProtocolRuleResponseName           ProtocolValidationRule = "response_name"
	ProtocolRuleDuplicateResponse      ProtocolValidationRule = "duplicate_response"
	ProtocolRuleConfirmationLifecycle  ProtocolValidationRule = "confirmation_lifecycle"
	ProtocolRuleContinuationNotAllowed ProtocolValidationRule = "continuation_not_allowed"
	ProtocolRuleProviderReadyOrder     ProtocolValidationRule = "provider_ready_order"
	ProtocolRuleIncompleteCall         ProtocolValidationRule = "incomplete_call"
	ProtocolRuleIncompleteConfirmation ProtocolValidationRule = "incomplete_confirmation"
)

// ProtocolValidationError reports a deterministic protocol failure without
// requiring callers to parse an untyped error string.
type ProtocolValidationError struct {
	Rule         ProtocolValidationRule
	ContentIndex int
	PartIndex    int
	Message      string
}

func (e *ProtocolValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Rule)
	}
	return e.Message
}

func (e *ProtocolValidationError) Unwrap() error { return ErrProtocolValidation }

// ProtocolFrontierError wraps a typed validation failure while preserving the
// corrupt frontier classification returned by ScanProtocolFrontier.
type ProtocolFrontierError struct {
	Frontier ProtocolFrontier
	Cause    error
}

func (e *ProtocolFrontierError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == nil {
		return fmt.Sprintf("protocol frontier: %s", e.Frontier.Status)
	}
	return fmt.Sprintf("protocol frontier: %s: %v", e.Frontier.Status, e.Cause)
}

func (e *ProtocolFrontierError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type protocolCall struct {
	id                      string
	name                    string
	responseCount           int
	confirmationPlaceholder bool
	confirmationWrapperID   string
	isConfirmationWrapper   bool
}

type protocolLedger struct {
	calls map[string]*protocolCall
	order []string
}

func newProtocolLedger() protocolLedger {
	return protocolLedger{calls: make(map[string]*protocolCall)}
}

func (l protocolLedger) openCallCount() int {
	count := 0
	for _, call := range l.calls {
		if protocolCallOpen(call, l) {
			count++
		}
	}
	return count
}

func protocolCallOpen(call *protocolCall, _ protocolLedger) bool {
	if call == nil {
		return false
	}
	if call.isConfirmationWrapper {
		return call.responseCount == 0
	}
	if call.confirmationWrapperID != "" {
		return call.responseCount < 2
	}
	return call.responseCount == 0 || (call.confirmationPlaceholder && call.responseCount == 1)
}

func protocolCallComplete(call *protocolCall) bool {
	if call == nil {
		return false
	}
	if call.isConfirmationWrapper {
		return call.responseCount == 1
	}
	if call.confirmationWrapperID != "" {
		return call.responseCount == 2
	}
	return call.responseCount == 1 && !call.confirmationPlaceholder
}

func protocolValidationError(rule ProtocolValidationRule, contentIndex, partIndex int, message string) error {
	return &ProtocolValidationError{Rule: rule, ContentIndex: contentIndex, PartIndex: partIndex, Message: message}
}

// ValidateContentProtocol validates a provider-neutral content sequence using
// explicit structural, completeness, continuation, and provider-order rules.
// Retention pins are intentionally not an input: they can widen compaction's
// active suffix, but cannot make an incomplete provider request valid.
func ValidateContentProtocol(contents []Content, options ProtocolValidationOptions) error {
	ledger, err := scanContentProtocol(contents, options)
	if err != nil {
		return err
	}
	if !options.RequireComplete {
		return nil
	}
	for _, id := range ledger.order {
		call := ledger.calls[id]
		if protocolCallComplete(call) {
			continue
		}
		if call.confirmationWrapperID != "" {
			return protocolValidationError(
				ProtocolRuleIncompleteConfirmation, -1, -1,
				fmt.Sprintf("function call %q has no terminal response after confirmation", call.id),
			)
		}
		return protocolValidationError(
			ProtocolRuleIncompleteCall, -1, -1,
			fmt.Sprintf("function call %q has no response in completed turn", call.id),
		)
	}
	return nil
}

// ScanProtocolFrontier performs the pure content-only classification. The
// scanner never fabricates recoverable durable-job evidence; that status is
// reserved for a later host reconciliation layer. With no options,
// confirmation lifecycle events are accepted because they are part of the
// provider-neutral protocol contract.
func ScanProtocolFrontier(contents []Content, options ...ProtocolValidationOptions) (ProtocolFrontier, error) {
	validation := ProtocolValidationOptions{AllowConfirmationLifecycle: true}
	if len(options) > 0 {
		validation = options[0]
	}
	// Frontier classification describes an incomplete suffix. Completeness is
	// checked separately by ValidateContentProtocol.
	validation.RequireComplete = false
	ledger, err := scanContentProtocol(contents, validation)
	if err != nil {
		frontier := ProtocolFrontier{Status: ProtocolCorrupt, OpenCallCount: ledger.openCallCount()}
		return frontier, &ProtocolFrontierError{Frontier: frontier, Cause: err}
	}

	frontier := ProtocolFrontier{Status: ProtocolReady, OpenCallCount: ledger.openCallCount()}
	unknownOpen := false
	pendingConfirmation := false
	for _, id := range ledger.order {
		call := ledger.calls[id]
		if !protocolCallOpen(call, ledger) {
			continue
		}
		if call.isConfirmationWrapper {
			pendingConfirmation = true
			continue
		}
		if call.confirmationWrapperID == "" {
			unknownOpen = true
			continue
		}
		wrapper := ledger.calls[call.confirmationWrapperID]
		if wrapper == nil || !protocolCallOpen(wrapper, ledger) {
			unknownOpen = true
		}
	}
	if unknownOpen {
		frontier.Status = ProtocolCompletionUnknown
	} else if pendingConfirmation {
		frontier.Status = ProtocolPendingConfirmation
	}
	return frontier, nil
}

// ClassifyProtocolFrontier is an expressive alias for the pure scanner.
func ClassifyProtocolFrontier(contents []Content, options ...ProtocolValidationOptions) (ProtocolFrontier, error) {
	return ScanProtocolFrontier(contents, options...)
}

// ProtocolDigest returns a deterministic digest of protocol identity and
// ordering. Text, arguments, response data, and structured payloads are
// represented only by their kind, so the digest is safe for diagnostics and
// independent of map iteration order.
func ProtocolDigest(contents []Content) string {
	type digestPart struct {
		Kind         string `json:"kind"`
		ID           string `json:"id,omitempty"`
		Name         string `json:"name,omitempty"`
		OriginalID   string `json:"original_id,omitempty"`
		OriginalName string `json:"original_name,omitempty"`
		Continuation string `json:"continuation,omitempty"`
	}
	type digestContent struct {
		Role  ContentRole  `json:"role"`
		Parts []digestPart `json:"parts"`
	}
	digestContents := make([]digestContent, len(contents))
	for contentIndex, content := range contents {
		digestContents[contentIndex] = digestContent{Role: content.Role, Parts: make([]digestPart, len(content.Parts))}
		for partIndex, part := range content.Parts {
			digestPart := digestPart{}
			switch {
			case part.FunctionCall != nil:
				digestPart.Kind = "function_call"
				digestPart.ID = part.FunctionCall.ID
				digestPart.Name = part.FunctionCall.Name
				if id, name, ok := confirmationOriginalCall(part.FunctionCall); ok {
					digestPart.OriginalID = id
					digestPart.OriginalName = name
				}
			case part.FunctionResponse != nil:
				digestPart.Kind = "function_response"
				digestPart.ID = part.FunctionResponse.ID
				digestPart.Name = part.FunctionResponse.Name
				if part.FunctionResponse.WillContinue == nil {
					digestPart.Continuation = "absent"
				} else if *part.FunctionResponse.WillContinue {
					digestPart.Continuation = "true"
				} else {
					digestPart.Continuation = "false"
				}
			case len(part.StructuredJSON) > 0:
				digestPart.Kind = "structured"
			default:
				digestPart.Kind = "text"
			}
			digestContents[contentIndex].Parts[partIndex] = digestPart
		}
	}
	encoded, _ := json.Marshal(digestContents)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

// ContentProtocolDigest keeps the digest name explicit for callers that deal
// with multiple domain content digests.
func ContentProtocolDigest(contents []Content) string { return ProtocolDigest(contents) }

func scanContentProtocol(contents []Content, options ProtocolValidationOptions) (protocolLedger, error) {
	ledger := newProtocolLedger()
	for contentIndex, content := range contents {
		if content.Role != ContentRoleUser && content.Role != ContentRoleModel {
			return ledger, protocolValidationError(
				ProtocolRuleContentRole, contentIndex, -1,
				fmt.Sprintf("unsupported content role %q", content.Role),
			)
		}
		if options.RequireProviderReadyOrder {
			if err := validateOpenAIContent(content, contentIndex); err != nil {
				return ledger, err
			}
		}
		openAtContentStart := ledger.openCallCount() > 0
		for partIndex, part := range content.Parts {
			if part.FunctionCall != nil && part.FunctionResponse != nil {
				return ledger, protocolValidationError(ProtocolRuleContentPart, contentIndex, partIndex, "content part contains both function call and response")
			}
			if part.FunctionCall != nil && (len(part.StructuredJSON) > 0 || part.Text != "") {
				return ledger, protocolValidationError(ProtocolRuleContentPart, contentIndex, partIndex, "function call part contains unrelated content")
			}
			if part.FunctionResponse != nil && (len(part.StructuredJSON) > 0 || part.Text != "") {
				return ledger, protocolValidationError(ProtocolRuleContentPart, contentIndex, partIndex, "function response part contains unrelated content")
			}

			switch {
			case part.FunctionCall != nil:
				call := part.FunctionCall
				if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
					return ledger, protocolValidationError(ProtocolRuleCallIdentity, contentIndex, partIndex, fmt.Sprintf("invalid function call at content %d", contentIndex))
				}
				if _, exists := ledger.calls[call.ID]; exists {
					return ledger, protocolValidationError(ProtocolRuleDuplicateCall, contentIndex, partIndex, fmt.Sprintf("duplicate function call %q", call.ID))
				}
				if call.Name == ConfirmationFunctionName {
					if !options.AllowConfirmationLifecycle {
						return ledger, protocolValidationError(ProtocolRuleConfirmationLifecycle, contentIndex, partIndex, "confirmation lifecycle is not allowed")
					}
					originalID, originalName, ok := confirmationOriginalCall(call)
					original, exists := ledger.calls[originalID]
					if !ok || !exists || original.name != originalName || original.isConfirmationWrapper || original.responseCount != 1 || !original.confirmationPlaceholder {
						return ledger, protocolValidationError(ProtocolRuleConfirmationLifecycle, contentIndex, partIndex, fmt.Sprintf("invalid confirmation call %q", call.ID))
					}
					if original.confirmationWrapperID != "" {
						return ledger, protocolValidationError(ProtocolRuleConfirmationLifecycle, contentIndex, partIndex, fmt.Sprintf("duplicate confirmation for function call %q", originalID))
					}
					if options.RequireProviderReadyOrder && openAtContentStart {
						// A wrapper is the only model call allowed to extend an
						// already-open original invocation.
						if !protocolCallOpen(original, ledger) {
							return ledger, protocolValidationError(ProtocolRuleProviderReadyOrder, contentIndex, partIndex, "confirmation wrapper follows a closed call")
						}
					}
					wrapper := &protocolCall{id: call.ID, name: call.Name, isConfirmationWrapper: true}
					ledger.calls[call.ID] = wrapper
					ledger.order = append(ledger.order, call.ID)
					original.confirmationWrapperID = call.ID
					continue
				}
				if options.RequireProviderReadyOrder && openAtContentStart {
					return ledger, protocolValidationError(ProtocolRuleProviderReadyOrder, contentIndex, partIndex, "function call follows an open invocation")
				}
				ledger.calls[call.ID] = &protocolCall{id: call.ID, name: call.Name}
				ledger.order = append(ledger.order, call.ID)

			case part.FunctionResponse != nil:
				response := part.FunctionResponse
				if strings.TrimSpace(response.ID) == "" || strings.TrimSpace(response.Name) == "" {
					return ledger, protocolValidationError(ProtocolRuleResponseIdentity, contentIndex, partIndex, fmt.Sprintf("invalid function response at content %d", contentIndex))
				}
				call, exists := ledger.calls[response.ID]
				if !exists {
					return ledger, protocolValidationError(ProtocolRuleResponseBeforeCall, contentIndex, partIndex, fmt.Sprintf("function response %q has no matching call", response.ID))
				}
				if call.name != response.Name {
					return ledger, protocolValidationError(ProtocolRuleResponseName, contentIndex, partIndex, fmt.Sprintf("function response %q has no matching call", response.ID))
				}
				if response.WillContinue != nil && *response.WillContinue && !options.AllowContinuationResponses {
					return ledger, protocolValidationError(ProtocolRuleContinuationNotAllowed, contentIndex, partIndex, "continuation responses are not allowed")
				}
				if call.isConfirmationWrapper {
					if call.responseCount != 0 {
						return ledger, protocolValidationError(ProtocolRuleDuplicateResponse, contentIndex, partIndex, fmt.Sprintf("function response %q has no matching call", response.ID))
					}
				} else if call.responseCount > 0 {
					if call.confirmationWrapperID == "" || call.responseCount != 1 {
						return ledger, protocolValidationError(ProtocolRuleDuplicateResponse, contentIndex, partIndex, fmt.Sprintf("function response %q has no matching call", response.ID))
					}
					wrapper := ledger.calls[call.confirmationWrapperID]
					if wrapper == nil || wrapper.responseCount != 1 {
						return ledger, protocolValidationError(ProtocolRuleConfirmationLifecycle, contentIndex, partIndex, fmt.Sprintf("function response %q precedes confirmation decision", response.ID))
					}
				}
				if options.RequireProviderReadyOrder && openAtContentStart && !protocolCallOpen(call, ledger) {
					return ledger, protocolValidationError(ProtocolRuleProviderReadyOrder, contentIndex, partIndex, "response does not match an open invocation")
				}
				if call.responseCount == 0 && isConfirmationPlaceholder(response) {
					call.confirmationPlaceholder = true
				}
				call.responseCount++

			case len(part.StructuredJSON) > 0:
				if options.RequireProviderReadyOrder && ledger.openCallCount() > 0 {
					return ledger, protocolValidationError(ProtocolRuleProviderReadyOrder, contentIndex, partIndex, "structured content follows an open invocation")
				}

			case part.Text != "":
				if options.RequireProviderReadyOrder && ledger.openCallCount() > 0 {
					return ledger, protocolValidationError(ProtocolRuleProviderReadyOrder, contentIndex, partIndex, "plain content follows an open invocation")
				}
			}
		}
	}
	return ledger, nil
}

// validateOpenAIContent mirrors the content-level constraints enforced by the
// OpenAI-compatible adapter. It runs only for the provider-ready preflight;
// structural validation remains provider-neutral when that option is off.
func validateOpenAIContent(content Content, contentIndex int) error {
	callCount := 0
	responseCount := 0
	imageCount := 0
	textCount := 0
	var text strings.Builder

	for _, part := range content.Parts {
		switch {
		case part.FunctionCall != nil:
			callCount++
		case part.FunctionResponse != nil:
			responseCount++
		case len(part.StructuredJSON) > 0:
			if !isOpenAIImagePart(part.StructuredJSON) {
				return protocolValidationError(
					ProtocolRuleProviderReadyOrder, contentIndex, -1,
					"content contains a structured part unsupported by the OpenAI-compatible serializer",
				)
			}
			imageCount++
		default:
			textCount++
			text.WriteString(part.Text)
		}
	}

	if callCount > 0 && content.Role != ContentRoleModel {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			fmt.Sprintf("function calls require model role, got %q", content.Role),
		)
	}
	if responseCount > 0 && content.Role != ContentRoleUser {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			fmt.Sprintf("function responses require user role, got %q", content.Role),
		)
	}
	if imageCount > 0 && content.Role != ContentRoleUser {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			fmt.Sprintf("image content requires user role, got %q", content.Role),
		)
	}
	if callCount > 0 && responseCount > 0 {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			"content cannot mix function calls and responses",
		)
	}
	if imageCount > 0 && (callCount > 0 || responseCount > 0) {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			"content cannot mix images with function calls or responses",
		)
	}
	if responseCount > 0 && textCount > 0 {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			"function responses require a user-role content with no text",
		)
	}
	if callCount == 0 && responseCount == 0 && imageCount == 0 && strings.TrimSpace(text.String()) == "" {
		return protocolValidationError(
			ProtocolRuleProviderReadyOrder, contentIndex, -1,
			"content must have non-empty text",
		)
	}
	return nil
}

func isOpenAIImagePart(raw []byte) bool {
	var part struct {
		InlineData *struct {
			MIMEType string `json:"mimeType"`
		} `json:"inlineData"`
	}
	if err := json.Unmarshal(raw, &part); err != nil || part.InlineData == nil {
		return false
	}
	switch strings.ToLower(part.InlineData.MIMEType) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func isConfirmationPlaceholder(response *FunctionResponse) bool {
	if response == nil || response.Response == nil {
		return false
	}
	for _, key := range []string{"error", "status"} {
		value, ok := response.Response[key].(string)
		if !ok {
			continue
		}
		normalized := strings.ReplaceAll(strings.ToLower(value), "_", " ")
		if strings.Contains(normalized, "requires confirmation") {
			return true
		}
	}
	return false
}
