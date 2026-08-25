// Package config owns the non-sensitive, typed project configuration.
package config

import "github.com/Dauno/slack-local-agent/internal/domain"

const (
	DefaultProjectStateDir = ".local-agent"
	DefaultDatabaseFile    = ".local-agent/local-agent.db"
	DefaultConfigFile      = ".local-agent/config.yaml"
	DefaultManifestFile    = ".local-agent/app-manifest.local.yaml"
	DefaultEnvExampleFile  = ".local-agent/local.env.example"
	DefaultEnvFile         = ".env"
)

const (
	DefaultBusyMessage         = "El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos."
	DefaultModelErrorMessage   = "No pude completar la respuesta por un error del modelo. Intenta de nuevo."
	DefaultUnauthorizedMessage = "No tienes permiso para usar este bot. Pide acceso a quien administra local-agent."
)

const MaxSQLiteSummaryChars = domain.MaxPersistedSummaryChars

// Config is the complete non-sensitive configuration stored in config.yaml.
// Provider credentials are resolved from declarative definitions separately.
type Config struct {
	State            StateConfig             `yaml:"state"`
	Context          ContextConfig           `yaml:"context"`
	Runtime          RuntimeConfig           `yaml:"runtime"`
	Slack            SlackConfig             `yaml:"slack"`
	Sandbox          SandboxConfig           `yaml:"sandbox"`
	Canvases         CanvasesConfig          `yaml:"canvases"`
	Exports          ExportsConfig           `yaml:"exports"`
	ACP              ACPConfig               `yaml:"acp"`
	CodeIntelligence *CodeIntelligenceConfig `yaml:"code_intelligence"`
	Orchestration    OrchestrationConfig     `yaml:"orchestration"`

	document *sourceDocument
}

type CodeIntelligenceConfig struct {
	Enabled               bool                      `yaml:"enabled"`
	MaxProcesses          int                       `yaml:"max_processes"`
	InitTimeoutSeconds    int                       `yaml:"initialization_timeout_seconds"`
	RequestTimeoutSeconds int                       `yaml:"request_timeout_seconds"`
	LSPServers            []LSPServerConfig         `yaml:"lsp_servers"`
	LSPRoutes             map[string]LSPRouteConfig `yaml:"lsp_routes"`
}

type LSPServerConfig struct {
	ID        string   `yaml:"id"`
	Command   string   `yaml:"command"`
	Args      []string `yaml:"args,omitempty"`
	Languages []string `yaml:"languages"`
}

type LSPRouteConfig struct {
	Priority []string `yaml:"priority"`
	Fallback string   `yaml:"fallback,omitempty"`
}

type ACPConfig struct {
	MaxInlineResultBytes         int               `yaml:"max_inline_result_bytes"`
	MaxResultArtifactBytes       int               `yaml:"max_result_artifact_bytes"`
	DefaultJobTimeoutSeconds     int               `yaml:"default_job_timeout_seconds"`
	MaxJobTimeoutSeconds         int               `yaml:"max_job_timeout_seconds"`
	ReconciliationTimeoutSeconds int               `yaml:"reconciliation_timeout_seconds"`
	ProgressWarningSeconds       int               `yaml:"progress_warning_seconds"`
	WorkerConcurrency            int               `yaml:"worker_concurrency"`
	ArtifactRetentionDays        int               `yaml:"artifact_retention_days"`
	Delivery                     ACPDeliveryConfig `yaml:"delivery"`
}

type ACPDeliveryConfig struct {
	MaxMarkdownParts int `yaml:"max_markdown_parts"`
	MaxFileBytes     int `yaml:"max_file_bytes"`
}

type StateConfig struct {
	Dir string `yaml:"dir"`
	DB  string `yaml:"db"`
}

type ContextFeaturesConfig struct {
	ModelBudgetEnabled        bool `yaml:"model_budget_enabled"`
	RecoverableResultsEnabled bool `yaml:"recoverable_results_enabled"`
	ContinuityCapsuleEnabled  bool `yaml:"continuity_capsule_enabled"`
}

