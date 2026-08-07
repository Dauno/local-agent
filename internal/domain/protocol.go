package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// ProtocolFrontierStatus is the provider-neutral classification of the latest
// durable protocol boundary. It intentionally contains no protocol identity.
type ProtocolFrontierStatus string

const (
	ProtocolReady               ProtocolFrontierStatus = "ready"
	ProtocolPendingConfirmation ProtocolFrontierStatus = "pending_confirmation"
	ProtocolCompletionUnknown   ProtocolFrontierStatus = "completion_unknown"
	ProtocolCorrupt             ProtocolFrontierStatus = "corrupt_protocol"
)

// ProtocolFrontier is safe to expose to callers and operators. Call IDs,
// function names, arguments, response data, and event content stay private to
// the scanner and later host-evidence reconciliation.
type ProtocolFrontier struct {
	Status        ProtocolFrontierStatus
	OpenCallCount int
}

// ProtocolValidationOptions keeps structural validation, completeness, and
// continuation policy as separate controls.
type ProtocolValidationOptions struct {
	RequireComplete            bool
	AllowConfirmationLifecycle bool
	AllowContinuationResponses bool
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
	if e.Cause == nil {
		return fmt.Sprintf("protocol frontier: %s", e.Frontier.Status)
	}
	return fmt.Sprintf("protocol frontier: %s: %v", e.Frontier.Status, e.Cause)
}

func (e *ProtocolFrontierError) Unwrap() error {
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
	calls     map[string]*protocolCall
	order     []string
	openCount int
}

func newProtocolLedger() protocolLedger {
	return protocolLedger{calls: make(map[string]*protocolCall)}
}

func protocolCallOpen(call *protocolCall) bool {
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

// ScanProtocolFrontier performs pure content-only classification. With no
// options, confirmation lifecycle events are accepted because they are part
// of the provider-neutral protocol contract.
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
		frontier := ProtocolFrontier{Status: ProtocolCorrupt, OpenCallCount: ledger.openCount}
		return frontier, &ProtocolFrontierError{Frontier: frontier, Cause: err}
	}

	frontier := ProtocolFrontier{Status: ProtocolReady, OpenCallCount: ledger.openCount}
	unknownOpen := false
	pendingConfirmation := false
	for _, id := range ledger.order {
		call := ledger.calls[id]
		if !protocolCallOpen(call) {
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
		if wrapper == nil || !protocolCallOpen(wrapper) {
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

// ProtocolDigest returns a deterministic v1:<lowercase SHA-256> digest of
// protocol identity and ordering. Text, arguments, response data, and
// structured payloads are represented only by their kind, so the digest is
// safe for diagnostics and independent of map iteration order.
func ProtocolDigest(contents []Content) string {
	var canonical strings.Builder
	for _, content := range contents {
		canonical.WriteString(string(content.Role))
		canonical.WriteByte('|')
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				canonical.WriteString("function_call|")
				canonical.WriteString(part.FunctionCall.ID)
				canonical.WriteByte('|')
				canonical.WriteString(part.FunctionCall.Name)
				canonical.WriteByte('|')
				if id, name, ok := confirmationOriginalCall(part.FunctionCall); ok {
					canonical.WriteString(id)
					canonical.WriteByte('|')
					canonical.WriteString(name)
					canonical.WriteByte('|')
				}
			case part.FunctionResponse != nil:
				canonical.WriteString("function_response|")
				canonical.WriteString(part.FunctionResponse.ID)
				canonical.WriteByte('|')
				canonical.WriteString(part.FunctionResponse.Name)
				canonical.WriteByte('|')
				if part.FunctionResponse.WillContinue == nil {
					canonical.WriteString("absent|")
				} else if *part.FunctionResponse.WillContinue {
					canonical.WriteString("true|")
				} else {
					canonical.WriteString("false|")
				}
			case len(part.StructuredJSON) > 0:
				canonical.WriteString("structured|")
			default:
				canonical.WriteString("text|")
			}
		}
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("v1:%x", digest[:])
}

func scanContentProtocol(contents []Content, options ProtocolValidationOptions) (protocolLedger, error) {
	ledger := newProtocolLedger()
	for contentIndex, content := range contents {
		if content.Role != ContentRoleUser && content.Role != ContentRoleModel {
			return ledger, protocolValidationError(
				ProtocolRuleContentRole, contentIndex, -1,
				fmt.Sprintf("unsupported content role %q", content.Role),
			)
		}
		for partIndex, part := range content.Parts {
			if part.FunctionCall != nil && part.FunctionResponse != nil {
				return ledger, protocolValidationError(ProtocolRuleContentPart, contentIndex, partIndex, "content part contains both function call and response")
			}
			if (part.FunctionCall != nil || part.FunctionResponse != nil) && (len(part.StructuredJSON) > 0 || part.Text != "") {
				message := "function response part contains unrelated content"
				if part.FunctionCall != nil {
					message = "function call part contains unrelated content"
				}
				return ledger, protocolValidationError(ProtocolRuleContentPart, contentIndex, partIndex, message)
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
					wrapper := &protocolCall{id: call.ID, name: call.Name, isConfirmationWrapper: true}
					ledger.calls[call.ID] = wrapper
					ledger.order = append(ledger.order, call.ID)
					ledger.openCount++
					original.confirmationWrapperID = call.ID
					continue
				}
				ledger.calls[call.ID] = &protocolCall{id: call.ID, name: call.Name}
				ledger.order = append(ledger.order, call.ID)
				ledger.openCount++

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
				if call.responseCount == 0 && isConfirmationPlaceholder(response) {
					call.confirmationPlaceholder = true
				}
				wasOpen := protocolCallOpen(call)
				call.responseCount++
				nowOpen := protocolCallOpen(call)
				if wasOpen != nowOpen {
					if nowOpen {
						ledger.openCount++
					} else {
						ledger.openCount--
					}
				}

			}
		}
	}
	return ledger, nil
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
