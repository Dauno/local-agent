package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	slackUserIDPattern     = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
	slackTeamIDPattern     = regexp.MustCompile(`^T[A-Z0-9]{8,}$`)
	slackChannelIDPattern  = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)
	projectNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// FieldError identifies one invalid configuration field.
type FieldError struct {
	Field   string
	Problem string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s %s", e.Field, e.Problem)
}

// ValidationError aggregates all configuration problems found in one pass.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Fields) == 0 {
		return "invalid configuration"
	}

	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Error())
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// Has reports whether validation found a problem for field.
func (e *ValidationError) Has(field string) bool {
	if e == nil {
		return false
	}
	for _, problem := range e.Fields {
		if problem.Field == field {
			return true
		}
	}
	return false
}

// Validate checks cfg without mutating it and reports all actionable problems.
func Validate(cfg Config) error {
	var problems []FieldError
	requirePath(&problems, "state.dir", cfg.State.Dir)
	requirePath(&problems, "state.db", cfg.State.DB)
	validateContext(&problems, cfg)
	validateRuntimeAndSlack(&problems, cfg)
	validateOrchestration(&problems, cfg)

	if len(problems) > 0 {
		return &ValidationError{Fields: problems}
	}
	return nil
}

func validateContext(problems *[]FieldError, cfg Config) {
	if cfg.Context.MaxMessages <= 0 {
		addConfigProblem(problems, "context.max_messages", "must be greater than zero")
	}
	if cfg.Context.MaxChars <= 0 {
		addConfigProblem(problems, "context.max_chars", "must be greater than zero")
	}
	if cfg.Context.RetainMessagesPerConversation <= 0 {
		addConfigProblem(problems, "context.retain_messages_per_conversation", "must be greater than zero")
	}
	if cfg.Context.ModelBudget != nil {
		validateModelBudget(problems, *cfg.Context.ModelBudget)
	} else {
		addConfigProblem(problems, "context.model_budget", "must be configured")
	}
	if cfg.Context.RecoverableResults != nil {
		validateRecoverableResults(problems, *cfg.Context.RecoverableResults)
	} else {
		addConfigProblem(problems, "context.recoverable_results", "must be configured")
	}
	if cfg.Context.ContextFeatures == nil {
		addConfigProblem(problems, "context.context_features", "must be configured")
	} else if cfg.Context.ContextFeatures.ModelBudgetEnabled && !cfg.Context.ContextFeatures.RecoverableResultsEnabled {
		addConfigProblem(problems, "context.context_features.recoverable_results_enabled", "must be enabled when model_budget_enabled is enabled")
	}
	if cfg.Context.ADKCompaction != nil {
		validateADKCompaction(problems, *cfg.Context.ADKCompaction)
	}
	if cfg.CodeIntelligence == nil {
		addConfigProblem(problems, "code_intelligence", "must be configured")
	} else if cfg.CodeIntelligence.Enabled {
		if !cfg.Sandbox.Enabled {
			addConfigProblem(problems, "code_intelligence.enabled", "requires sandbox.enabled")
		}
		if cfg.Context.ContextFeatures == nil || !cfg.Context.ContextFeatures.RecoverableResultsEnabled {
			addConfigProblem(problems, "code_intelligence.enabled", "requires context.context_features.recoverable_results_enabled")
		}
		if cfg.CodeIntelligence.MaxProcesses <= 0 {
			addConfigProblem(problems, "code_intelligence.max_processes", "must be greater than zero when enabled")
		}
		if cfg.CodeIntelligence.InitTimeoutSeconds <= 0 {
			addConfigProblem(problems, "code_intelligence.initialization_timeout_seconds", "must be greater than zero when enabled")
		}
		if cfg.CodeIntelligence.RequestTimeoutSeconds <= 0 {
			addConfigProblem(problems, "code_intelligence.request_timeout_seconds", "must be greater than zero when enabled")
		}
	}
}

