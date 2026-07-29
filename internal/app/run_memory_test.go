package app

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/secure"
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

func TestMemoryCuratorLLMDoesNotLogModelResponse(t *testing.T) {
	const responseText = "unlogged model response"
	var output bytes.Buffer
	curator := &memoryCuratorLLM{
		llm:    staticADKModel{response: &model.LLMResponse{Content: genai.NewContentFromText(responseText, genai.RoleModel), FinishReason: genai.FinishReasonStop}},
		logger: logging.New(&output, "debug", secure.NewRedactor()),
	}
	if _, err := curator.GenerateText(t.Context(), "prompt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), responseText) {
		t.Fatalf("model response leaked into curator log: %s", output.String())
	}
}

type staticADKModel struct{ response *model.LLMResponse }

func (staticADKModel) Name() string { return "test-model" }

func (m staticADKModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(m.response, nil)
	}
}