type ContextConfig struct {
	MaxMessages                   int                       `yaml:"max_messages"`
	MaxChars                      int                       `yaml:"max_chars"`
	RetainMessagesPerConversation int                       `yaml:"retain_messages_per_conversation"`
	ADKCompaction                 *ADKCompactionConfig      `yaml:"adk_compaction"`
	ModelBudget                   *ModelBudgetConfig        `yaml:"model_budget"`
	RecoverableResults            *RecoverableResultsConfig `yaml:"recoverable_results"`
	ContextFeatures               *ContextFeaturesConfig    `yaml:"context_features"`
}

type ModelBudgetConfig struct {
	MaxRequestPercent int `yaml:"max_request_percent"`
}

type RecoverableResultsConfig struct {
	MaxResultBytes   int64 `yaml:"max_result_bytes"`
	ChunkMaxBytes    int   `yaml:"chunk_max_bytes"`
	RetentionDays    int   `yaml:"retention_days"`
	CleanupBatchSize int   `yaml:"cleanup_batch_size"`
}

type ADKCompactionConfig struct {
	Enabled             bool `yaml:"enabled"`
	MaxHistoryChars     int  `yaml:"max_history_chars"`
	RecentTurns         int  `yaml:"recent_turns"`
	SummaryEnabled      bool `yaml:"summary_enabled"`
	SummaryMaxChars     int  `yaml:"summary_max_chars"`
	SummaryBudgetTokens int  `yaml:"summary_budget_tokens"`
}

type RuntimeConfig struct {
	LogLevel                string `yaml:"log_level"`
	ModelTimeoutSeconds     int    `yaml:"model_timeout_seconds"`
	SlackAPITimeoutSeconds  int    `yaml:"slack_api_timeout_seconds"`
	MaxConcurrentModelCalls int    `yaml:"max_concurrent_model_calls"`
	ShutdownGraceSeconds    int    `yaml:"shutdown_grace_seconds"`
	BusyMessage             string `yaml:"busy_message"`
	ModelErrorMessage       string `yaml:"model_error_message"`
}

type SlackConfig struct {
	AppName             string                   `yaml:"app_name"`
	BotDisplayName      string                   `yaml:"bot_display_name"`
	UnauthorizedMessage string                   `yaml:"unauthorized_message"`
	AllowAllUsers       bool                     `yaml:"allow_all_users"`
	AllowedUserIDs      []string                 `yaml:"allowed_user_ids"`
	AllowedTeamIDs      []string                 `yaml:"allowed_team_ids"`
	AllowedChannelIDs   []string                 `yaml:"allowed_channel_ids"`
	PartLabels          bool                     `yaml:"part_labels"`
	StandardAgent       SlackStandardAgentConfig `yaml:"standard_agent"`
	Context             SlackContextConfig       `yaml:"context"`
	Files               SlackFilesConfig         `yaml:"files"`
}

// SlackStandardAgentConfig gates incompatible standard Slack agent behavior.
type SlackStandardAgentConfig struct {
	ThreadedDM            bool                            `yaml:"threaded_dm"`
	ProgressEnabled       bool                            `yaml:"progress_enabled"`
	PromptsEnabled        bool                            `yaml:"prompts_enabled"`
	SuggestedPrompts      []string                        `yaml:"suggested_prompts"`
	StreamingEnabled      bool                            `yaml:"streaming_enabled"`
	UpdateIntervalSeconds int                             `yaml:"update_interval_seconds"`
	ProgressLabels        map[domain.ProgressState]string `yaml:"progress_labels"`
}

type SlackFilesConfig struct {
	MaxBytesPerFile             int    `yaml:"max_bytes_per_file"`
	MaxProcessedChars           int    `yaml:"max_processed_chars"`
	TranscriptionProfile        string `yaml:"transcription_profile"`
	TranscriptionTimeoutSeconds int    `yaml:"transcription_timeout_seconds"`
}