func validateRuntimeAndSlack(problems *[]FieldError, cfg Config) {
	requireText := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			addConfigProblem(problems, field, "must not be empty")
		}
	}
	switch cfg.Runtime.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		addConfigProblem(problems, "runtime.log_level", "must be one of debug, info, warn, or error")
	}
	if cfg.Runtime.ModelTimeoutSeconds < 0 {
		addConfigProblem(problems, "runtime.model_timeout_seconds", "must be non-negative (0 disables the application-level timeout)")
	}
	if cfg.Runtime.SlackAPITimeoutSeconds < 0 {
		addConfigProblem(problems, "runtime.slack_api_timeout_seconds", "must be non-negative")
	}
	if cfg.Runtime.MaxConcurrentModelCalls <= 0 {
		addConfigProblem(problems, "runtime.max_concurrent_model_calls", "must be greater than zero")
	}
	if cfg.Runtime.ShutdownGraceSeconds <= 0 || cfg.Runtime.ShutdownGraceSeconds > 3600 {
		addConfigProblem(problems, "runtime.shutdown_grace_seconds", "must be between 1 and 3600")
	}
	requireText("runtime.busy_message", cfg.Runtime.BusyMessage)
	requireText("runtime.model_error_message", cfg.Runtime.ModelErrorMessage)

	requireText("slack.app_name", cfg.Slack.AppName)
	requireText("slack.bot_display_name", cfg.Slack.BotDisplayName)
	requireText("slack.unauthorized_message", cfg.Slack.UnauthorizedMessage)
	validateIDs(problems, "slack.allowed_user_ids", cfg.Slack.AllowedUserIDs, slackUserIDPattern, "a plausible Slack user ID beginning with U or W")
	validateIDs(problems, "slack.allowed_team_ids", cfg.Slack.AllowedTeamIDs, slackTeamIDPattern, "a plausible Slack team ID beginning with T")
	validateIDs(problems, "slack.allowed_channel_ids", cfg.Slack.AllowedChannelIDs, slackChannelIDPattern, "a plausible Slack public or private channel ID beginning with C or G")
	if (cfg.Slack.StandardAgent.ProgressEnabled || cfg.Slack.StandardAgent.PromptsEnabled || cfg.Slack.StandardAgent.StreamingEnabled) && !cfg.Slack.StandardAgent.ThreadedDM {
		addConfigProblem(problems, "slack.standard_agent.threaded_dm", "must be true when progress, prompts, or streaming are enabled")
	}
	if cfg.Slack.StandardAgent.UpdateIntervalSeconds < 3 {
		addConfigProblem(problems, "slack.standard_agent.update_interval_seconds", "must be at least 3")
	}
	if len(cfg.Slack.StandardAgent.SuggestedPrompts) > 5 {
		addConfigProblem(problems, "slack.standard_agent.suggested_prompts", "must contain at most 5 prompts")
	}
	if cfg.Slack.StandardAgent.PromptsEnabled && len(cfg.Slack.StandardAgent.SuggestedPrompts) == 0 {
		addConfigProblem(problems, "slack.standard_agent.suggested_prompts", "must contain at least one prompt when prompts are enabled")
	}
	for index, prompt := range cfg.Slack.StandardAgent.SuggestedPrompts {
		field := fmt.Sprintf("slack.standard_agent.suggested_prompts[%d]", index)
		if strings.TrimSpace(prompt) == "" {
			addConfigProblem(problems, field, "must not be empty")
		} else if len([]rune(prompt)) > 200 {
			addConfigProblem(problems, field, "must not exceed 200 Unicode code points")
		}
		if strings.ContainsAny(prompt, "\r\n\x00") {
			addConfigProblem(problems, field, "must be a single line without NUL bytes")
		}
	}
	validateProgressLabels(problems, cfg.Slack.StandardAgent.ProgressLabels)
	validateIDs(problems, "opencode.management.allowed_user_ids", cfg.OpenCode.Management.AllowedUserIDs, slackUserIDPattern, "a plausible Slack user ID beginning with U or W")
	validateACP(problems, cfg.ACP)

	const maxFileBytes = 5 * 1024 * 1024
	const maxFileChars = 20_000
	if cfg.Slack.Files.MaxBytesPerFile <= 0 {
		addConfigProblem(problems, "slack.files.max_bytes_per_file", "must be greater than zero")
	} else if cfg.Slack.Files.MaxBytesPerFile > maxFileBytes {
		addConfigProblem(problems, "slack.files.max_bytes_per_file", fmt.Sprintf("must not exceed %d", maxFileBytes))
	}
	if cfg.Slack.Files.MaxProcessedChars <= 0 {
		addConfigProblem(problems, "slack.files.max_processed_chars", "must be greater than zero")
	} else if cfg.Slack.Files.MaxProcessedChars > maxFileChars {
		addConfigProblem(problems, "slack.files.max_processed_chars", fmt.Sprintf("must not exceed %d", maxFileChars))
	}
	if cfg.Slack.Files.TranscriptionProfile != "" && !validProviderProfileReference(cfg.Slack.Files.TranscriptionProfile) {
		addConfigProblem(problems, "slack.files.transcription_profile", "must use provider/profile syntax without whitespace")
	}
	if cfg.Slack.Files.TranscriptionTimeoutSeconds <= 0 {
		addConfigProblem(problems, "slack.files.transcription_timeout_seconds", "must be greater than zero")
	}

	if cfg.Slack.Context.Enabled {
		if cfg.Slack.Context.MaxChars <= 0 {
			addConfigProblem(problems, "slack.context.max_chars", "must be greater than zero when enabled")
		}
		if cfg.Slack.Context.TimeoutSeconds <= 0 {
			addConfigProblem(problems, "slack.context.timeout_seconds", "must be greater than zero when enabled")
		} else if cfg.Runtime.SlackAPITimeoutSeconds > 0 && cfg.Slack.Context.TimeoutSeconds > cfg.Runtime.SlackAPITimeoutSeconds {
			addConfigProblem(problems, "slack.context.timeout_seconds", "must not exceed runtime.slack_api_timeout_seconds when that timeout is enabled")
		}
		if cfg.Slack.Context.ProfileCacheTTLMinutes <= 0 {
			addConfigProblem(problems, "slack.context.profile_cache_ttl_minutes", "must be greater than zero when enabled")
		}
		if cfg.Slack.Context.ConversationCacheTTLMinutes <= 0 {
			addConfigProblem(problems, "slack.context.conversation_cache_ttl_minutes", "must be greater than zero when enabled")
		}
	} else {
		if cfg.Slack.Context.MaxChars < 0 {
			addConfigProblem(problems, "slack.context.max_chars", "must not be negative")
		}
		if cfg.Slack.Context.TimeoutSeconds < 0 {
			addConfigProblem(problems, "slack.context.timeout_seconds", "must not be negative")
		}
		if cfg.Slack.Context.ProfileCacheTTLMinutes < 0 {
			addConfigProblem(problems, "slack.context.profile_cache_ttl_minutes", "must not be negative")
		}
		if cfg.Slack.Context.ConversationCacheTTLMinutes < 0 {
			addConfigProblem(problems, "slack.context.conversation_cache_ttl_minutes", "must not be negative")
		}
	}
}

