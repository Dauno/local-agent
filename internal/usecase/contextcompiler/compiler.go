package contextcompiler

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// Compiler implements port.ContextCompiler by producing a bounded model-facing
// projection. It externalizes oversized FunctionResponse payloads via a
// RecoverableResultStore while preserving protocol identity and ordering.
type Compiler struct {
	resultStore  port.RecoverableResultStore
	tokenCounter port.RequestTokenCounter
	metrics      port.MetricRecorder
}

// New creates a compiler. The request counter is the admission authority;
// code-point costs are retained only for deterministic allocation heuristics.
func New(resultStore port.RecoverableResultStore, counter port.RequestTokenCounter, recorders ...port.MetricRecorder) *Compiler {
	var recorder port.MetricRecorder
	if len(recorders) > 0 {
		recorder = recorders[0]
	}
	return &Compiler{resultStore: resultStore, tokenCounter: counter, metrics: recorder}
}

// Compile coordinates analysis, assembly, admission, allocation, and
// externalization phases.
func (c *Compiler) Compile(ctx context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	if c == nil {
		return domain.CompileResult{}, errors.New("context compiler: compiler is required")
	}
	if c.tokenCounter == nil {
		return domain.CompileResult{}, errors.New("context compiler: request token counter is required")
	}
	if req.FixedRequestTokens < 0 {
		return domain.CompileResult{}, fmt.Errorf("context compiler: fixed request tokens must not be negative, got %d", req.FixedRequestTokens)
	}
	if err := domain.ValidateRequestBudget(req.ModelBudget); err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: validate request budget: %w", err)
	}

	started := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.Observe(domain.MetricContextCompileDuration, time.Since(started).Seconds(), nil)
		}
	}()

	state, err := analyzeCompilation(req)
	if err != nil {
		return domain.CompileResult{}, err
	}
	if len(state.turns) == 0 {
		return domain.CompileResult{Contents: nil, Diagnostics: state.diagnostics}, nil
	}
	if c.metrics != nil && state.diagnostics.ContinuityCodePoints > 0 {
		c.metrics.Observe(domain.MetricContinuityCheckpointRenderCodePoints, float64(state.diagnostics.ContinuityCodePoints), nil)
	}

	state, err = assembleCompilation(state)
	if err != nil {
		return domain.CompileResult{}, err
	}
	state, err = c.countCompilation(ctx, state, true)
	if err != nil {
		return domain.CompileResult{}, err
	}
	if state.count.Tokens <= triggerTokens(state.hardLimit, req.ModelBudget.TriggerTokens) {
		return c.completeCompilation(state, true)
	}

	state, err = c.reduceCompilation(ctx, state)
	if err != nil {
		return c.compilationError(state, err)
	}
	return c.completeCompilation(state, false)
}

func (c *Compiler) countCompilation(ctx context.Context, state compilationState, initial bool) (compilationState, error) {
	count, err := c.countProjection(ctx, state.result.Contents, state.request.FixedRequestTokens)
	if err != nil {
		return state, err
	}
	state.count = count
	if initial {
		state.diagnostics.RequestTokensBefore = count.Tokens
		state.diagnostics.CounterStrategy = count.Strategy
	}
	return state, nil
}

func (c *Compiler) completeCompilation(state compilationState, unchanged bool) (domain.CompileResult, error) {
	if state.count.Tokens > state.hardLimit {
		state.diagnostics.RequestTokensAfter = state.count.Tokens
		state.diagnostics.ReductionReason = "irreducible"
		state.diagnostics.ReductionStage = "min_irreducible"
		c.recordDiagnostics(state.diagnostics, true)
		return domain.CompileResult{Diagnostics: state.diagnostics}, &domain.IrreducibleContextError{
			MinimumTokens: state.count.Tokens,
			HardTokens:    state.hardLimit,
		}
	}

	state.diagnostics.RequestTokensAfter = state.count.Tokens
	if afterCodePoints, err := domain.ContentCost(state.result.Contents); err == nil {
		state.diagnostics.RequestCodePointsAfter = afterCodePoints
	}
	if unchanged {
		state.diagnostics.ReductionReason = "unchanged"
	} else {
		switch {
		case state.diagnostics.ResponsesExternalized > 0 && state.diagnostics.LateExternalized:
			state.diagnostics.ReductionReason = "request_budget"
			state.diagnostics.ReductionStage = "late"
		case state.diagnostics.ResponsesExternalized > 0:
			state.diagnostics.ReductionReason = "request_budget"
			state.diagnostics.ReductionStage = "planned"
		case state.optionalEvicted || len(state.result.Contents) != len(state.request.Contents):
			state.diagnostics.ReductionReason = "bounded"
			state.diagnostics.ReductionStage = "optional"
		default:
			state.diagnostics.ReductionReason = "unchanged"
		}
	}
	c.recordDiagnostics(state.diagnostics, false)
	return domain.CompileResult{Contents: state.result.Contents, Diagnostics: state.diagnostics}, nil
}