type SlackContextConfig struct {
	Enabled                     bool `yaml:"enabled"`
	MaxChars                    int  `yaml:"max_chars"`
	TimeoutSeconds              int  `yaml:"timeout_seconds"`
	ProfileCacheTTLMinutes      int  `yaml:"profile_cache_ttl_minutes"`
	ConversationCacheTTLMinutes int  `yaml:"conversation_cache_ttl_minutes"`
}

type SandboxConfig struct {
	Enabled               bool              `yaml:"enabled"`
	Projects              map[string]string `yaml:"projects"`
	CommandTimeoutSeconds int               `yaml:"command_timeout_seconds"`
	MaxOutputBytes        int               `yaml:"max_output_bytes"`
}

type CanvasesConfig struct {
	Enabled         bool `yaml:"enabled"`
	MaxTitleChars   int  `yaml:"max_title_chars"`
	MaxContentChars int  `yaml:"max_content_chars"`
	MaxContentBytes int  `yaml:"max_content_bytes"`
	TimeoutSeconds  int  `yaml:"timeout_seconds"`
}

type ExportsConfig struct {
	Enabled          bool `yaml:"enabled"`
	MaxFilenameChars int  `yaml:"max_filename_chars"`
	MaxContentBytes  int  `yaml:"max_content_bytes"`
	TimeoutSeconds   int  `yaml:"timeout_seconds"`
}

type OrchestrationConfig struct {
	Workstreams    WorkstreamConfig     `yaml:"workstreams"`
	ResultHandles  ResultHandlesConfig  `yaml:"result_handles"`
	Knowledge      KnowledgeConfig      `yaml:"knowledge"`
	ResultAnalysis ResultAnalysisConfig `yaml:"result_analysis"`
}

type WorkstreamConfig struct {
	Enabled                bool `yaml:"enabled"`
	MaxNonTerminalTasks    int  `yaml:"max_non_terminal_tasks"`
	MaxDependenciesPerTask int  `yaml:"max_dependencies_per_task"`
	// SnapshotBudgetTokens is the provider-shaped per-turn source budget for
	// the active workstream snapshot admitted into a normal human turn's
	// frame. Zero disables snapshot admission.
	SnapshotBudgetTokens int `yaml:"snapshot_budget_tokens"`
}

// KnowledgeConfig gates the TRD 05 scoped knowledge command surface. When
// disabled, memory-human commands receive a deterministic disabled response
// and never mutate state or reach the model, and the projection worker is
// not started. Projection settings are projection-specific. MaxCardTokens
// and Retrieval carry the TRD 06 contracts: card budgets, retrieval bounds,
// and the optional embedding configuration. Defaults keep retrieval and
// embeddings disabled.
type KnowledgeConfig struct {
	Enabled                   bool                     `yaml:"enabled"`
	ProjectionIntervalSeconds int                      `yaml:"projection_interval_seconds"`
	ProjectionMaxRetries      int                      `yaml:"projection_max_retries"`
	ProjectionRetentionDays   int                      `yaml:"projection_retention_days"`
	MaxCardTokens             int                      `yaml:"max_card_tokens"`
	Retrieval                 KnowledgeRetrievalConfig `yaml:"retrieval"`
}

// KnowledgeRetrievalConfig carries the TRD 06 retrieval bounds. Embeddings
// are opt-in: enabled requires retrieval enabled and a fully configured
// embedding block; disabled leaves the embedding fields inert.
type KnowledgeRetrievalConfig struct {
	Enabled                 bool                     `yaml:"enabled"`
	TimeoutSeconds          int                      `yaml:"timeout_seconds"`
	MaxQueryRunes           int                      `yaml:"max_query_runes"`
	MaxCandidatesPerChannel int                      `yaml:"max_candidates_per_channel"`
	MaxCards                int                      `yaml:"max_cards"`
	MaxDocumentBytes        int                      `yaml:"max_document_bytes"`
	WorkerIntervalSeconds   int                      `yaml:"worker_interval_seconds"`
	WorkerMaxRetries        int                      `yaml:"worker_max_retries"`
	WorkerBatchSize         int                      `yaml:"worker_batch_size"`
	Embedding               KnowledgeEmbeddingConfig `yaml:"embedding"`
}

