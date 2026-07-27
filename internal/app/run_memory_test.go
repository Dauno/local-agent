package app

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestMemoryCuratorLLMUsesADKFinishReason(t *testing.T) {
	for _, test := range []struct {
		name         string
		finishReason genai.FinishReason
		wantText     string
		wantError    bool
	}{
		{name: "complete", finishReason: genai.FinishReasonStop, wantText: `{"operations":[]}`},
		{name: "truncated", finishReason: genai.FinishReasonMaxTokens, wantText: `{"operations":[`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			llm := &memoryCuratorLLM{llm: staticADKModel{response: &model.LLMResponse{
				Content: genai.NewContentFromText(test.wantText, genai.RoleModel), FinishReason: test.finishReason,
			}}}
			text, err := llm.GenerateText(t.Context(), "prompt")
			if test.wantError {
				if !errors.Is(err, errCuratorResponseIncomplete) || !strings.Contains(err.Error(), "finish_reason=MAX_TOKENS") {
					t.Fatalf("GenerateText() error = %v, want incomplete MAX_TOKENS", err)
				}
				return
			}
			if err != nil || text != test.wantText {
				t.Fatalf("GenerateText() = %q, %v", text, err)
			}
		})
	}
}

type staticADKModel struct{ response *model.LLMResponse }

func (staticADKModel) Name() string { return "test-model" }

func (m staticADKModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(m.response, nil)
	}
}