func validateOrchestration(problems *[]FieldError, cfg Config) {
	if cfg.Orchestration.Workstreams.MaxNonTerminalTasks <= 0 || cfg.Orchestration.Workstreams.MaxNonTerminalTasks > domain.HardMaxWorkstreamTasks {
		addConfigProblem(problems, "orchestration.workstreams.max_non_terminal_tasks", fmt.Sprintf("must be between 1 and %d", domain.HardMaxWorkstreamTasks))
	}
	if cfg.Orchestration.Workstreams.MaxDependenciesPerTask <= 0 || cfg.Orchestration.Workstreams.MaxDependenciesPerTask > domain.HardMaxWorkstreamDependencies {
		addConfigProblem(problems, "orchestration.workstreams.max_dependencies_per_task", fmt.Sprintf("must be between 1 and %d", domain.HardMaxWorkstreamDependencies))
	}
	if cfg.Orchestration.Workstreams.Enabled && len(cfg.Sandbox.Projects) == 0 {
		addConfigProblem(problems, "orchestration.workstreams", "requires at least one registered sandbox project when enabled")
	}
	if cfg.Orchestration.Workstreams.SnapshotBudgetTokens < 1 || cfg.Orchestration.Workstreams.SnapshotBudgetTokens > domain.HardWorkstreamSnapshotBudgetTokens {
		addConfigProblem(problems, "orchestration.workstreams.snapshot_budget_tokens", fmt.Sprintf("must be between 1 and %d", domain.HardWorkstreamSnapshotBudgetTokens))
	}
	if cfg.Orchestration.ResultHandles.MaxProducingCallsPerStep != 1 {
		addConfigProblem(problems, "orchestration.result_handles.max_producing_calls_per_step", "must equal 1")
	}
	if cfg.Orchestration.ResultHandles.ProducingCallReserveTokens <= 0 {
		addConfigProblem(problems, "orchestration.result_handles.producing_call_reserve_tokens", "must be greater than zero")
	}
	if cfg.Orchestration.ResultHandles.Retention.ContextDays < 1 || cfg.Orchestration.ResultHandles.Retention.ContextDays > domain.HardMaxResultRetentionDays {
		addConfigProblem(problems, "orchestration.result_handles.retention.context_days", fmt.Sprintf("must be between 1 and %d", domain.HardMaxResultRetentionDays))
	}
	if cfg.Orchestration.ResultHandles.Retention.ConversationDays < 1 || cfg.Orchestration.ResultHandles.Retention.ConversationDays > domain.HardMaxResultRetentionDays {
		addConfigProblem(problems, "orchestration.result_handles.retention.conversation_days", fmt.Sprintf("must be between 1 and %d", domain.HardMaxResultRetentionDays))
	}
	if cfg.Orchestration.ResultHandles.Retention.WorkstreamDays < 1 || cfg.Orchestration.ResultHandles.Retention.WorkstreamDays > domain.HardMaxResultRetentionDays {
		addConfigProblem(problems, "orchestration.result_handles.retention.workstream_days", fmt.Sprintf("must be between 1 and %d", domain.HardMaxResultRetentionDays))
	}
	if cfg.Orchestration.ResultHandles.Retention.ExportedDays < 1 || cfg.Orchestration.ResultHandles.Retention.ExportedDays > domain.HardMaxResultRetentionDays {
		addConfigProblem(problems, "orchestration.result_handles.retention.exported_days", fmt.Sprintf("must be between 1 and %d", domain.HardMaxResultRetentionDays))
	}
	if cfg.Orchestration.Knowledge.ProjectionIntervalSeconds <= 0 {
		addConfigProblem(problems, "orchestration.knowledge.projection_interval_seconds", "must be greater than zero")
	}
	if cfg.Orchestration.Knowledge.ProjectionMaxRetries <= 0 {
		addConfigProblem(problems, "orchestration.knowledge.projection_max_retries", "must be greater than zero")
	}
	if cfg.Orchestration.Knowledge.ProjectionRetentionDays <= 0 {
		addConfigProblem(problems, "orchestration.knowledge.projection_retention_days", "must be greater than zero")
	}
	validateKnowledgeRetrieval(problems, cfg)
	validateResultAnalysis(problems, cfg)
	if cfg.Sandbox.Enabled {
		if len(cfg.Sandbox.Projects) == 0 {
			addConfigProblem(problems, "sandbox.projects", "must contain at least one registered project when enabled")
		}
		for name, path := range cfg.Sandbox.Projects {
			if strings.TrimSpace(name) == "" || len(name) > 64 || !projectNamePattern.MatchString(name) {
				addConfigProblem(problems, "sandbox.projects", "project names must use 1-64 letters, digits, dots, underscores, or hyphens")
			}
			requirePath(problems, fmt.Sprintf("sandbox.projects[%q]", name), path)
		}
		if cfg.Sandbox.CommandTimeoutSeconds <= 0 {
			addConfigProblem(problems, "sandbox.command_timeout_seconds", "must be greater than zero when enabled")
		}
		if cfg.Sandbox.MaxOutputBytes <= 0 {
			addConfigProblem(problems, "sandbox.max_output_bytes", "must be greater than zero when enabled")
		}
	}
	if cfg.Canvases.Enabled {
		if cfg.Canvases.MaxTitleChars <= 0 {
			addConfigProblem(problems, "canvases.max_title_chars", "must be greater than zero when enabled")
		}
		if cfg.Canvases.MaxContentChars <= 0 {
			addConfigProblem(problems, "canvases.max_content_chars", "must be greater than zero when enabled")
		}
		if cfg.Canvases.MaxContentBytes <= 0 {
			addConfigProblem(problems, "canvases.max_content_bytes", "must be greater than zero when enabled")
		}
		if cfg.Canvases.TimeoutSeconds <= 0 {
			addConfigProblem(problems, "canvases.timeout_seconds", "must be greater than zero when enabled")
		}
	}
	if cfg.Exports.Enabled {
		if cfg.Exports.MaxFilenameChars <= 0 {
			addConfigProblem(problems, "exports.max_filename_chars", "must be greater than zero when enabled")
		} else if cfg.Exports.MaxFilenameChars > domain.MaxGeneratedFilenameRunes {
			addConfigProblem(problems, "exports.max_filename_chars", fmt.Sprintf("must not exceed %d", domain.MaxGeneratedFilenameRunes))
		}
		if cfg.Exports.MaxContentBytes <= 0 {
			addConfigProblem(problems, "exports.max_content_bytes", "must be greater than zero when enabled")
		} else if cfg.Exports.MaxContentBytes > domain.MaxGeneratedFileBytes {
			addConfigProblem(problems, "exports.max_content_bytes", fmt.Sprintf("must not exceed %d", domain.MaxGeneratedFileBytes))
		}
		if cfg.Exports.TimeoutSeconds <= 0 {
			addConfigProblem(problems, "exports.timeout_seconds", "must be greater than zero when enabled")
		}
	}
}

