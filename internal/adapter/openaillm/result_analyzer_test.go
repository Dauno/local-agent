package openaillm

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

// analysisModelFake serves one scripted response per call, in order, and
// records every request it received.
type analysisModelFake struct {
	responses []string
	calls     int
	requests  []*model.LLMRequest
}

func (m *analysisModelFake) Name() string { return "analysis-test" }

func (m *analysisModelFake) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.requests = append(m.requests, request)
	response := ""
	if m.calls < len(m.responses) {
		response = m.responses[m.calls]
	}
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText(response, genai.RoleModel), FinishReason: genai.FinishReasonStop}, nil)
	}
}

func testLeafInput() port.AnalysisLeafInput {
	return port.AnalysisLeafInput{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveText:  "which configs set the retry limit above 3?",
		Constraints:    []string{"cite only text present in the segment"},
		SegmentOrdinal: 0,
		SegmentText:    "retry_limit = 5\ntimeout = 30\n",
		PromptVersion:  LeafPromptVersion,
	}
}

func TestAnalyzeLeafNoToolsAndFencedSourceWithFixedPromptVersion(t *testing.T) {
	fake := &analysisModelFake{responses: []string{
		`{"findings":["retry_limit is set to 5"],"constraints":[],"contradictions":[],"unresolved_questions":[],"evidence_selectors":[{"offset_bytes":0,"length_bytes":16}]}`,
	}}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: modelcalllimiter.New(1)})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if err != nil {
		t.Fatalf("analyze leaf: %v", err)
	}
	if len(leaf.Findings) != 1 || leaf.Findings[0].Text != "retry_limit is set to 5" {
		t.Fatalf("leaf findings = %+v", leaf.Findings)
	}
	if leaf.SegmentOrdinal != 0 {
		t.Fatalf("leaf segment ordinal = %d, want 0", leaf.SegmentOrdinal)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("expected exactly one model call, got %d", len(fake.requests))
	}
	request := fake.requests[0]
	if len(request.Tools) != 0 {
		t.Fatalf("expected no tools on the leaf request, got %v", request.Tools)
	}
	prompt := request.Contents[0].Parts[0].Text
	if !strings.Contains(prompt, "<<<DATA>>>\nretry_limit = 5") {
		t.Fatalf("expected the segment text fenced as untrusted data, prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "no tools and no conversation history") {
		t.Fatalf("expected the no-tools/no-history statement in the prompt, prompt = %q", prompt)
	}
	if LeafPromptVersion == "" {
		t.Fatal("leaf prompt version must be a fixed non-empty constant")
	}
}

func TestAnalyzeLeafRedactsBeforeParsing(t *testing.T) {
	const secret = "sk-leaf-secret"
	fake := &analysisModelFake{responses: []string{
		`{"findings":["the token is ` + secret + `"],"constraints":[],"contradictions":[],"unresolved_questions":[],"evidence_selectors":[]}`,
	}}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: modelcalllimiter.New(1), Redact: secure.NewRedactor(secret).String})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if err != nil {
		t.Fatalf("analyze leaf: %v", err)
	}
	if len(leaf.Findings) != 1 || strings.Contains(leaf.Findings[0].Text, secret) {
		t.Fatalf("leaf finding leaked the secret: %q", leaf.Findings[0].Text)
	}
}

// TestAnalyzeLeafRetriesInvalidStructuredOutputThenSucceeds proves the
// bounded-retry-then-typed-failure contract's success path: an
// unparseable first response is retried, and a valid second response wins.
func TestAnalyzeLeafRetriesInvalidStructuredOutputThenSucceeds(t *testing.T) {
	fake := &analysisModelFake{responses: []string{
		"not json at all",
		`{"findings":["retry_limit is set to 5"],"constraints":[],"contradictions":[],"unresolved_questions":[],"evidence_selectors":[]}`,
	}}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: modelcalllimiter.New(1), MaxLeafAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if err != nil {
		t.Fatalf("expected the second attempt to succeed: %v", err)
	}
	if len(leaf.Findings) != 1 {
		t.Fatalf("leaf findings = %+v", leaf.Findings)
	}
	if fake.calls != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", fake.calls)
	}
}

// TestAnalyzeLeafFailsTypedAfterBoundedRetries proves the failure path:
// every attempt is invalid, and the call count is bounded by
// MaxLeafAttempts, never unbounded.
func TestAnalyzeLeafFailsTypedAfterBoundedRetries(t *testing.T) {
	fake := &analysisModelFake{responses: []string{
		"not json at all",
		"still not json",
		"never json",
	}}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: modelcalllimiter.New(1), MaxLeafAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if err == nil {
		t.Fatal("expected AnalyzeLeaf to fail after exhausting its bounded retries")
	}
	if !strings.Contains(err.Error(), string(domain.AnalysisFailureLeafSchemaInvalid)) {
		t.Fatalf("expected a typed analysis_leaf_schema_invalid failure, got %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("expected exactly MaxLeafAttempts=2 model calls, got %d", fake.calls)
	}
}

// TestAnalyzeLeafFailsTypedWhenLeafHasNoSurvivingContent proves a
// structurally valid but empty leaf (no findings, no unresolved questions)
// is retried and then fails typed, mirroring domain.AnalysisLeaf.Validate's
// own rule.
func TestAnalyzeLeafFailsTypedWhenLeafHasNoSurvivingContent(t *testing.T) {
	fake := &analysisModelFake{responses: []string{
		`{"findings":[],"constraints":[],"contradictions":[],"unresolved_questions":[],"evidence_selectors":[]}`,
		`{"findings":[],"constraints":[],"contradictions":[],"unresolved_questions":[],"evidence_selectors":[]}`,
	}}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: modelcalllimiter.New(1), MaxLeafAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if err == nil {
		t.Fatal("expected an empty leaf to fail typed")
	}
	if !strings.Contains(err.Error(), string(domain.AnalysisFailureLeafSchemaInvalid)) {
		t.Fatalf("expected a typed analysis_leaf_schema_invalid failure, got %v", err)
	}
}

func TestAnalyzeLeafReleasesLimiterPermitOnLimitReached(t *testing.T) {
	fake := &analysisModelFake{}
	limiter := modelcalllimiter.New(1)
	release, acquired := limiter.TryAcquire()
	if !acquired {
		t.Fatal("expected to acquire the only permit")
	}
	analyzer, err := NewResultAnalyzer(fake, ResultAnalyzerConfig{ModelCalls: limiter})
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.AnalyzeLeaf(context.Background(), testLeafInput())
	if !errors.Is(err, port.ErrModelCallLimitReached) {
		t.Fatalf("expected ErrModelCallLimitReached while the only permit is held, got %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("expected zero model calls when the limiter has no permit, got %d", fake.calls)
	}
	release()
}
