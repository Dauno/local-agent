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
	MetricContinuityCheckpointRenderCodePoints  = "continuity_checkpoint_render_code_points"
)

// Knowledge retrieval metrics. Emitted with only the closed enum labels
// channel, outcome, and reason; values never carry queries, content,
// vectors, handles, digests, source references, actor, conversation, or
// credential data.
const (
	MetricKnowledgeRetrievalTotal           = "knowledge_retrieval_total"
	MetricKnowledgeRetrievalDuration        = "knowledge_retrieval_duration_seconds"
	MetricKnowledgeRetrievalCandidates      = "knowledge_retrieval_candidates"
	MetricKnowledgeRetrievalSelected        = "knowledge_retrieval_selected"
	MetricKnowledgeRetrievalEmptyTotal      = "knowledge_retrieval_empty_total"
	MetricKnowledgeRetrievalChannelFailure  = "knowledge_retrieval_channel_failure_total"
	MetricKnowledgeRetrievalStaleIndex      = "knowledge_retrieval_stale_index_total"
	MetricKnowledgeRetrievalCardTokens      = "knowledge_retrieval_card_tokens"
	MetricKnowledgeLexicalQueueDepth        = "knowledge_lexical_queue_depth"
	MetricKnowledgeEmbeddingQueueDepth      = "knowledge_embedding_queue_depth"
	MetricKnowledgeEmbeddingRequestDuration = "knowledge_embedding_request_duration_seconds"
)

// Closed knowledge retrieval metric label names.
const (
	MetricLabelChannel = "channel"
	MetricLabelOutcome = "outcome"
	MetricLabelReason  = "reason"
)

// KnowledgeRetrievalOutcome is the closed outcome label set for knowledge
// retrieval metrics.
type KnowledgeRetrievalOutcome string

const (
	KnowledgeRetrievalOutcomeSuccess            KnowledgeRetrievalOutcome = "success"
	KnowledgeRetrievalOutcomeEmpty              KnowledgeRetrievalOutcome = "empty"
	KnowledgeRetrievalOutcomeValidationRejected KnowledgeRetrievalOutcome = "validation_rejected"
	KnowledgeRetrievalOutcomeUnavailable        KnowledgeRetrievalOutcome = "unavailable"
)

// KnowledgeRetrievalReasonLabel is the closed reason label set beyond the
// shared failure categories: stale-index exclusions and oversized
// omissions.
type KnowledgeRetrievalReasonLabel string

const (
	KnowledgeRetrievalReasonLabelStaleIndex KnowledgeRetrievalReasonLabel = "stale_index"
	KnowledgeRetrievalReasonLabelOversized  KnowledgeRetrievalReasonLabel = "oversized"
)

// knowledgeRetrievalMetricLabelKeys freezes the admissible label keys per
// knowledge retrieval metric. Metrics not listed accept no labels.
var knowledgeRetrievalMetricLabelKeys = map[string]map[string]bool{
	MetricKnowledgeRetrievalTotal:           {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalDuration:        {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalCandidates:      {MetricLabelChannel: true},
	MetricKnowledgeRetrievalSelected:        {MetricLabelChannel: true},
	MetricKnowledgeRetrievalEmptyTotal:      {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalChannelFailure:  {MetricLabelChannel: true, MetricLabelReason: true},
	MetricKnowledgeRetrievalStaleIndex:      {MetricLabelChannel: true, MetricLabelReason: true},
	MetricKnowledgeRetrievalCardTokens:      {},
	MetricKnowledgeLexicalQueueDepth:        {},
	MetricKnowledgeEmbeddingQueueDepth:      {},
	MetricKnowledgeEmbeddingRequestDuration: {MetricLabelOutcome: true},
}

// knowledgeRetrievalMetricRequiredKeys freezes the mandatory label presence
// per knowledge retrieval metric: counts and observations must carry their
// attribution label, while queue depths and card tokens stay unlabeled.
var knowledgeRetrievalMetricRequiredKeys = map[string]map[string]bool{
	MetricKnowledgeRetrievalTotal:           {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalDuration:        {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalCandidates:      {MetricLabelChannel: true},
	MetricKnowledgeRetrievalSelected:        {MetricLabelChannel: true},
	MetricKnowledgeRetrievalEmptyTotal:      {MetricLabelOutcome: true},
	MetricKnowledgeRetrievalChannelFailure:  {MetricLabelChannel: true, MetricLabelReason: true},
	MetricKnowledgeRetrievalStaleIndex:      {MetricLabelChannel: true, MetricLabelReason: true},
	MetricKnowledgeEmbeddingRequestDuration: {MetricLabelOutcome: true},
}

func validKnowledgeRetrievalMetricName(metric string) bool {
	_, known := knowledgeRetrievalMetricLabelKeys[metric]
	return known
}

// IsKnowledgeRetrievalMetric reports whether name is one of the frozen
// knowledge retrieval metric names. The metrics adapter uses it to apply the
// exact per-metric label contract instead of the global allowlist.
func IsKnowledgeRetrievalMetric(metric string) bool {
	return validKnowledgeRetrievalMetricName(metric)
}

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
	// MetricExternalAgentResultIdentityInvalidTotal counts durable result
	// identity verification failures (invalid identity, byte mismatch, or
	// digest mismatch). It is label-free: no job ID, actor, conversation,
	// digest, reference, or content value is ever recorded.
	MetricExternalAgentResultIdentityInvalidTotal = "external_agent_result_identity_invalid_total"
	// MetricExternalAgentActivationSuppressionTotal counts terminal
	// foreground publications that were deliberately suppressed from creating
	// a root activation. It is label-free and carries no delivery identity.
	MetricExternalAgentActivationSuppressionTotal = "external_agent_activation_suppression_total"
)

// external-agent live progress observability metrics. Labels are restricted to bounded
// enums (event kind, phase, health, outcome). Job ID, session ID, PID,
// tool-call ID, actor, conversation, and paths are forbidden as labels.
const (
	MetricExternalAgentProgressEventTotal          = "external_agent_acp_progress_event_total"
	MetricExternalAgentPhaseTransitionTotal        = "external_agent_acp_phase_transition_total"
	MetricExternalAgentProgressPersistFailureTotal = "external_agent_acp_progress_persist_failure_total"
	MetricExternalAgentInactivityWarningTotal      = "external_agent_acp_inactivity_warning_total"
	MetricExternalAgentActiveJobs                  = "external_agent_acp_active_jobs"
)
