package domain

import (
	"errors"
	"strings"
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

func protocolContinuationResponse(id, name string, continuation bool) Content {
	return Content{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: id, Name: name, WillContinue: &continuation}}}}
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
		name            string
		contents        []Content
		options         ProtocolValidationOptions
		rule            ProtocolValidationRule
		contentIndex    int
		partIndex       int
		frontierCorrupt bool
	}{
		{
			name:            "response before call",
			contents:        []Content{protocolResponse("call-1", "lookup", nil)},
			options:         protocolOptions(false),
			rule:            ProtocolRuleResponseBeforeCall,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "duplicate call ID",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolCallContent(ContentRoleModel, "call-1", "write"),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleDuplicateCall,
			contentIndex:    1,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "mismatched response name",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolResponse("call-1", "write", nil),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleResponseName,
			contentIndex:    1,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:            "unsupported content role",
			contents:        []Content{{Role: ContentRole("tool"), Parts: []ContentPart{{Text: "invalid"}}}},
			options:         protocolOptions(false),
			rule:            ProtocolRuleContentRole,
			contentIndex:    0,
			partIndex:       -1,
			frontierCorrupt: true,
		},
		{
			name: "part contains call and response",
			contents: []Content{{
				Role: ContentRoleModel,
				Parts: []ContentPart{{
					FunctionCall:     &FunctionCall{ID: "call-1", Name: "lookup"},
					FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup"},
				}},
			}},
			options:         protocolOptions(false),
			rule:            ProtocolRuleContentPart,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "function call contains text",
			contents: []Content{{
				Role:  ContentRoleModel,
				Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "lookup"}, Text: "ambiguous"}},
			}},
			options:         protocolOptions(false),
			rule:            ProtocolRuleContentPart,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "function response contains text",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "lookup"}, Text: "ambiguous"}}},
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleContentPart,
			contentIndex:    1,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:            "function call has empty ID",
			contents:        []Content{protocolCallContent(ContentRoleModel, "", "lookup")},
			options:         protocolOptions(false),
			rule:            ProtocolRuleCallIdentity,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:            "function call has empty name",
			contents:        []Content{protocolCallContent(ContentRoleModel, "call-1", "")},
			options:         protocolOptions(false),
			rule:            ProtocolRuleCallIdentity,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:            "function response has empty ID",
			contents:        []Content{protocolResponse("", "lookup", nil)},
			options:         protocolOptions(false),
			rule:            ProtocolRuleResponseIdentity,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:            "function response has empty name",
			contents:        []Content{protocolResponse("call-1", "", nil)},
			options:         protocolOptions(false),
			rule:            ProtocolRuleResponseIdentity,
			contentIndex:    0,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "duplicate response",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolResponse("call-1", "lookup", nil),
				protocolResponse("call-1", "lookup", nil),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleDuplicateResponse,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "continuation is disabled",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "lookup"),
				protocolContinuationResponse("call-1", "lookup", true),
			},
			options:         protocolOptions(true),
			rule:            ProtocolRuleContinuationNotAllowed,
			contentIndex:    1,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "confirmation lifecycle is disabled",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "call-1", "write"),
			},
			options:         ProtocolValidationOptions{},
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "confirmation original call is missing",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "", "write"),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "confirmation original call has empty name",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "call-1", ""),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "confirmation original call has invalid encoding",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{
					ID: "wrapper-1", Name: ConfirmationFunctionName, Args: map[string]any{"originalFunctionCall": "call-1"},
				}}}},
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "confirmation original call is unknown",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "missing-call", "write"),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    2,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "duplicate confirmation",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "call-1", "write"),
				protocolConfirmationCall("wrapper-2", "call-1", "write"),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    3,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name: "terminal response precedes confirmation decision",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"result": "done"}),
			},
			options:         protocolOptions(false),
			rule:            ProtocolRuleConfirmationLifecycle,
			contentIndex:    3,
			partIndex:       0,
			frontierCorrupt: true,
		},
		{
			name:         "incomplete call has no terminal response",
			contents:     []Content{protocolCallContent(ContentRoleModel, "call-1", "lookup")},
			options:      protocolOptions(true),
			rule:         ProtocolRuleIncompleteCall,
			contentIndex: -1,
			partIndex:    -1,
		},
		{
			name: "incomplete confirmation has no terminal response",
			contents: []Content{
				protocolCallContent(ContentRoleModel, "call-1", "write"),
				protocolResponse("call-1", "write", map[string]any{"error": "requires confirmation"}),
				protocolConfirmationCall("wrapper-1", "call-1", "write"),
				protocolResponse("wrapper-1", ConfirmationFunctionName, map[string]any{"confirmed": true}),
			},
			options:      protocolOptions(true),
			rule:         ProtocolRuleIncompleteConfirmation,
			contentIndex: -1,
			partIndex:    -1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateContentProtocol(test.contents, test.options)
			if err == nil {
				t.Fatal("malformed protocol unexpectedly passed validation")
			}
			if !errors.Is(err, ErrProtocolValidation) {
				t.Fatalf("error = %v, want errors.Is(err, ErrProtocolValidation)", err)
			}
			var validationErr *ProtocolValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ProtocolValidationError", err)
			}
			if validationErr.Rule != test.rule {
				t.Fatalf("rule = %q, want %q", validationErr.Rule, test.rule)
			}
			if validationErr.ContentIndex != test.contentIndex || validationErr.PartIndex != test.partIndex {
				t.Fatalf("error indexes = (%d, %d), want (%d, %d)", validationErr.ContentIndex, validationErr.PartIndex, test.contentIndex, test.partIndex)
			}
			if test.frontierCorrupt {
				frontier, frontierErr := ScanProtocolFrontier(test.contents, test.options)
				var classificationErr *ProtocolFrontierError
				if frontier.Status != ProtocolCorrupt || !errors.As(frontierErr, &classificationErr) {
					t.Fatalf("corrupt scan = %#v, %v", frontier, frontierErr)
				}
				if !errors.Is(frontierErr, ErrProtocolValidation) {
					t.Fatalf("frontier error = %v, want errors.Is(err, ErrProtocolValidation)", frontierErr)
				}
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
}

