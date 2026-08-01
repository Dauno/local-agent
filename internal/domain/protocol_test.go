package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func protocolText(role ContentRole, text string) Content {
	return Content{Role: role, Parts: []ContentPart{{Text: text}}}
}

func protocolCallContent(role ContentRole, id, name string) Content {
	return Content{Role: role, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: id, Name: name}}}}
}

func protocolCalls(idName ...string) Content {
	parts := make([]ContentPart, 0, len(idName)/2)
	for index := 0; index+1 < len(idName); index += 2 {
		parts = append(parts, ContentPart{FunctionCall: &FunctionCall{ID: idName[index], Name: idName[index+1]}})
	}
	return Content{Role: ContentRoleModel, Parts: parts}
}

func protocolResponse(id, name string, response map[string]any) Content {
	return Content{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: id, Name: name, Response: response}}}}
}

func protocolConfirmationCall(id, originalID, originalName string) Content {
	return Content{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{
		ID:   id,
		Name: ConfirmationFunctionName,
		Args: map[string]any{"originalFunctionCall": map[string]any{"id": originalID, "name": originalName}},
	}}}}
}

func protocolOptions(complete bool) ProtocolValidationOptions {
	return ProtocolValidationOptions{RequireComplete: complete, AllowConfirmationLifecycle: true}
}

func TestScanProtocolFrontierClassifiesCompleteSequences(t *testing.T) {
	tests := []struct {
		name     string
		contents []Content
		open     int
	}{
		{name: "text only", contents: []Content{protocolText(ContentRoleUser, "hello")}},
		{
			name: "one call and response",
			contents: []Content{
				protocolText(ContentRoleUser, "lookup"),
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolResponse("call-1", "lookup", map[string]any{"value": "ok"}),
			},
		},
		{
			name: "parallel calls all respond",
			contents: []Content{
				protocolText(ContentRoleUser, "lookup both"),
				protocolCalls("call-1", "lookup", "call-2", "lookup"),
				{
					Role: ContentRoleUser,
					Parts: []ContentPart{
						{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup"}},
						{FunctionResponse: &FunctionResponse{ID: "call-2", Name: "lookup"}},
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frontier, err := ScanProtocolFrontier(test.contents)
			if err != nil {
				t.Fatalf("scan frontier: %v", err)
			}
			if frontier.Status != ProtocolReady || frontier.OpenCallCount != test.open {
				t.Fatalf("frontier = %#v", frontier)
			}
			if err := ValidateContentProtocol(test.contents, protocolOptions(true)); err != nil {
				t.Fatalf("complete validation: %v", err)
			}
		})
	}
}

func TestScanProtocolFrontierClassifiesMissingParallelResponse(t *testing.T) {
	contents := []Content{
		protocolText(ContentRoleUser, "lookup both"),
		protocolCalls("call-1", "lookup", "call-2", "lookup"),
		protocolResponse("call-1", "lookup", nil),
	}
	frontier, err := ScanProtocolFrontier(contents)
	if err != nil {
		t.Fatalf("scan frontier: %v", err)
	}
	if frontier.Status != ProtocolCompletionUnknown || frontier.OpenCallCount != 1 {
		t.Fatalf("frontier = %#v", frontier)
	}
	if err := ValidateContentProtocol(contents, protocolOptions(true)); err == nil {
		t.Fatal("incomplete parallel batch accepted as complete")
	}
}

func TestProtocolValidationRejectsMalformedSequencesWithTypedErrors(t *testing.T) {
	tests := []struct {
		name     string
		contents []Content
		rule     ProtocolValidationRule
	}{
		{
			name:     "response before call",
			contents: []Content{protocolResponse("call-1", "lookup", nil)},
			rule:     ProtocolRuleResponseBeforeCall,
		},
		{
			name: "duplicate call ID",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolCallContent(ContentRoleModel, "call-1", "write"),
			},
			rule: ProtocolRuleDuplicateCall,
		},
		{
			name: "mismatched response name",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolResponse("call-1", "write", nil),
			},
			rule: ProtocolRuleResponseName,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateContentProtocol(test.contents, protocolOptions(false))
			var validationErr *ProtocolValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ProtocolValidationError", err)
			}
			if validationErr.Rule != test.rule {
				t.Fatalf("rule = %q, want %q", validationErr.Rule, test.rule)
			}
			frontier, frontierErr := ScanProtocolFrontier(test.contents)
			var classificationErr *ProtocolFrontierError
			if frontier.Status != ProtocolCorrupt || !errors.As(frontierErr, &classificationErr) {
				t.Fatalf("corrupt scan = %#v, %v", frontier, frontierErr)
			}
		})
	}
}

