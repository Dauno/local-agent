package openaillm

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

type summaryModelFake struct {
	response string
	request  *model.LLMRequest
}

func (m *summaryModelFake) Name() string { return "summary-test" }

func (m *summaryModelFake) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.request = request
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText(m.response, genai.RoleModel), FinishReason: genai.FinishReasonStop}, nil)
	}
}

func TestSummarizerRedactsOutputBeforeValidation(t *testing.T) {
	const secret = "sk-summary-secret"
	modelFake := &summaryModelFake{response: "The user stated that the token is " + secret + "."}
	summarizer, err := NewSummarizer(modelFake, SummarizerConfig{
		MaxChars: 200, ModelCalls: modelcalllimiter.New(1), Redact: secure.NewRedactor(secret).String,
	})
	if err != nil {
		t.Fatal(err)
	}
	turns := []domain.ConversationTurn{{Ordinal: 1, Closed: true, Contents: []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "goal"}}}}}}
	result, err := summarizer.Summarize(context.Background(), port.ConversationSummaryRequest{ClosedTurns: turns, MaxChars: 200, MaxOutputTokens: 17})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text == "" || contains(result.Text, secret) {
		t.Fatalf("summary leaked output secret: %q", result.Text)
	}
	if modelFake.request == nil || modelFake.request.Config == nil || modelFake.request.Config.MaxOutputTokens != 17 {
		t.Fatalf("summary output token bound = %#v", modelFake.request)
	}
}

func contains(text, fragment string) bool {
	for index := 0; index+len(fragment) <= len(text); index++ {
		if text[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