// KnowledgeEmbeddingConfig names an opaque OpenAI-compatible embedding
// endpoint. There is no default provider, endpoint, model, dimensions, or
// threshold; the API key is resolved from the named environment variable at
// composition time and is never stored or emitted.
type KnowledgeEmbeddingConfig struct {
	Enabled                  bool   `yaml:"enabled"`
	ProviderID               string `yaml:"provider_id"`
	BaseURL                  string `yaml:"base_url"`
	APIKeyEnv                string `yaml:"api_key_env"`
	Model                    string `yaml:"model"`
	Dimensions               int    `yaml:"dimensions"`
	MinSimilarityBasisPoints int    `yaml:"min_similarity_basis_points"`
	TimeoutSeconds           int    `yaml:"timeout_seconds"`
}

// ResultHandlesConfig gates new V2 result materialization and workstream links.
// Disabling it leaves existing V2 data readable for recovery and inspection.
type ResultHandlesConfig struct {
	Enabled                    bool                  `yaml:"enabled"`
	MaxProducingCallsPerStep   int                   `yaml:"max_producing_calls_per_step"`
	ProducingCallReserveTokens int                   `yaml:"producing_call_reserve_tokens"`
	Retention                  ResultRetentionConfig `yaml:"retention"`
}

// ResultRetentionConfig carries the TRD 02 per-class retention ages in days.
// A retention worker is not implemented yet (see TRD 02 §Retention); these
// ages currently drive only the offline doctor observability check.
type ResultRetentionConfig struct {
	ContextDays      int `yaml:"context_days"`
	ConversationDays int `yaml:"conversation_days"`
	WorkstreamDays   int `yaml:"workstream_days"`
	ExportedDays     int `yaml:"exported_days"`
}

// ResultAnalysisConfig carries the TRD 07 objective-bound result analysis
// bounds. The gate defaults to disabled. Every bound is a static hard-capped
// value validated in internal/config/validate.go; a non-positive value is
// rejected, never silently defaulted.
type ResultAnalysisConfig struct {
	Enabled               bool                         `yaml:"enabled"`
	MaxSegmentBytes       int                          `yaml:"max_segment_bytes"`
	OverlapBasisPoints    int                          `yaml:"overlap_basis_points"`
	OverlapMaxBytes       int                          `yaml:"overlap_max_bytes"`
	MaxLeaves             int                          `yaml:"max_leaves"`
	MaxReductionFanIn     int                          `yaml:"max_reduction_fan_in"`
	MaxReductionDepth     int                          `yaml:"max_reduction_depth"`
	MaxConcurrentLeaves   int                          `yaml:"max_concurrent_leaves"`
	MaxAttemptsPerStep    int                          `yaml:"max_attempts_per_step"`
	CallTimeoutSeconds    int                          `yaml:"call_timeout_seconds"`
	WallTimeSeconds       int                          `yaml:"wall_time_seconds"`
	WorkerIntervalSeconds int                          `yaml:"worker_interval_seconds"`
	Evidence              ResultAnalysisEvidenceConfig `yaml:"evidence"`
	Model                 ResultAnalysisModelConfig    `yaml:"model"`
}

// ResultAnalysisEvidenceConfig carries the four static bounds from TRD 07
// §Evidence and Downstream Bundle.
type ResultAnalysisEvidenceConfig struct {
	ExcerptBytes        int `yaml:"excerpt_bytes"`
	SelectorsPerLeaf    int `yaml:"selectors_per_leaf"`
	ReferencesPerPacket int `yaml:"references_per_packet"`
	BundleBytes         int `yaml:"bundle_bytes"`
}