func (c *Compiler) compilationError(state compilationState, err error) (domain.CompileResult, error) {
	if !state.exposeDiagnosticsOnError {
		return domain.CompileResult{}, err
	}
	c.recordDiagnostics(state.diagnostics, state.diagnosticsIrreducible)
	return domain.CompileResult{Diagnostics: state.diagnostics}, err
}

func (c *Compiler) recordDiagnostics(diag domain.CompileDiagnostics, irreducible bool) {
	if c == nil || c.metrics == nil {
		return
	}
	// ProtectedTokens is a legacy dashboard metric. Keep its current name and
	// value while code-point diagnostics use the explicit CodePoints fields.
	if diag.ProtectedTokens > 0 {
		c.metrics.Observe(domain.MetricContextProtectedTokens, float64(diag.ProtectedTokens), nil)
	}
	if diag.ContinuityTokens > 0 {
		c.metrics.Observe(domain.MetricContextContinuityTokens, float64(diag.ContinuityTokens), nil)
	}
	c.metrics.Observe(domain.MetricContextProtectedCodePoints, float64(diag.ProtectedCodePoints), nil)
	c.metrics.Observe(domain.MetricContextContinuityCodePoints, float64(diag.ContinuityCodePoints), nil)
	c.metrics.Observe(domain.MetricContextRecentTurnsRetained, float64(diag.RecentTurnsRetained), nil)
	if diag.ResponsesExternalized > 0 {
		c.metrics.AddCounter(domain.MetricContextResponsesExternalized, int64(diag.ResponsesExternalized), nil)
	}
	c.metrics.Observe(domain.MetricContextTokensRemoved, float64(diag.ResponseTokensRemoved), nil)
	c.metrics.Observe(domain.MetricContextResponseCodePointsRemoved, float64(diag.ResponseCodePointsRemoved), nil)
	c.metrics.Observe(domain.MetricContextRecountPasses, float64(diag.RecountPasses), nil)
	c.metrics.Observe(domain.MetricContextCountBeforeReduction, float64(diag.RequestTokensBefore), nil)
	c.metrics.Observe(domain.MetricContextCountAfterReduction, float64(diag.RequestTokensAfter), nil)
	if diag.ProtectedTokens > 0 {
		c.metrics.Observe(domain.MetricContextMinimumRequestTokens, float64(diag.ProtectedTokens), nil)
	}
	if diag.LateExternalized || diag.ReductionStage == "late" {
		c.metrics.AddCounter(domain.MetricContextLateExternalization, 1, nil)
		c.metrics.AddCounter(domain.MetricContextLateExternalized, int64(diag.ResponsesExternalized), nil)
	}
	reductionReason := diag.ReductionStage
	if reductionReason == "" {
		reductionReason = diag.ReductionReason
	}
	if reductionReason == "irreducible" || reductionReason == "min_irreducible" {
		switch {
		case diag.ResponsesExternalized > 0:
			reductionReason = "late"
		case diag.RecountPasses > 0:
			reductionReason = "optional"
		default:
			reductionReason = ""
		}
	}
	if reductionReason != "" && reductionReason != "unchanged" && reductionReason != "empty" {
		c.metrics.AddCounter(domain.MetricModelRequestReductionTotal, 1, port.MetricLabels{"reduction_reason": reductionReason})
	}
	if irreducible {
		c.metrics.AddCounter(domain.MetricModelRequestIrreducibleTotal, 1, port.MetricLabels{"guard_outcome": "irreducible"})
	}
}

func (c *Compiler) countProjection(ctx context.Context, contents []domain.Content, fixedTokens int) (port.TokenCount, error) {
	serialized, err := domain.CanonicalJSON(contents)
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("context compiler: serialize final projection: %w", err)
	}
	count, err := c.tokenCounter.CountRequest(ctx, port.ModelRequestEnvelope{SerializerID: port.SerializerContextProjectionV1, Serialized: string(serialized)})
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("request_token_count_unavailable: %w", err)
	}
	if count.Tokens < 0 {
		return port.TokenCount{}, errors.New("request_token_count_unavailable: counter returned a negative token count")
	}
	// A non-empty request with a zero count is not meaningful. Fail closed for
	// injected counters while preserving the byte-bound compatibility fallback.
	if count.Tokens == 0 && len(serialized) > 256 {
		if count.Strategy != "byte_bound" {
			return port.TokenCount{}, errors.New("request_token_count_unavailable: counter returned zero for a non-empty request")
		}
		count.Tokens = utf8.RuneCount(serialized)
	}
	// The byte-bound counter already measures one serialized projection. The
	// provider-shaped final guard performs the authoritative provider count.
	count.Tokens = sumCosts([]int{count.Tokens, fixedTokens})
	return count, nil
}

func triggerTokens(hard, trigger int) int {
	if trigger <= 0 || trigger > hard {
		return hard
	}
	return trigger
}