func validProviderProfileReference(value string) bool {
	if strings.Count(value, "/") != 1 || strings.ContainsAny(value, " \t\r\n\x00") {
		return false
	}
	parts := strings.SplitN(value, "/", 2)
	return parts[0] != "" && parts[1] != ""
}

// validateKnowledgeRetrieval enforces the TRD 06 static bounds and the
// static enablement gates that do not depend on the selected root type.
// Root-type gates and credential resolution belong to composition.
func validateKnowledgeRetrieval(problems *[]FieldError, cfg Config) {
	knowledge := cfg.Orchestration.Knowledge
	retrieval := knowledge.Retrieval
	embedding := retrieval.Embedding

	if knowledge.MaxCardTokens < 1 || knowledge.MaxCardTokens > domain.HardMaxKnowledgeCardBudget {
		addConfigProblem(problems, "orchestration.knowledge.max_card_tokens",
			fmt.Sprintf("must be between 1 and %d", domain.HardMaxKnowledgeCardBudget))
	}
	rangeChecks := []struct {
		field string
		value int
		min   int
		max   int
	}{
		{"orchestration.knowledge.retrieval.timeout_seconds", retrieval.TimeoutSeconds, 1, domain.HardMaxKnowledgeRetrievalTimeoutSeconds},
		{"orchestration.knowledge.retrieval.max_query_runes", retrieval.MaxQueryRunes, 1, domain.HardMaxKnowledgeRetrievalMaxQueryRunes},
		{"orchestration.knowledge.retrieval.max_candidates_per_channel", retrieval.MaxCandidatesPerChannel, 1, domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel},
		{"orchestration.knowledge.retrieval.max_cards", retrieval.MaxCards, 1, domain.HardMaxKnowledgeRetrievalMaxCards},
		{"orchestration.knowledge.retrieval.max_document_bytes", retrieval.MaxDocumentBytes, 1, domain.HardMaxKnowledgeRetrievalMaxDocumentBytes},
		{"orchestration.knowledge.retrieval.worker_interval_seconds", retrieval.WorkerIntervalSeconds, 1, domain.HardMaxKnowledgeRetrievalWorkerIntervalSeconds},
		{"orchestration.knowledge.retrieval.worker_max_retries", retrieval.WorkerMaxRetries, 1, domain.HardMaxKnowledgeRetrievalWorkerMaxRetries},
		{"orchestration.knowledge.retrieval.worker_batch_size", retrieval.WorkerBatchSize, 1, domain.HardMaxKnowledgeRetrievalWorkerBatchSize},
		{"orchestration.knowledge.retrieval.embedding.timeout_seconds", embedding.TimeoutSeconds, 1, domain.HardMaxKnowledgeEmbeddingTimeoutSeconds},
		{"orchestration.knowledge.retrieval.embedding.dimensions", embedding.Dimensions, 0, domain.HardMaxKnowledgeEmbeddingDimensions},
		{"orchestration.knowledge.retrieval.embedding.min_similarity_basis_points", embedding.MinSimilarityBasisPoints, 0, domain.HardMaxKnowledgeMinSimilarityBasisPoints},
	}
	for _, check := range rangeChecks {
		if check.value < check.min || check.value > check.max {
			addConfigProblem(problems, check.field, fmt.Sprintf("must be between %d and %d", check.min, check.max))
		}
	}

	if retrieval.Enabled && !knowledge.Enabled {
		addConfigProblem(problems, "orchestration.knowledge.retrieval.enabled", "requires orchestration.knowledge.enabled")
	}
	if retrieval.Enabled && (cfg.Context.ContextFeatures == nil || !cfg.Context.ContextFeatures.ModelBudgetEnabled) {
		addConfigProblem(problems, "orchestration.knowledge.retrieval.enabled", "requires context.context_features.model_budget_enabled")
	}
	if embedding.Enabled && !retrieval.Enabled {
		addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.enabled", "requires orchestration.knowledge.retrieval.enabled")
	}
	if embedding.Enabled {
		if strings.TrimSpace(embedding.ProviderID) == "" {
			addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.provider_id", "must not be empty when embedding is enabled")
		}
		if strings.TrimSpace(embedding.Model) == "" {
			addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.model", "must not be empty when embedding is enabled")
		}
		if !validBoundedEnvironmentName(embedding.APIKeyEnv) {
			addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.api_key_env", "must be a bounded valid environment variable name when embedding is enabled")
		}
		if embedding.Dimensions < 1 {
			addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.dimensions", fmt.Sprintf("must be between 1 and %d when embedding is enabled", domain.HardMaxKnowledgeEmbeddingDimensions))
		}
		if embedding.MinSimilarityBasisPoints < 1 {
			addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.min_similarity_basis_points", fmt.Sprintf("must be between 1 and %d when embedding is enabled", domain.HardMaxKnowledgeMinSimilarityBasisPoints))
		}
		validateEmbeddingBaseURL(problems, "orchestration.knowledge.retrieval.embedding.base_url", embedding.BaseURL)
	} else if strings.TrimSpace(embedding.BaseURL) != "" {
		validateEmbeddingBaseURL(problems, "orchestration.knowledge.retrieval.embedding.base_url", embedding.BaseURL)
	}
	if embedding.APIKeyEnv != "" && !validBoundedEnvironmentName(embedding.APIKeyEnv) {
		addConfigProblem(problems, "orchestration.knowledge.retrieval.embedding.api_key_env", "must be a bounded valid environment variable name")
	}
	for field, value := range map[string]string{
		"provider_id": embedding.ProviderID,
		"model":       embedding.Model,
	} {
		validateBoundedSingleLine(problems, "orchestration.knowledge.retrieval.embedding."+field, value)
	}
}