// ResultAnalysisModelConfig optionally names a distinct model profile for
// analysis leaf and reduction calls. When Enabled is false, analysis uses
// the main model profile; the fingerprint recorded on the analysis row is
// derived from whichever profile is effective, so the fallback is explicit
// in the identity. There is no default provider, endpoint, or model; the API
// key is resolved from the named environment variable at composition time
// and is never stored or emitted.
type ResultAnalysisModelConfig struct {
	Enabled         bool   `yaml:"enabled"`
	ProviderID      string `yaml:"provider_id"`
	BaseURL         string `yaml:"base_url"`
	APIKeyEnv       string `yaml:"api_key_env"`
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
}

// Default returns a new Config populated with the PRD defaults.
func Default() Config {
	return Config{
		State: StateConfig{
			Dir: DefaultProjectStateDir,
			DB:  DefaultDatabaseFile,
		},
		Context: ContextConfig{
			MaxMessages:                   30,
			MaxChars:                      20_000,
			RetainMessagesPerConversation: 100,
			ModelBudget:                   &ModelBudgetConfig{MaxRequestPercent: 60},
			RecoverableResults: &RecoverableResultsConfig{
				MaxResultBytes:   4 * 1024 * 1024,
				ChunkMaxBytes:    16384,
				RetentionDays:    7,
				CleanupBatchSize: 100,
			},
			ADKCompaction: &ADKCompactionConfig{
				Enabled: true, MaxHistoryChars: 120_000, RecentTurns: 8,
				SummaryEnabled: true, SummaryMaxChars: 8_000, SummaryBudgetTokens: 2_048,
			},
			ContextFeatures: &ContextFeaturesConfig{},
		},
		Runtime: RuntimeConfig{
			LogLevel:                "info",
			ModelTimeoutSeconds:     0,
			SlackAPITimeoutSeconds:  30,
			MaxConcurrentModelCalls: 4,
			ShutdownGraceSeconds:    30,
			BusyMessage:             DefaultBusyMessage,
			ModelErrorMessage:       DefaultModelErrorMessage,
		},
		Slack: SlackConfig{
			AppName:             "Local Agent",
			BotDisplayName:      "Dev Agent",
			UnauthorizedMessage: DefaultUnauthorizedMessage,
			AllowAllUsers:       false,
			AllowedUserIDs:      []string{},
			AllowedTeamIDs:      []string{},
			AllowedChannelIDs:   []string{},
			PartLabels:          true,
			StandardAgent: SlackStandardAgentConfig{SuggestedPrompts: []string{
				"Resume el contexto y destaca las decisiones pendientes.",
				"Analiza el proyecto y señala los riesgos principales.",
				"Prepara un plan de implementación verificable.",
			}, UpdateIntervalSeconds: 3, ProgressLabels: map[domain.ProgressState]string{}},
			Context: SlackContextConfig{
				Enabled:                     false,
				MaxChars:                    1500,
				TimeoutSeconds:              5,
				ProfileCacheTTLMinutes:      60,
				ConversationCacheTTLMinutes: 15,
			},
			Files: SlackFilesConfig{
				MaxBytesPerFile:             5 * 1024 * 1024,
				MaxProcessedChars:           20_000,
				TranscriptionTimeoutSeconds: 120,
			},
		},
		Sandbox:  SandboxConfig{Enabled: true, Projects: map[string]string{"workspace": "."}, CommandTimeoutSeconds: 30, MaxOutputBytes: 64 * 1024},
		Canvases: CanvasesConfig{MaxTitleChars: 150, MaxContentChars: 50000, MaxContentBytes: 5 * 1024 * 1024, TimeoutSeconds: 30},
		Exports:  ExportsConfig{MaxFilenameChars: 128, MaxContentBytes: 1024 * 1024, TimeoutSeconds: 30},
		ACP: ACPConfig{
			MaxInlineResultBytes:     64 * 1024,
			MaxResultArtifactBytes:   16 * 1024 * 1024,
			DefaultJobTimeoutSeconds: 7200, MaxJobTimeoutSeconds: 86400,
			ReconciliationTimeoutSeconds: 1800,
			ProgressWarningSeconds:       900,
			WorkerConcurrency:            1,
			ArtifactRetentionDays:        30,
			Delivery:                     ACPDeliveryConfig{MaxMarkdownParts: 6, MaxFileBytes: 16 * 1024 * 1024},
		},
		CodeIntelligence: &CodeIntelligenceConfig{
			Enabled: false, MaxProcesses: 4, InitTimeoutSeconds: 20, RequestTimeoutSeconds: 10,
		},
		Orchestration: OrchestrationConfig{Workstreams: WorkstreamConfig{
			Enabled: false, MaxNonTerminalTasks: domain.DefaultMaxWorkstreamTasks,
			MaxDependenciesPerTask: domain.DefaultMaxWorkstreamDependencies,
			SnapshotBudgetTokens:   domain.DefaultWorkstreamSnapshotBudgetTokens,
		}, ResultHandles: ResultHandlesConfig{
			Enabled: false, MaxProducingCallsPerStep: 1, ProducingCallReserveTokens: 2_048,
			Retention: ResultRetentionConfig{
				ContextDays: domain.DefaultResultRetentionContextDays, ConversationDays: domain.DefaultResultRetentionConversationDays,
				WorkstreamDays: domain.DefaultResultRetentionWorkstreamDays, ExportedDays: domain.DefaultResultRetentionExportedDays,
			},
		}, Knowledge: KnowledgeConfig{
			Enabled:                   false,
			ProjectionIntervalSeconds: 60,
			ProjectionMaxRetries:      3,
			ProjectionRetentionDays:   90,
			MaxCardTokens:             domain.DefaultMaxKnowledgeCardBudget,
			Retrieval: KnowledgeRetrievalConfig{
				Enabled:                 false,
				TimeoutSeconds:          domain.DefaultKnowledgeRetrievalTimeoutSeconds,
				MaxQueryRunes:           domain.DefaultKnowledgeRetrievalMaxQueryRunes,
				MaxCandidatesPerChannel: domain.DefaultKnowledgeRetrievalMaxCandidatesPerChannel,
				MaxCards:                domain.DefaultKnowledgeRetrievalMaxCards,
				MaxDocumentBytes:        domain.DefaultKnowledgeRetrievalMaxDocumentBytes,
				WorkerIntervalSeconds:   domain.DefaultKnowledgeRetrievalWorkerIntervalSeconds,
				WorkerMaxRetries:        domain.DefaultKnowledgeRetrievalWorkerMaxRetries,
				WorkerBatchSize:         domain.DefaultKnowledgeRetrievalWorkerBatchSize,
				Embedding: KnowledgeEmbeddingConfig{
					Enabled:        false,
					TimeoutSeconds: domain.DefaultKnowledgeEmbeddingTimeoutSeconds,
				},
			},
		}, ResultAnalysis: ResultAnalysisConfig{
			Enabled:               false,
			MaxSegmentBytes:       24576,
			OverlapBasisPoints:    1000,
			OverlapMaxBytes:       4096,
			MaxLeaves:             64,
			MaxReductionFanIn:     8,
			MaxReductionDepth:     4,
			MaxConcurrentLeaves:   2,
			MaxAttemptsPerStep:    2,
			CallTimeoutSeconds:    120,
			WallTimeSeconds:       900,
			WorkerIntervalSeconds: 5,
			Evidence: ResultAnalysisEvidenceConfig{
				ExcerptBytes:        2048,
				SelectorsPerLeaf:    8,
				ReferencesPerPacket: 32,
				BundleBytes:         32768,
			},
			Model: ResultAnalysisModelConfig{Enabled: false},
		}},
	}
}