func TestConfirmationLifecycleFrontierAtValidBoundaries(t *testing.T) {
	originalCall := protocolCallContent(ContentRoleModel, "call-1", "write")
	placeholder := protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"})
	wrapper := protocolConfirmationCall("wrapper-1", "call-1", "write")
	decision := protocolResponse("wrapper-1", ConfirmationFunctionName, map[string]any{"confirmed": true})
	terminal := protocolResponse("call-1", "write", map[string]any{"result": "done"})

	tests := []struct {
		name     string
		contents []Content
		status   ProtocolFrontierStatus
		open     int
	}{
		{
			name:     "placeholder only remains uncertain",
			contents: []Content{protocolText(ContentRoleUser, "write"), originalCall, placeholder},
			status:   ProtocolCompletionUnknown,
			open:     1,
		},
		{
			name:     "wrapper is pending confirmation",
			contents: []Content{protocolText(ContentRoleUser, "write"), originalCall, placeholder, wrapper},
			status:   ProtocolPendingConfirmation,
			open:     2,
		},
		{
			name:     "consumed decision without terminal is uncertain",
			contents: []Content{protocolText(ContentRoleUser, "write"), originalCall, placeholder, wrapper, decision},
			status:   ProtocolCompletionUnknown,
			open:     1,
		},
		{
			name:     "terminal response closes original call",
			contents: []Content{protocolText(ContentRoleUser, "write"), originalCall, placeholder, wrapper, decision, terminal},
			status:   ProtocolReady,
			open:     0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frontier, err := ScanProtocolFrontier(test.contents)
			if err != nil {
				t.Fatalf("scan frontier: %v", err)
			}
			if frontier.Status != test.status || frontier.OpenCallCount != test.open {
				t.Fatalf("frontier = %#v", frontier)
			}
		})
	}

	if err := ValidateContentProtocol([]Content{originalCall, placeholder, wrapper, decision}, protocolOptions(true)); err == nil {
		t.Fatal("consumed confirmation without terminal response accepted as complete")
	}
	if err := ValidateContentProtocol([]Content{originalCall, placeholder, wrapper, decision, terminal}, protocolOptions(true)); err != nil {
		t.Fatalf("complete confirmation lifecycle rejected: %v", err)
	}
}

func TestProtocolValidationContinuationMarkerIsUnsupportedByDefault(t *testing.T) {
	continuation := true
	contents := []Content{
		protocolCallContent(ContentRoleModel, "call-1", "lookup"),
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{
			ID: "call-1", Name: "lookup", WillContinue: &continuation,
		}}}},
	}
	err := ValidateContentProtocol(contents, protocolOptions(true))
	var validationErr *ProtocolValidationError
	if !errors.As(err, &validationErr) || validationErr.Rule != ProtocolRuleContinuationNotAllowed {
		t.Fatalf("continuation validation error = %v", err)
	}

	frontier, frontierErr := ScanProtocolFrontier(contents, ProtocolValidationOptions{
		AllowConfirmationLifecycle: true,
	})
	if frontier.Status != ProtocolCorrupt || frontierErr == nil {
		t.Fatalf("unsupported continuation frontier = %#v, %v", frontier, frontierErr)
	}

	capabilities := ProviderProtocolCapabilities{AllowContinuationResponses: true}
	if err := ValidateContentProtocol(contents, ProtocolValidationOptions{
		RequireComplete:            true,
		AllowConfirmationLifecycle: true,
		AllowContinuationResponses: capabilities.AllowContinuationResponses,
	}); err != nil {
		t.Fatalf("capability-enabled terminal response rejected: %v", err)
	}
}

func TestProtocolProviderReadyOrderIsSeparateFromStructuralValidation(t *testing.T) {
	contents := []Content{
		protocolText(ContentRoleUser, "start"),
		protocolCallContent(ContentRoleModel, "call-1", "lookup"),
		protocolText(ContentRoleUser, "retry"),
	}
	if err := ValidateContentProtocol(contents, ProtocolValidationOptions{AllowConfirmationLifecycle: true}); err != nil {
		t.Fatalf("structural validation rejected open suffix: %v", err)
	}
	err := ValidateContentProtocol(contents, ProtocolValidationOptions{
		AllowConfirmationLifecycle: true,
		RequireProviderReadyOrder:  true,
	})
	var validationErr *ProtocolValidationError
	if !errors.As(err, &validationErr) || validationErr.Rule != ProtocolRuleProviderReadyOrder {
		t.Fatalf("provider-order error = %v", err)
	}
}