// validateEmbeddingBaseURL enforces the embedding endpoint contract: an
// absolute URL with no userinfo, query, or fragment; HTTPS everywhere except
// loopback HTTP; redirects are rejected later by the adapter.
func validateEmbeddingBaseURL(problems *[]FieldError, field, value string) {
	if strings.TrimSpace(value) == "" {
		addConfigProblem(problems, field, "must not be empty")
		return
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		addConfigProblem(problems, field, "must be an absolute http or https URL")
		return
	}
	if parsed.User != nil {
		addConfigProblem(problems, field, "must not contain userinfo")
	}
	if parsed.RawQuery != "" {
		addConfigProblem(problems, field, "must not contain a query")
	}
	if parsed.Fragment != "" {
		addConfigProblem(problems, field, "must not contain a fragment")
	}
	if parsed.Scheme == "http" {
		switch strings.ToLower(parsed.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
		default:
			addConfigProblem(problems, field, "must use https except for loopback endpoints")
		}
	}
}

// validateResultAnalysis enforces the TRD 07 static hard bounds for
// orchestration.result_analysis. Every field must be positive and within its
// domain hard maximum; non-positive values are rejected, never silently
// defaulted. The optional model block is validated only when enabled, the
// same opt-in pattern as orchestration.knowledge.retrieval.embedding.
func validateResultAnalysis(problems *[]FieldError, cfg Config) {
	analysis := cfg.Orchestration.ResultAnalysis
	evidence := analysis.Evidence
	model := analysis.Model

	rangeChecks := []struct {
		field string
		value int
		max   int
	}{
		{"orchestration.result_analysis.max_segment_bytes", analysis.MaxSegmentBytes, domain.HardMaxAnalysisSegmentBytes},
		{"orchestration.result_analysis.overlap_basis_points", analysis.OverlapBasisPoints, domain.HardMaxAnalysisOverlapBasisPoints},
		{"orchestration.result_analysis.overlap_max_bytes", analysis.OverlapMaxBytes, domain.HardMaxAnalysisOverlapBytes},
		{"orchestration.result_analysis.max_leaves", analysis.MaxLeaves, domain.HardMaxAnalysisLeaves},
		{"orchestration.result_analysis.max_reduction_fan_in", analysis.MaxReductionFanIn, domain.HardMaxAnalysisReductionFanIn},
		{"orchestration.result_analysis.max_reduction_depth", analysis.MaxReductionDepth, domain.HardMaxAnalysisReductionDepth},
		{"orchestration.result_analysis.max_concurrent_leaves", analysis.MaxConcurrentLeaves, domain.HardMaxAnalysisConcurrentLeaves},
		{"orchestration.result_analysis.max_attempts_per_step", analysis.MaxAttemptsPerStep, domain.HardMaxAnalysisAttemptsPerStep},
		{"orchestration.result_analysis.call_timeout_seconds", analysis.CallTimeoutSeconds, domain.HardMaxAnalysisCallTimeoutSeconds},
		{"orchestration.result_analysis.wall_time_seconds", analysis.WallTimeSeconds, domain.HardMaxAnalysisWallTimeSeconds},
		{"orchestration.result_analysis.evidence.excerpt_bytes", evidence.ExcerptBytes, domain.HardMaxAnalysisEvidenceExcerptBytes},
		{"orchestration.result_analysis.evidence.selectors_per_leaf", evidence.SelectorsPerLeaf, domain.HardMaxAnalysisEvidencePerLeaf},
		{"orchestration.result_analysis.evidence.references_per_packet", evidence.ReferencesPerPacket, domain.HardMaxAnalysisEvidencePerPacket},
		{"orchestration.result_analysis.evidence.bundle_bytes", evidence.BundleBytes, domain.HardMaxAnalysisBundleBytes},
	}
	for _, check := range rangeChecks {
		if check.value < 1 || check.value > check.max {
			addConfigProblem(problems, check.field, fmt.Sprintf("must be between 1 and %d", check.max))
		}
	}
	if analysis.WorkerIntervalSeconds < 1 {
		addConfigProblem(problems, "orchestration.result_analysis.worker_interval_seconds", "must be greater than zero")
	}

	// FIND-097: TRD 07 requires the reduction tree to be satisfiable at
	// configuration load, not deferred to when an analysis actually runs.
	// This calls the exact rule domain.AnalysisLimits.Validate uses
	// (domain.AnalysisReductionTreeSatisfiable) instead of reimplementing
	// the fan-in/depth/leaves arithmetic here, so the rule has one
	// definition even though it is enforced at two boundaries.
	if !domain.AnalysisReductionTreeSatisfiable(analysis.MaxReductionFanIn, analysis.MaxReductionDepth, analysis.MaxLeaves) {
		addConfigProblem(problems, "orchestration.result_analysis",
			fmt.Sprintf("max_reduction_fan_in %d raised to max_reduction_depth %d cannot cover max_leaves %d",
				analysis.MaxReductionFanIn, analysis.MaxReductionDepth, analysis.MaxLeaves))
	}

	if model.Enabled {
		if strings.TrimSpace(model.ProviderID) == "" {
			addConfigProblem(problems, "orchestration.result_analysis.model.provider_id", "must not be empty when the analysis model profile is enabled")
		}
		if strings.TrimSpace(model.Model) == "" {
			addConfigProblem(problems, "orchestration.result_analysis.model.model", "must not be empty when the analysis model profile is enabled")
		}
		if !validBoundedEnvironmentName(model.APIKeyEnv) {
			addConfigProblem(problems, "orchestration.result_analysis.model.api_key_env", "must be a bounded valid environment variable name when the analysis model profile is enabled")
		}
		validateEmbeddingBaseURL(problems, "orchestration.result_analysis.model.base_url", model.BaseURL)
	} else if strings.TrimSpace(model.BaseURL) != "" {
		validateEmbeddingBaseURL(problems, "orchestration.result_analysis.model.base_url", model.BaseURL)
	}
	if model.APIKeyEnv != "" && !validBoundedEnvironmentName(model.APIKeyEnv) {
		addConfigProblem(problems, "orchestration.result_analysis.model.api_key_env", "must be a bounded valid environment variable name")
	}
	// FIND-099: the analysis profile's reasoning_effort must validate
	// against the same closed set as the main model profile
	// (model.reasoning_effort, checked below in Validate). validReasoningEffort
	// is the one shared definition of that set. Unlike the main profile,
	// which always has an effective reasoning_effort, an empty value is
	// valid here: it means "unspecified", and the effective analysis
	// profile then falls back to the main model's reasoning_effort, the
	// same way an empty provider_id or model falls back when the whole
	// analysis model block is not enabled. A non-empty value must still be
	// one of the closed set.
	if model.ReasoningEffort != "" && !validReasoningEffort(model.ReasoningEffort) {
		addConfigProblem(problems, "orchestration.result_analysis.model.reasoning_effort", "must be empty or one of none, minimal, low, medium, high, or xhigh")
	}
	for field, value := range map[string]string{
		"provider_id":      model.ProviderID,
		"model":            model.Model,
		"reasoning_effort": model.ReasoningEffort,
	} {
		validateBoundedSingleLine(problems, "orchestration.result_analysis.model."+field, value)
	}
}

