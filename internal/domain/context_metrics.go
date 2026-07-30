package domain

// Request budget metrics (names only, not wired — instrumentation happens in Wave 4).
const (
	MetricModelRequestContextWindowTokens    = "model_request_context_window_tokens"
	MetricModelRequestHardLimitTokens        = "model_request_hard_limit_tokens"
	MetricModelRequestTokens                 = "model_request_tokens"
	MetricModelRequestUtilizationBasisPoints = "model_request_utilization_basis_points"
	MetricModelRequestCounterStrategyTotal   = "model_request_counter_strategy_total"
	MetricModelRequestGuardOutcomeTotal      = "model_request_guard_outcome_total"
	MetricModelRequestReductionTotal         = "model_request_reduction_total"
	MetricModelRequestIrreducibleTotal       = "model_request_irreducible_total"
)

// Context compiler metrics.
const (
	MetricContextProtectedTokens       = "context_protected_tokens"
	MetricContextContinuityTokens      = "context_continuity_tokens"
	MetricContextRecentTurnsRetained   = "context_recent_turns_retained"
	MetricContextResponsesExternalized = "context_responses_externalized_total"
	MetricContextTokensRemoved         = "context_tokens_removed"
	MetricContextCompileDuration       = "context_compile_duration_seconds"
	MetricContextRecountPasses         = "context_recount_passes"
)

// Recoverable results metrics.
const (
	MetricRecoverableResultPutTotal         = "recoverable_result_put_total"
	MetricRecoverableResultPutBytes         = "recoverable_result_put_bytes"
	MetricRecoverableResultChunkReadTotal   = "recoverable_result_chunk_read_total"
	MetricRecoverableResultUnavailableTotal = "recoverable_result_unavailable_total"
	MetricRecoverableResultIntegrityFailure = "recoverable_result_integrity_failure_total"
	MetricRecoverableResultCleanupTotal     = "recoverable_result_cleanup_total"
	MetricRecoverableResultActiveCount      = "recoverable_result_active_count"
)

// Continuity metrics.
const (
	MetricContinuityCheckpointCommitTotal       = "continuity_checkpoint_commit_total"
	MetricContinuityCheckpointCASConflictTotal  = "continuity_checkpoint_cas_conflict_total"
	MetricContinuityCheckpointValidationFailure = "continuity_checkpoint_validation_failure_total"
	MetricContinuityCheckpointFallbackTotal     = "continuity_checkpoint_fallback_total"
	MetricContinuityCheckpointRenderTokens      = "continuity_checkpoint_render_tokens"
)

// Code intelligence metrics.
const (
	MetricCodeReadFullTotal       = "code_read_full_total"
	MetricCodeReadRangeTotal      = "code_read_range_total"
	MetricCodeReadBytes           = "code_read_bytes"
	MetricLSPServerState          = "lsp_server_state"
	MetricLSPRequestTotal         = "lsp_request_total"
	MetricLSPRequestDuration      = "lsp_request_duration_seconds"
	MetricLSPFallbackTotal        = "lsp_fallback_total"
	MetricSyntaxQueryTotal        = "syntax_query_total"
	MetricSyntaxQueryDuration     = "syntax_query_duration_seconds"
	MetricSyntaxQueryFailureTotal = "syntax_query_failure_total"
	MetricSyntaxResultTruncated   = "syntax_result_truncated_total"
)