func TestProtocolProviderReadyOrderMatchesOpenAIContentConstraints(t *testing.T) {
	responsePart := ContentPart{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup"}}
	options := ProtocolValidationOptions{
		RequireComplete:            true,
		AllowConfirmationLifecycle: true,
		RequireProviderReadyOrder:  true,
	}
	tests := []struct {
		name     string
		contents []Content
		wantRule ProtocolValidationRule
	}{
		{
			name:     "function call requires model role",
			contents: []Content{protocolCallContent(ContentRoleUser, "call-1", "lookup")},
			wantRule: ProtocolRuleProviderReadyOrder,
		},
		{
			name:     "function response requires user role",
			contents: []Content{{Role: ContentRoleModel, Parts: []ContentPart{responsePart}}},
			wantRule: ProtocolRuleProviderReadyOrder,
		},
		{
			name: "function responses cannot share content with text",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				{Role: ContentRoleUser, Parts: []ContentPart{responsePart, {Text: "extra"}}},
			},
			wantRule: ProtocolRuleProviderReadyOrder,
		},
		{
			name:     "unsupported structured part",
			contents: []Content{{Role: ContentRoleUser, Parts: []ContentPart{{StructuredJSON: json.RawMessage(`{"fileData":{"mimeType":"text/plain"}}`)}}}},
			wantRule: ProtocolRuleProviderReadyOrder,
		},
		{
			name:     "image requires user role",
			contents: []Content{{Role: ContentRoleModel, Parts: []ContentPart{{StructuredJSON: json.RawMessage(`{"inlineData":{"mimeType":"image/png"}}`)}}}},
			wantRule: ProtocolRuleProviderReadyOrder,
		},
		{
			name:     "unsupported content role",
			contents: []Content{{Role: ContentRole("tool"), Parts: []ContentPart{{Text: "invalid"}}}},
			wantRule: ProtocolRuleContentRole,
		},
		{
			name: "duplicate response remains rejected",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolResponse("call-1", "lookup", nil),
				protocolResponse("call-1", "lookup", nil),
			},
			wantRule: ProtocolRuleDuplicateResponse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateContentProtocol(test.contents, options)
			var validationErr *ProtocolValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("validation error = %v, want ProtocolValidationError", err)
			}
			if validationErr.Rule != test.wantRule {
				t.Fatalf("validation rule = %q, want %q", validationErr.Rule, test.wantRule)
			}
		})
	}

	valid := []Content{
		protocolText(ContentRoleUser, "lookup"),
		{Role: ContentRoleModel, Parts: []ContentPart{{Text: "I will look up the value."}, {FunctionCall: &FunctionCall{ID: "call-1", Name: "lookup"}}}},
		protocolResponse("call-1", "lookup", nil),
	}
	if err := ValidateContentProtocol(valid, options); err != nil {
		t.Fatalf("model call with text rejected: %v", err)
	}
	image := []Content{{Role: ContentRoleUser, Parts: []ContentPart{{StructuredJSON: json.RawMessage(`{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}`)}}}}
	if err := ValidateContentProtocol(image, options); err != nil {
		t.Fatalf("supported image rejected: %v", err)
	}
}

func TestRetentionPinsCannotAuthorizeIncompleteProviderRequest(t *testing.T) {
	contents := []Content{
		protocolText(ContentRoleUser, "start"),
		protocolCallContent(ContentRoleModel, "call-1", "lookup"),
	}
	turns, activeStart, err := ClassifyConversationTurns(contents, TurnClassificationOptions{OpenInvocationIDs: map[string]struct{}{"call-1": {}}})
	if err != nil {
		t.Fatalf("classify pinned suffix: %v", err)
	}
	if activeStart != 0 || len(turns) != 1 || !turns[0].HasOpenInvocation {
		t.Fatalf("pinned classification = turns=%#v active_start=%d", turns, activeStart)
	}
	if err := ValidateContentProtocol(contents, ProtocolValidationOptions{
		RequireComplete:            true,
		AllowConfirmationLifecycle: true,
	}); err == nil {
		t.Fatal("retention pin authorized incomplete provider request")
	}
}

func TestProtocolDigestIgnoresMapIterationAndContentData(t *testing.T) {
	leftArgs := map[string]any{
		"z": map[string]any{"second": 2, "first": 1},
		"a": []any{"one", "two"},
	}
	rightArgs := map[string]any{
		"a": []any{"one", "two"},
		"z": map[string]any{"first": 1, "second": 2},
	}
	left := []Content{
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "first text"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "lookup", Args: leftArgs}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup", Response: map[string]any{"result": "left"}}}}},
	}
	right := []Content{
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "different text"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "lookup", Args: rightArgs}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup", Response: map[string]any{"result": "right"}}}}},
	}
	if leftDigest, rightDigest := ProtocolDigest(left), ProtocolDigest(right); leftDigest != rightDigest {
		t.Fatalf("protocol digests differ: %q != %q", leftDigest, rightDigest)
	}
	if ProtocolDigest(left) != ContentProtocolDigest(left) {
		t.Fatal("digest alias changed protocol digest")
	}
}