// validReasoningEffort reports whether value is one of the closed reasoning
// effort levels. FIND-099: this is the one shared definition of that set,
// used both by the main model.reasoning_effort field and by
// orchestration.result_analysis.model.reasoning_effort, so the two can never
// diverge again. The empty string is deliberately not accepted here: the
// analysis profile's caller treats "" as its own separate "unspecified,
// fall back to the main profile" case before ever calling this function.
func validReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func validateBoundedSingleLine(problems *[]FieldError, field, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		addConfigProblem(problems, field, "must be a single line without NUL bytes")
		return
	}
	if utf8.RuneCountInString(value) > 128 {
		addConfigProblem(problems, field, "must not exceed 128 Unicode code points")
	}
}

// validBoundedEnvironmentName reports whether the value matches the safe
// environment identifier syntax and stays within the frozen bounded-value
// contract shared with the other embedding identifiers.
func validBoundedEnvironmentName(value string) bool {
	if value == "" || utf8.RuneCountInString(value) > 128 {
		return false
	}
	return environmentNamePattern.MatchString(value)
}

// validateProgressLabels restricts slack.standard_agent.progress_labels to the
// six known progress states, with non-empty single-line labels bounded by the
// Slack markdown_text limit (see domain.ProgressLabelMaxRunes).
func validateProgressLabels(problems *[]FieldError, labels map[domain.ProgressState]string) {
	valid := map[domain.ProgressState]bool{
		domain.ProgressWorking:             true,
		domain.ProgressWaitingConfirmation: true,
		domain.ProgressFinalizing:          true,
		domain.ProgressCleared:             true,
		domain.ProgressFailed:              true,
		domain.ProgressInterrupted:         true,
	}
	states := make([]domain.ProgressState, 0, len(labels))
	for state := range labels {
		states = append(states, state)
	}
	slices.Sort(states)
	for _, state := range states {
		label := labels[state]
		field := fmt.Sprintf("slack.standard_agent.progress_labels[%q]", state)
		if !valid[state] {
			addConfigProblem(problems, field, "must be one of working, waiting_confirmation, finalizing, cleared, failed, or interrupted")
			continue
		}
		if strings.TrimSpace(label) == "" {
			addConfigProblem(problems, field, "must not be empty")
		} else if strings.ContainsAny(label, "\r\n\x00") {
			addConfigProblem(problems, field, "must be a single line without NUL bytes")
		} else if utf8.RuneCountInString(label) > domain.ProgressLabelMaxRunes {
			addConfigProblem(problems, field, fmt.Sprintf("must not exceed %d Unicode code points", domain.ProgressLabelMaxRunes))
		}
	}
}