func TestProtocolDigestV1IsStableAndSensitiveToIdentity(t *testing.T) {
	continuation := false
	base := []Content{
		protocolText(ContentRoleUser, "request"),
		protocolCallContent(ContentRoleModel, "call-1", "lookup"),
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{
			ID: "call-1", Name: "lookup", WillContinue: &continuation,
		}}}},
		protocolConfirmationCall("wrapper-1", "call-1", "lookup"),
	}

	const wantGolden = "v1:f4d1a5160c145da5d2c419b73800dc2766099952b306537a2906b5a1df87a4ad"
	got := ProtocolDigest(base)
	if got != wantGolden {
		t.Fatalf("protocol digest = %q, want golden %q", got, wantGolden)
	}
	if !strings.HasPrefix(got, "v1:") {
		t.Fatalf("protocol digest = %q, want explicit v1 format", got)
	}

	clone := func() []Content {
		result := make([]Content, len(base))
		for index, content := range base {
			result[index] = content.Clone()
		}
		return result
	}
	variants := []struct {
		name   string
		mutate func([]Content)
	}{
		{name: "role", mutate: func(contents []Content) { contents[0].Role = ContentRoleModel }},
		{name: "order", mutate: func(contents []Content) { contents[1], contents[2] = contents[2], contents[1] }},
		{name: "IDs", mutate: func(contents []Content) {
			contents[1].Parts[0].FunctionCall.ID = "call-2"
			contents[2].Parts[0].FunctionResponse.ID = "call-2"
		}},
		{name: "names", mutate: func(contents []Content) {
			contents[1].Parts[0].FunctionCall.Name = "search"
			contents[2].Parts[0].FunctionResponse.Name = "search"
		}},
		{name: "WillContinue", mutate: func(contents []Content) {
			value := true
			contents[2].Parts[0].FunctionResponse.WillContinue = &value
		}},
		{name: "original confirmation identity", mutate: func(contents []Content) {
			contents[3].Parts[0].FunctionCall.Args["originalFunctionCall"] = map[string]any{"id": "call-2", "name": "search"}
		}},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			contents := clone()
			variant.mutate(contents)
			if digest := ProtocolDigest(contents); digest == got {
				t.Fatalf("protocol digest did not change for %s: %q", variant.name, digest)
			}
		})
	}
}
