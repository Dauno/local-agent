package domain

// Request budget metrics. Values are emitted through port.MetricRecorder with
// bounded, non-sensitive labels.
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
	MetricContextProtectedTokens           = "context_protected_tokens"
	MetricContextContinuityTokens          = "context_continuity_tokens"
	MetricContextRecentTurnsRetained       = "context_recent_turns_retained"
	MetricContextResponsesExternalized     = "context_responses_externalized_total"
	MetricContextTokensRemoved             = "context_tokens_removed"
	MetricContextCompileDuration           = "context_compile_duration_seconds"
	MetricContextRecountPasses             = "context_recount_passes"
	MetricContextCountBeforeReduction      = "context_count_before_reduction"
	MetricContextCountAfterReduction       = "context_count_after_reduction"
	MetricContextLateExternalization       = "context_late_externalization_total"
	MetricContextLateExternalized          = "context_late_externalized_responses"
	MetricContextMinimumRequestTokens      = "context_minimum_request_tokens"
	MetricContextProtectedCodePoints       = "context_protected_code_points"
	MetricContextContinuityCodePoints      = "context_continuity_code_points"
	MetricContextResponseCodePointsRemoved = "context_response_code_points_removed"
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
	MetricContinuityCheckpointRenderCodePoints  = "continuity_checkpoint_render_code_points"
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

// Durable external-agent notification metrics. Labels are restricted by the
// in-process recorder to bounded, non-sensitive dimensions.
const (
	MetricExternalAgentNotificationClaimTotal       = "external_agent_notification_claim_total"
	MetricExternalAgentNotificationPublishTotal     = "external_agent_notification_publish_total"
	MetricExternalAgentNotificationFailureTotal     = "external_agent_notification_failure_total"
	MetricExternalAgentNotificationReconcileTotal   = "external_agent_notification_reconcile_total"
	MetricExternalAgentNotificationCASConflictTotal = "external_agent_notification_cas_conflict_total"
	MetricExternalAgentNotificationStuck            = "external_agent_notification_stuck"
	MetricExternalAgentStatusAuthorizationTotal     = "external_agent_status_authorization_total"
	MetricExternalAgentActivationClaimTotal         = "external_agent_activation_claim_total"
	MetricExternalAgentActivationTotal              = "external_agent_activation_total"
	MetricExternalAgentActivationReconcileTotal     = "external_agent_activation_reconcile_total"
	MetricExternalAgentActivationCASConflictTotal   = "external_agent_activation_cas_conflict_total"
	MetricExternalAgentActivationStuck              = "external_agent_activation_stuck"
)

// ACP live progress observability metrics. Labels are restricted to bounded
// enums (event kind, phase, health, outcome). Job ID, session ID, PID,
// tool-call ID, actor, conversation, and paths are forbidden as labels.
const (
	MetricExternalAgentACPProgressEventTotal          = "external_agent_acp_progress_event_total"
	MetricExternalAgentACPPhaseTransitionTotal        = "external_agent_acp_phase_transition_total"
	MetricExternalAgentACPProgressPersistFailureTotal = "external_agent_acp_progress_persist_failure_total"
	MetricExternalAgentACPInactivityWarningTotal      = "external_agent_acp_inactivity_warning_total"
	MetricExternalAgentACPActiveJobs                  = "external_agent_acp_active_jobs"
)