func validateModelBudget(problems *[]FieldError, cfg ModelBudgetConfig) {
	const (
		minPercent = 20
		maxPercent = 80
	)
	if cfg.MaxRequestPercent < minPercent || cfg.MaxRequestPercent > maxPercent {
		addConfigProblem(problems, "context.model_budget.max_request_percent",
			fmt.Sprintf("must be between %d and %d", minPercent, maxPercent))
	}
}

func validateRecoverableResults(problems *[]FieldError, cfg RecoverableResultsConfig) {
	if cfg.MaxResultBytes <= 0 {
		addConfigProblem(problems, "context.recoverable_results.max_result_bytes", "must be greater than zero")
	}
	if cfg.ChunkMaxBytes <= 0 {
		addConfigProblem(problems, "context.recoverable_results.chunk_max_bytes", "must be greater than zero")
	} else if cfg.MaxResultBytes > 0 && cfg.ChunkMaxBytes > int(cfg.MaxResultBytes) {
		addConfigProblem(problems, "context.recoverable_results.chunk_max_bytes", "must not exceed max_result_bytes")
	}
	if cfg.RetentionDays <= 0 {
		addConfigProblem(problems, "context.recoverable_results.retention_days", "must be greater than zero")
	}
	if cfg.CleanupBatchSize <= 0 {
		addConfigProblem(problems, "context.recoverable_results.cleanup_batch_size", "must be greater than zero")
	}
}

func validateADKCompaction(problems *[]FieldError, cfg ADKCompactionConfig) {
	positive := []struct {
		field string
		value int
	}{
		{"context.adk_compaction.max_history_chars", cfg.MaxHistoryChars},
		{"context.adk_compaction.recent_turns", cfg.RecentTurns},
		{"context.adk_compaction.summary_max_chars", cfg.SummaryMaxChars},
		{"context.adk_compaction.summary_budget_tokens", cfg.SummaryBudgetTokens},
	}
	for _, item := range positive {
		if item.value <= 0 {
			addConfigProblem(problems, item.field, "must be greater than zero")
		}
	}
	if cfg.SummaryEnabled && cfg.SummaryMaxChars >= cfg.MaxHistoryChars && cfg.MaxHistoryChars > 0 && cfg.SummaryMaxChars > 0 {
		addConfigProblem(problems, "context.adk_compaction.summary_max_chars", "must be smaller than max_history_chars")
	}
	if cfg.SummaryMaxChars > MaxSQLiteSummaryChars {
		addConfigProblem(problems, "context.adk_compaction.summary_max_chars", "must not exceed SQLite summary limit of 8000 code points")
	}
	if cfg.SummaryEnabled && cfg.MaxHistoryChars > 0 && cfg.SummaryMaxChars > 0 && cfg.MaxHistoryChars <= cfg.SummaryMaxChars+1000 {
		addConfigProblem(problems, "context.adk_compaction.max_history_chars", "must reserve more than 1000 code points for one non-empty user content after the summary")
	}
	if !cfg.SummaryEnabled && cfg.MaxHistoryChars > 0 && cfg.MaxHistoryChars < 100 {
		addConfigProblem(problems, "context.adk_compaction.max_history_chars", "must reserve at least 100 code points for one non-empty user content when summaries are disabled")
	}
}

// ValidateADKCompaction applies production-only constraints that require the
// selected root provider. Generic YAML validation remains provider agnostic.
func ValidateADKCompaction(cfg Config, durableOpenAICompatible, summarizerCompatible bool) error {
	if cfg.Context.ADKCompaction == nil {
		if durableOpenAICompatible {
			return &ValidationError{Fields: []FieldError{{Field: "context.adk_compaction", Problem: "must be configured and enabled for a durable openai_compatible root"}}}
		}
		return nil
	}
	var problems []FieldError
	validateADKCompaction(&problems, *cfg.Context.ADKCompaction)
	if durableOpenAICompatible && !cfg.Context.ADKCompaction.Enabled {
		problems = append(problems, FieldError{Field: "context.adk_compaction.enabled", Problem: "must be true for a durable openai_compatible root"})
	}
	if durableOpenAICompatible && cfg.Context.ADKCompaction.SummaryEnabled && !summarizerCompatible {
		problems = append(problems, FieldError{Field: "context.adk_compaction.summary_enabled", Problem: "requires a compatible non-streaming no-tool summarizer composition"})
	}
	if len(problems) > 0 {
		return &ValidationError{Fields: problems}
	}
	return nil
}

func validateACP(problems *[]FieldError, cfg ACPConfig) {
	const (
		maxFrameCeiling    = 64 * 1024 * 1024
		maxInlineCeiling   = 16 * 1024 * 1024
		maxArtifactCeiling = 256 * 1024 * 1024
		maxStderrCeiling   = 4 * 1024 * 1024
		maxTimeoutCeiling  = 7 * 24 * 60 * 60
	)
	positive := []struct {
		field string
		value int
		max   int
	}{
		{"acp.max_frame_bytes", cfg.MaxFrameBytes, maxFrameCeiling},
		{"acp.max_inline_result_bytes", cfg.MaxInlineResultBytes, maxInlineCeiling},
		{"acp.max_result_artifact_bytes", cfg.MaxResultArtifactBytes, maxArtifactCeiling},
		{"acp.stderr_tail_bytes", cfg.StderrTailBytes, maxStderrCeiling},
		{"acp.max_job_timeout_seconds", cfg.MaxJobTimeoutSeconds, maxTimeoutCeiling},
		{"acp.worker_concurrency", cfg.WorkerConcurrency, 64},
		{"acp.artifact_retention_days", cfg.ArtifactRetentionDays, 3650},
	}
	for _, item := range positive {
		if item.value <= 0 {
			addConfigProblem(problems, item.field, "must be greater than zero")
		} else if item.value > item.max {
			addConfigProblem(problems, item.field, fmt.Sprintf("must not exceed %d", item.max))
		}
	}
	if cfg.DefaultJobTimeoutSeconds <= 0 {
		addConfigProblem(problems, "acp.default_job_timeout_seconds", "must be greater than zero")
	} else if cfg.DefaultJobTimeoutSeconds > cfg.MaxJobTimeoutSeconds {
		addConfigProblem(problems, "acp.default_job_timeout_seconds", "must not exceed acp.max_job_timeout_seconds")
	}
	if cfg.ReconciliationTimeoutSeconds <= 0 {
		addConfigProblem(problems, "acp.reconciliation_timeout_seconds", "must be greater than zero")
	} else if cfg.MaxJobTimeoutSeconds > 0 && cfg.ReconciliationTimeoutSeconds > cfg.MaxJobTimeoutSeconds {
		addConfigProblem(problems, "acp.reconciliation_timeout_seconds", "must not exceed acp.max_job_timeout_seconds")
	}
	if cfg.IdleTimeoutSeconds < 0 {
		addConfigProblem(problems, "acp.idle_timeout_seconds", "must be zero or greater")
	}
	if cfg.ProgressWarningSeconds <= 0 {
		addConfigProblem(problems, "acp.progress_warning_seconds", "must be greater than zero")
	} else if cfg.MaxJobTimeoutSeconds > 0 && cfg.ProgressWarningSeconds > cfg.MaxJobTimeoutSeconds {
		addConfigProblem(problems, "acp.progress_warning_seconds", "must not exceed acp.max_job_timeout_seconds")
	}
	if cfg.Delivery.MaxMarkdownParts < 1 || cfg.Delivery.MaxMarkdownParts > 8 {
		addConfigProblem(problems, "acp.delivery.max_markdown_parts", "must be between 1 and 8")
	}
	if cfg.Delivery.MaxFileBytes <= 0 {
		addConfigProblem(problems, "acp.delivery.max_file_bytes", "must be greater than zero")
	} else if cfg.MaxResultArtifactBytes > 0 && cfg.Delivery.MaxFileBytes > cfg.MaxResultArtifactBytes {
		addConfigProblem(problems, "acp.delivery.max_file_bytes", "must not exceed acp.max_result_artifact_bytes")
	}
	if cfg.Delivery.MaxMarkdownParts > 0 && cfg.MaxInlineResultBytes > 0 &&
		int64(cfg.MaxInlineResultBytes) > int64(cfg.Delivery.MaxMarkdownParts*domain.SlackMarkdownChunkRunes) {
		addConfigProblem(problems, "acp.max_inline_result_bytes", "must fit within configured Markdown delivery capacity")
	}
}

func addConfigProblem(problems *[]FieldError, field, problem string) {
	*problems = append(*problems, FieldError{Field: field, Problem: problem})
}

// Validate checks the receiver without mutating it.
func (cfg Config) Validate() error {
	return Validate(cfg)
}

func requirePath(problems *[]FieldError, field, value string) {
	if strings.TrimSpace(value) == "" {
		*problems = append(*problems, FieldError{Field: field, Problem: "must not be empty"})
		return
	}
	if strings.ContainsRune(value, '\x00') {
		*problems = append(*problems, FieldError{Field: field, Problem: "must not contain a NUL byte"})
	}
}

func validateIDs(problems *[]FieldError, field string, values []string, pattern *regexp.Regexp, expected string) {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		itemField := fmt.Sprintf("%s[%d]", field, index)
		if !pattern.MatchString(value) {
			*problems = append(*problems, FieldError{Field: itemField, Problem: "must be " + expected})
		}
		if _, exists := seen[value]; exists {
			*problems = append(*problems, FieldError{Field: itemField, Problem: fmt.Sprintf("duplicates %q", value)})
		}
		seen[value] = struct{}{}
	}
}
